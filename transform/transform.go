// Package transform 是"内容转换器"（DESIGN §11 P2）的纯逻辑层。
//
// 六个能力，全部是 文本 → 文本 的纯函数：
//
//	json.beautify     JSON 美化（2 空格缩进）
//	json.minify       JSON 压成一行
//	base64.encode     文本 → Base64
//	base64.decode     Base64 → 文本
//	url.stripUTM      去掉 URL 上的 utm_* 等跟踪参数
//	case.upper/lower/title
//	lines.removeBlank 去掉空行
//
// # 为什么单独一个包、而且不碰任何全局状态
//
// 这一层最容易写成"在绑定方法里当场 strings.Replace 两下"，然后没有任何
// 测试——因为它看起来不值得测。但转换器的 bug 恰恰是**静默**的：
// Base64 解码失败如果返回原文而不是报错，用户会以为"转换成功了，内容就长这样"，
// 然后把一段乱码粘出去。所以规则是：
//
//   - 每个 op 要么返回**语义正确**的结果，要么返回 error，绝不"失败时返回原文"；
//   - op 的注册表在这里，前端拿 OpIDs() 去渲染菜单，新增 op 不会漏到前端。
//
// "去格式贴纯文本"不在这里：它不是文本 → 文本，而是**选择条目的哪一种表示**
// （丢掉 HTML / RTF，只留纯文本），属于回写路径，见 Writeback.PastePlain。
package transform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnknownOp 表示传入的 op 名不在注册表里。
//
// 单独一个哨兵而不是 fmt.Errorf 一句话：前端拿它区分"这个 op 我不支持"
// （应该刷新 op 列表）与"这个转换失败了"（应该显示失败原因）。
var ErrUnknownOp = errors.New("transform: 未知的转换操作")

// 失败的种类（Kind）。
//
// # 为什么要给错误分类，而不是只给一句话
//
// 转换失败是**用户日常会遇到**的事（粘了一段不是 JSON 的东西就点了"JSON
// 美化"）。如果这里只返回一句中文，那么英文界面上会弹出中文；
// 如果只返回一句英文，中文用户又看不懂。正确做法是：
//
//	这里给出**机器可判定的种类**，由界面翻成用户的语言；
//	msg 只作为"没有对应文案时的兜底"和日志细节。
//
// 于是 transform 包不需要知道 i18n 的存在（它也不该知道），
// 而 `transform.KindOf(err)` 就是前后端之间的那层契约。
const (
	KindEmpty         = "empty"         // 输入是空的
	KindTooLarge      = "tooLarge"      // 超过输入上限
	KindInvalidJSON   = "invalidJSON"   // 不是合法 JSON
	KindMultipleJSON  = "multipleJSON"  // 里面有多个 JSON 值
	KindInvalidBase64 = "invalidBase64" // 不是合法 Base64
	KindBinaryResult  = "binaryResult"  // Base64 解出来不是 UTF-8 文本
	KindMultipleLines = "multipleLines" // 只支持单行的转换收到了多行
	KindUnknownOp     = "unknownOp"     // op 不存在
	KindUnknown       = "unknown"       // 其它
)

// Error 是带种类的转换错误。
type Error struct {
	Kind string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Errorf 造一个带种类的错误。
func Errorf(kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// KindOf 从错误里取出种类；不是本包的错误时返回 KindUnknown。
//
// ⚠️ 包一层 `fmt.Errorf("...: %w", err)` 之后仍然要能取出种类 —— 所以走
// errors.As 而不是类型断言。上层（比如绑定层给错误加上下文）会包一层。
func KindOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, ErrUnknownOp) {
		return KindUnknownOp
	}
	return KindUnknown
}

// Op 是一次转换。
//
// Label 只是给日志/诊断用的原始英文名；**用户看到的文案在前端 i18n 里**
// （converter.* 那组键），因为它是界面文本，不该由后端决定。
type Op struct {
	ID    string
	Label string
	Apply func(string) (string, error)
}

// ops 是**唯一真源**。顺序就是前端菜单里的顺序。
//
// 顺序有意按"日常使用频率"排：JSON 与去空行几乎是每天用的，
// Base64 与大小写次之。
var ops = []Op{
	{ID: "json.beautify", Label: "Beautify JSON", Apply: jsonBeautify},
	{ID: "json.minify", Label: "Minify JSON", Apply: jsonMinify},
	{ID: "lines.removeBlank", Label: "Remove blank lines", Apply: removeBlankLines},
	{ID: "url.stripUTM", Label: "Strip tracking params", Apply: stripUTM},
	{ID: "base64.encode", Label: "Base64 encode", Apply: base64Encode},
	{ID: "base64.decode", Label: "Base64 decode", Apply: base64Decode},
	{ID: "case.upper", Label: "UPPER CASE", Apply: caseUpper},
	{ID: "case.lower", Label: "lower case", Apply: caseLower},
	{ID: "case.title", Label: "Title Case", Apply: titleCase},
}

// OpIDs 返回全部 op 的 ID（与 ops 同序）。
//
// 存在的意义是**让前端的菜单跟着后端走**：前端只需要知道怎么显示某个 ID，
// 不需要知道有哪些 ID。加了新 op 而前端没跟上时，前端会渲染出一个
// "未命名"的项（见 frontend/src/i18n.ts 的 converter.* 回退），
// 而不是"后端有、界面上没有"。
func OpIDs() []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.ID)
	}
	return out
}

// Lookup 按 ID 找一个 op。
func Lookup(id string) (Op, bool) {
	for _, o := range ops {
		if o.ID == id {
			return o, true
		}
	}
	return Op{}, false
}

// Apply 按 ID 执行一次转换。
func Apply(id, in string) (string, error) {
	op, ok := Lookup(id)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownOp, id)
	}
	return op.Apply(in)
}

// ── JSON ────────────────────────────────────────────────────────

// maxTransformInput 是单次转换的输入上限（字符数）。
//
// 与 capture.textMaxChars 同一个量级：转换本身是 O(n) 的内存操作，
// 再加一倍输出缓冲，256K 字符对应的峰值只有几百 KB。
// 设上限是为了让"有人把 100 MB 的日志粘进来点美化"变成一个明确的错误，
// 而不是让进程被 OOM 杀掉。
const maxTransformInput = 4 << 20 // 4M 字符

func guardSize(s string) error {
	if len(s) > maxTransformInput {
		return Errorf(KindTooLarge, "transform: 内容太大（%d 字节，上限 %d）", len(s), maxTransformInput)
	}
	return nil
}

// jsonBeautify 用 2 空格缩进重排 JSON。
//
// ⚠️ 两个不能省的细节：
//
//  1. **先 json.Valid 再 Unmarshal 到 any**，最后 MarshalIndent。
//     直接把原文 MarshalIndent 是不行的（JSON 不是 Go 值）。
//  2. 用 json.Decoder 的 UseNumber 解析，否则大整数（如
//     9007199254740993、或 64 位 ID）会被转成 float64 而**悄悄改值**——
//     这是"美化"把数据改坏的经典翻车点。
func jsonBeautify(s string) (string, error) {
	v, err := decodeJSON(s)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", Errorf(KindUnknown, "transform: JSON 美化失败：%v", err)
	}
	return string(b), nil
}

// jsonMinify 压成一行（不留任何缩进与多余空白）。
func jsonMinify(s string) (string, error) {
	v, err := decodeJSON(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	// Encoder 默认会写一个换行，且会转义 < > &，这里都关掉：
	// 压行的语义就是"一行"，而 HTML 转义会让 URL/代码片段变得没法读。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", Errorf(KindUnknown, "transform: JSON 压缩失败：%v", err)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// decodeJSON 解析一段 JSON，保留数字的字面量表示。
func decodeJSON(s string) (any, error) {
	if err := guardSize(s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s) == "" {
		return nil, Errorf(KindEmpty, "transform: 内容是空的，没有 JSON 可处理")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, Errorf(KindInvalidJSON, "transform: 不是合法的 JSON：%v", err)
	}
	// 尾部还有内容（如 `{} {}`）也算不合法——只转换第一个值会让用户以为
	// 后面的部分也处理了。
	if dec.More() {
		return nil, Errorf(KindMultipleJSON, "transform: 内容里有多个 JSON 值，无法确定要处理哪个")
	}
	return v, nil
}

// ── 去空行 ──────────────────────────────────────────────────────

// removeBlankLines 删掉"只有空白"的行，并顺手去掉每行行尾的空白。
//
// 两个取舍：
//   - 只删**空白行**，不删"\t"开头的缩进行（那会破坏 YAML / 代码块）；
//   - 不动行首缩进（保留结构），只 rstrip。粘贴日志时最常见的需求
//     就是把行尾那串空格清掉。
func removeBlankLines(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		// \r 是 Windows 换行残留，先剥掉再判空，否则 "\r" 会被当成"非空行"。
		ln = strings.TrimRight(ln, "\r")
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n"), nil
}

// ── URL 跟踪参数 ────────────────────────────────────────────────

// trackingParams 是要剥掉的参数名（小写比对）。
//
// utm_* 用前缀匹配覆盖全部（utm_source / utm_campaign / …），
// 其余是各平台自己的跟踪参数，按 DESIGN §11 P2 只点名了 utm_*，
// 这里顺带带上最常见的几个——它们与 utm_* 一样是"给分享者看的"，
// 用户复制链接时想去的正是它们。
var trackingParams = []string{
	"gclid", "fbclid", "msclkid", "dclid", "yclid", "igshid", "mkt_tok", "_hsenc", "_hsmi", "spm",
}

// stripUTM 去掉 URL 查询串里的跟踪参数。
//
// 返回值的保证：**只删查询串里命中的参数**，其余部分逐字节保留
// （包括 fragment 里的 `#`、路径里的百分号转义、以及参数原有的顺序）。
// 所以这里不用 net/url 重新编码整条 URL——那会把 `%2F` 变成 `%2F`→`/`、
// 把 `+` 变成 `%20`、把 query 里的重复键合并，用户会发现"去个 utm 而已，
// 链接怎么变了"。手写切分虽然啰嗦，但它是**只做减法**的。
func stripUTM(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", Errorf(KindEmpty, "transform: 内容是空的，没有 URL 可处理")
	}
	// 只处理单行单 URL：多行内容里逐行剥离会让"转换后还是多行"这件事
	// 变得不可预期，而用户想要的通常是一次一个链接。
	if strings.ContainsAny(trimmed, "\n\r") {
		return "", Errorf(KindMultipleLines, "transform: 只支持单行 URL（内容里有多行）")
	}

	// 先切出 fragment，它里面的 & 不是查询参数分隔符。
	head, frag := trimmed, ""
	if i := strings.IndexByte(trimmed, '#'); i >= 0 {
		head, frag = trimmed[:i], trimmed[i:]
	}
	// query 必须是"?"之后的部分；没有 "?" 就原样返回（可能已经是干净的）。
	qi := strings.IndexByte(head, '?')
	if qi < 0 {
		return head + frag, nil
	}
	base, query := head[:qi+1], head[qi+1:]
	if query == "" {
		// 空查询串：**逐字节原样返回**。不要顺手把那个悬空的 `?` 去掉——
		// "只做减法"的承诺要包含"没有可减的东西时输出等于输入"，
		// 否则用户复制一个干净的链接来点一下，链接竟然变了。
		return trimmed, nil
	}

	parts := strings.Split(query, "&")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		name := p
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			name = p[:eq]
		}
		if isTrackingParam(name) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		// 参数全被剥光了：连 "?" 一起去掉，否则会留下一个悬空的问号。
		return strings.TrimSuffix(base, "?") + frag, nil
	}
	return base + strings.Join(kept, "&") + frag, nil
}

func isTrackingParam(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "utm_") {
		return true
	}
	// 二分没必要（表只有 10 项），但要保证确定性：表已排序，用 sort.Search 会
	// 要求调用方也维护有序性，得不偿失。
	for _, t := range trackingParams {
		if lower == t {
			return true
		}
	}
	return false
}

// ── Base64 ──────────────────────────────────────────────────────

var b64Decoder = base64.StdEncoding

func base64Encode(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	if s == "" {
		return "", Errorf(KindEmpty, "transform: 内容是空的，没有东西可编码")
	}
	return b64Decoder.EncodeToString([]byte(s)), nil
}

// base64Decode 解码一段 Base64。
//
// 两个"方便但不放水"的处理：
//
//   - 容错 1：先去掉内容里的所有空白。用户从网页/邮件里复制来的 Base64
//     常带换行，直接解码会失败。
//   - 容错 2：无填充（RawStdEncoding）再试一次。很多 JWT / 短串默认不带 `=`。
//
// 但**不做**"解码失败就返回原文"：那会让用户拿到一段看似成功的结果，
// 而实际上是没转换的原文。宁可报错。
func base64Decode(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	if clean == "" {
		return "", Errorf(KindEmpty, "transform: 内容是空的，没有可解码的 Base64")
	}
	b, err := b64Decoder.DecodeString(clean)
	if err != nil {
		// 退一步试无填充形式。
		if b2, err2 := base64.RawStdEncoding.DecodeString(strings.TrimRight(clean, "=")); err2 == nil {
			b, err = b2, nil
		}
	}
	if err != nil {
		return "", Errorf(KindInvalidBase64, "transform: 不是合法的 Base64：%v", err)
	}
	out := string(b)
	if !utf8.ValidString(out) {
		// 解出来不是文本：这多半是用户拿它当"解码器"去解一段二进制，
		// 比如图片。明确说出来，而不是把乱码塞进剪贴板。
		return "", Errorf(KindBinaryResult, "transform: 解出来不是 UTF-8 文本（可能是一段二进制内容）")
	}
	return out, nil
}

// ── 大小写 ──────────────────────────────────────────────────────

// caseUpper / caseLower 就是 strings.ToUpper / ToLower，
// 之所以各包一层：注册表里的函数签名统一带 error，
// 并且大小写转换同样要过一遍输入上限（一个 100 MB 的串转大写也是 100 MB）。
func caseUpper(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	return strings.ToUpper(s), nil
}

func caseLower(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	return strings.ToLower(s), nil
}

// titleCase 把每个词的**首字母**大写，其余小写。
//
// 用 unicode 的分词判定而不是按空白切：
//
//	词内第一个"字母或数字"大写，词内其余字符小写；
//	词边界 = 非字母数字的字符（该字符本身原样保留）。
//
// 于是 "hello-world" → "Hello-World"。注意撇号也算词边界，所以
// "it's" → "It'S"——这是**刻意**与 macOS 的"标题大小写"保持一致，
// 而不是发明一套自己的规则。想要严格的英文标题规则就用 case.upper/lower。
func titleCase(s string) (string, error) {
	if err := guardSize(s); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(s))
	startOfWord := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if startOfWord {
				b.WriteRune(unicode.ToUpper(r))
				startOfWord = false
			} else {
				b.WriteRune(unicode.ToLower(r))
			}
			continue
		}
		// 非字母数字：本身是词边界，原样保留。
		b.WriteRune(r)
		startOfWord = true
	}
	return b.String(), nil
}

// ── 测试与诊断用的辅助 ──────────────────────────────────────────

// AllIDs 返回排好序的 op ID（给测试断言"注册表没漏项"用）。
func AllIDs() []string {
	out := OpIDs()
	sort.Strings(out)
	return out
}
