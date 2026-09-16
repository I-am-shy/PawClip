package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件是自研的最小 YAML 子集编解码器。
//
// 为什么不用第三方库：docs/BACKUP-FORMAT.md §9（docs/DESIGN.md §9）明确要求
// "不要引入 YAML 解析库"——YAML 只用于导出清单，而清单的结构是我们
// 自己定的、完全受控的一小片。为了这一片引入一个几万行、历史上出过
// 反序列化漏洞的解析器不划算。
//
// **它是子集，不是 YAML 的全集。** 支持：
//   - 块式 mapping 与 sequence，2 空格缩进
//   - 单引号 / 双引号 / 裸标量
//   - 流式 sequence（`[1, 2]`、`[]`，仅标量元素）
//   - null / true / false / 整数 / 浮点
//   - `#` 注释（整行与行尾）
//
// 不支持（遇到就报 manifest_unreadable，不猜）：
//   - 锚点与别名（& / *）、标签（!!）、多文档（---）、折叠/字面块（| / >）
//   - 制表符缩进（YAML 本身也禁止）
//   - 复杂键（`? key`）
//
// 这个取舍是刻意的：读不懂就明确失败，比"猜错了悄悄丢数据"安全得多。

// ── 发射器 ──────────────────────────────────────────────────────

// yamlEmit 把值写成 YAML 标量。
//
// 引号规则保守一点没关系（多引几个引号不影响可读性），但**该引不引会
// 改变语义**：`color: #E24B4A` 里的 `#` 会被当成注释，值直接变成空。
func yamlEmit(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return yamlQuote(x)
	case time.Time:
		if x.IsZero() {
			return "null"
		}
		return yamlQuote(x.Format(time.RFC3339))
	case *time.Time:
		if x == nil {
			return "null"
		}
		return yamlEmit(*x)
	case *string:
		if x == nil {
			return "null"
		}
		return yamlQuote(*x)
	case *int64:
		if x == nil {
			return "null"
		}
		return strconv.FormatInt(*x, 10)
	default:
		// 兜底：交给 JSON 再转字符串，至少不会崩。
		b, err := json.Marshal(v)
		if err != nil {
			return yamlQuote(fmt.Sprint(v))
		}
		return yamlQuote(string(b))
	}
}

// yamlQuote 决定一个字符串该怎么写。
func yamlQuote(s string) string {
	if s == "" {
		return "''"
	}
	// 含换行或控制字符：只能上双引号 + 转义（单引号里换行会被折叠成空格，
	// 那会静默改掉内容——剪贴板文本里换行太常见了）。
	if strings.ContainsAny(s, "\n\r\t") || hasControl(s) {
		return yamlDoubleQuote(s)
	}
	if yamlNeedsQuote(s) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return true
		}
	}
	return false
}

func yamlDoubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlNeedsQuote 判断一个裸标量会不会被误读。
func yamlNeedsQuote(s string) bool {
	if s == "" {
		return true
	}
	// 会被当成注释开头 / 结构字符 / 引号。
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") {
		return true
	}
	// 行首的指示符。
	switch s[0] {
	case '-', '?', ' ':
		return true
	}
	// 行尾空格会被 rstrip 吃掉。
	if strings.HasSuffix(s, " ") {
		return true
	}
	// 看起来像别的类型：不加引号就会变成 bool / null / 数字，
	// 于是 `text: 123` 读回来是数字、`text: true` 读回来是布尔。
	switch strings.ToLower(s) {
	case "null", "~", "true", "false", "yes", "no", "on", "off":
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	return false
}

// yamlIndent 写缩进。
func yamlIndent(w io.Writer, n int) {
	for i := 0; i < n; i++ {
		_, _ = w.Write([]byte("  "))
	}
}

// writeYAMLHeader 写清单头部（不含 items）。
func writeYAMLHeader(w io.Writer, h Header) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "format: %s\n", yamlEmit(h.Format))
	fmt.Fprintf(&b, "formatVersion: %d\n", h.FormatVersion)
	fmt.Fprintf(&b, "appVersion: %s\n", yamlEmit(h.AppVersion))
	fmt.Fprintf(&b, "exportedAt: %s\n", yamlEmit(h.ExportedAt))
	fmt.Fprintf(&b, "platform: %s\n", yamlEmit(h.Platform))
	fmt.Fprintf(&b, "scope: %s\n", yamlEmit(h.Scope))

	b.WriteString("stats:\n")
	yamlIndent(&b, 1)
	fmt.Fprintf(&b, "items: %d\n", h.Stats.Items)
	yamlIndent(&b, 1)
	fmt.Fprintf(&b, "categories: %d\n", h.Stats.Categories)
	yamlIndent(&b, 1)
	fmt.Fprintf(&b, "tags: %d\n", h.Stats.Tags)
	yamlIndent(&b, 1)
	fmt.Fprintf(&b, "blobBytes: %d\n", h.Stats.BlobBytes)

	if len(h.Settings) > 0 {
		b.WriteString("settings:\n")
		if err := writeYAMLValue(&b, 1, h.Settings); err != nil {
			return err
		}
	}

	if len(h.Categories) == 0 {
		// 必须写成一行 `categories: []`。写成 "categories:\n  []\n" 是非法 YAML，
		// 独立一行的 `[]` 不是一个合法的节点续行。
		b.WriteString("categories: []\n")
	} else {
		b.WriteString("categories:\n")
	}
	for _, c := range h.Categories {
		yamlIndent(&b, 1)
		fmt.Fprintf(&b, "- id: %d\n", c.ID)
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "name: %s\n", yamlEmit(c.Name))
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "color: %s\n", yamlEmit(c.Color))
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "icon: %s\n", yamlEmit(c.Icon))
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "sortOrder: %d\n", c.SortOrder)
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "ttlSeconds: %s\n", yamlEmit(c.TTLSeconds))
		yamlIndent(&b, 2)
		if block, ok := ruleAsYAMLBlock(c.Rule); ok {
			b.WriteString("rule:\n")
			if err := writeYAMLValue(&b, 3, block); err != nil {
				return err
			}
		} else {
			// 空规则、或库里存的不是合法 JSON 时退成 null。
			// 不原样把坏 JSON 塞进去——那会产出一个没人能解析的包。
			b.WriteString("rule: null\n")
		}
	}

	if len(h.Tags) == 0 {
		b.WriteString("tags: []\n") // 同上：空集合必须写在一行里
	} else {
		b.WriteString("tags:\n")
	}
	for _, tg := range h.Tags {
		yamlIndent(&b, 1)
		fmt.Fprintf(&b, "- id: %d\n", tg.ID)
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "name: %s\n", yamlEmit(tg.Name))
		yamlIndent(&b, 2)
		fmt.Fprintf(&b, "color: %s\n", yamlEmit(tg.Color))
	}

	b.WriteString("items:\n")
	_, err := w.Write(b.Bytes())
	return err
}

// ruleAsYAMLBlock 把分类的自动归类规则（库里存的是 JSON 文本）
// 转成可写进 YAML 的嵌套结构。
//
// §3.2 的示例里 rule 就是一个嵌套块：
//
//	rule:
//	  match: any
//	  conditions:
//	    - field: sourceAppId
//	      op: startsWith
//	      value: com.apple.Safari
//
// 所以不能把 JSON 原文直接当标量写进去——那样开头是 `{`，解析侧会
// （正确地）判定为不受支持的流式 mapping，整包读不出来。
func ruleAsYAMLBlock(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	if v == nil {
		return nil, false
	}
	return v, true
}

// writeYAMLValue 写任意嵌套值（只用于 settings 那一块）。
func writeYAMLValue(w io.Writer, depth int, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// 排序让输出稳定——同一份数据两次导出的清单应当逐字节相同，
		// 否则用户没法用 git diff 看"这次备份改了什么"。
		sort.Strings(keys)
		for _, k := range keys {
			yamlIndent(w, depth)
			vv := x[k]
			if isYAMLBlock(vv) {
				fmt.Fprintf(w, "%s:\n", k)
				if err := writeYAMLValue(w, depth+1, vv); err != nil {
					return err
				}
				continue
			}
			fmt.Fprintf(w, "%s: %s\n", k, yamlFlow(vv))
		}
	case []any:
		for _, e := range x {
			// 元素是 mapping 时必须写成块式：
			//
			//	- field: sourceAppId
			//	  op: startsWith
			//	  value: com.apple.Safari
			//
			// 用 yamlFlow 会输出 `- {}`，把整个对象**静默丢掉**——
			// 分类的自动归类规则就是这么没的（条件全变空对象）。
			if m, ok := e.(map[string]any); ok && len(m) > 0 {
				if err := writeYAMLMapAsSeqItem(w, m, depth); err != nil {
					return err
				}
				continue
			}
			if isYAMLBlock(e) {
				yamlIndent(w, depth)
				_, _ = w.Write([]byte("-\n"))
				if err := writeYAMLValue(w, depth+1, e); err != nil {
					return err
				}
				continue
			}
			yamlIndent(w, depth)
			fmt.Fprintf(w, "- %s\n", yamlFlow(e))
		}
	case nil:
		yamlIndent(w, depth)
		_, _ = w.Write([]byte("null\n"))
	default:
		yamlIndent(w, depth)
		fmt.Fprintf(w, "%s\n", yamlFlow(x))
	}
	return nil
}

// writeYAMLMapAsSeqItem 把一个 mapping 写成序列项（`- ` 开头，后续键对齐）。
func writeYAMLMapAsSeqItem(w io.Writer, m map[string]any, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i == 0 {
			yamlIndent(w, depth)
			_, _ = w.Write([]byte("- "))
		} else {
			// 后续键与 `- ` 之后的第一个键**对齐**，也就是深一层。
			yamlIndent(w, depth+1)
		}
		vv := m[k]
		if isYAMLBlock(vv) {
			fmt.Fprintf(w, "%s:\n", k)
			if err := writeYAMLValue(w, depth+2, vv); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(w, "%s: %s\n", k, yamlFlow(vv))
	}
	return nil
}

// isYAMLBlock 判断一个值该用块式还是流式写。
func isYAMLBlock(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) > 0
	case []any:
		// 标量数组用流式更紧凑（`types: [text, image]`），
		// 含嵌套结构的数组只能用块式。
		for _, e := range x {
			if _, ok := e.(map[string]any); ok {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// yamlFlow 把标量或标量数组写成一行。
func yamlFlow(v any) string {
	switch x := v.(type) {
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, yamlEmit(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// 空 map。
		return "{}"
	default:
		return yamlEmit(x)
	}
}

// writeYAMLItem 写一条 item。seqIndent 是 `- ` 那行的缩进层级。
//
// 手写而不是反射：item 是**流式**写的（§7），每写一条就落盘一条，
// 反射版还得先构造中间结构，反而更绕。
func writeYAMLItem(w io.Writer, it Item, seqIndent int) error {
	var b bytes.Buffer
	yamlIndent(&b, seqIndent)
	fmt.Fprintf(&b, "- id: %d\n", it.ID)

	kv := func(k string, v any) {
		yamlIndent(&b, seqIndent+1)
		fmt.Fprintf(&b, "%s: %s\n", k, yamlEmit(v))
	}
	kv("kind", it.Kind)
	kv("preview", it.Preview)
	kv("fingerprint", it.Fingerprint)
	kv("byteSize", it.ByteSize)
	kv("text", it.Text)
	kv("html", it.HTML)
	kv("rtfPath", it.RTFPath)
	if len(it.FilePaths) == 0 {
		kv("filePaths", nil)
	} else {
		yamlIndent(&b, seqIndent+1)
		parts := make([]string, 0, len(it.FilePaths))
		for _, p := range it.FilePaths {
			parts = append(parts, yamlEmit(p))
		}
		fmt.Fprintf(&b, "filePaths: [%s]\n", strings.Join(parts, ", "))
	}
	kv("imageWidth", it.ImageWidth)
	kv("imageHeight", it.ImageHeight)
	kv("sourceAppId", it.SourceAppID)
	kv("sourceAppName", it.SourceAppName)
	kv("sourceUrl", it.SourceURL)
	kv("categoryId", it.CategoryID)
	if len(it.TagIDs) == 0 {
		kv("tagIds", nil)
	} else {
		nums := make([]string, 0, len(it.TagIDs))
		for _, id := range it.TagIDs {
			nums = append(nums, strconv.FormatInt(id, 10))
		}
		yamlIndent(&b, seqIndent+1)
		fmt.Fprintf(&b, "tagIds: [%s]\n", strings.Join(nums, ", "))
	}
	kv("pinned", it.Pinned)
	kv("ttlSeconds", it.TTLSeconds)
	kv("expiresAt", it.ExpiresAt)
	kv("firstSeenAt", it.FirstSeenAt)
	kv("createdAt", it.CreatedAt)
	kv("lastUsedAt", it.LastUsedAt)
	kv("useCount", it.UseCount)

	if len(it.Blobs) == 0 {
		yamlIndent(&b, seqIndent+1)
		b.WriteString("blobs: []\n")
	} else {
		yamlIndent(&b, seqIndent+1)
		b.WriteString("blobs:\n")
		for _, bl := range it.Blobs {
			yamlIndent(&b, seqIndent+2)
			fmt.Fprintf(&b, "- role: %s\n", yamlEmit(bl.Role))
			yamlIndent(&b, seqIndent+3)
			fmt.Fprintf(&b, "path: %s\n", yamlEmit(bl.Path))
			yamlIndent(&b, seqIndent+3)
			fmt.Fprintf(&b, "mime: %s\n", yamlEmit(bl.Mime))
			yamlIndent(&b, seqIndent+3)
			fmt.Fprintf(&b, "bytes: %d\n", bl.Bytes)
			yamlIndent(&b, seqIndent+3)
			fmt.Fprintf(&b, "sha256: %s\n", yamlEmit(bl.SHA256))
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// ── 解析器 ──────────────────────────────────────────────────────

type yamlLine struct {
	indent int
	text   string
	num    int // 1-based，报错时告诉用户第几行
}

// parseYAMLSubset 把 YAML 子集解析成通用 Go 值
// （map[string]any / []any / string / float64 / bool / nil）。
//
// 返回的形态与 encoding/json 一致，所以调用方可以直接
// json.Marshal 一遍再 Unmarshal 进目标结构体——这样 YAML 与 JSON
// 两条路径共用同一套结构体标签，不会出现"JSON 能读 YAML 读不出"的偏差。
func parseYAMLSubset(data []byte) (any, error) {
	lines, err := tokenizeYAML(data)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("backup: YAML 文档为空")
	}
	v, next, err := parseYAMLNode(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next != len(lines) {
		return nil, fmt.Errorf("backup: YAML 第 %d 行起有多余内容（缩进错乱？）", lines[next].num)
	}
	return v, nil
}

func tokenizeYAML(data []byte) ([]yamlLine, error) {
	raw := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	out := make([]yamlLine, 0, len(raw))
	for i, ln := range raw {
		if strings.ContainsRune(ln, '\t') {
			// YAML 明确禁止用制表符缩进。放过去的话 indent 会算错，
			// 后面报一个风马牛不相及的错。
			return nil, fmt.Errorf("backup: YAML 第 %d 行含制表符（YAML 不允许用 tab 缩进）", i+1)
		}
		trimmed := strings.TrimRight(ln, " ")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		body := strings.TrimLeft(trimmed, " ")
		if strings.HasPrefix(body, "#") {
			continue
		}
		if strings.HasPrefix(body, "---") || strings.HasPrefix(body, "...") {
			return nil, fmt.Errorf("backup: YAML 第 %d 行的多文档标记不支持", i+1)
		}
		indent := len(trimmed) - len(body)
		if indent%2 != 0 {
			return nil, fmt.Errorf("backup: YAML 第 %d 行缩进为 %d，不是 2 的倍数", i+1, indent)
		}
		text, err := stripYAMLComment(body)
		if err != nil {
			return nil, fmt.Errorf("backup: YAML 第 %d 行：%w", i+1, err)
		}
		text = strings.TrimRight(text, " ")
		if text == "" {
			continue
		}
		out = append(out, yamlLine{indent: indent / 2, text: text, num: i + 1})
	}
	return out, nil
}

// stripYAMLComment 去掉行尾注释（引号内的 `#` 不算）。
func stripYAMLComment(s string) (string, error) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote != 0 && c == quote:
			// 单引号里两个连续单引号是转义，不算结束。
			if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			quote = 0
		case quote == 0 && c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i], nil
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("引号没有闭合")
	}
	return s, nil
}

func parseYAMLNode(lines []yamlLine, i, indent int) (any, int, error) {
	if i >= len(lines) {
		return nil, i, nil
	}
	if isYAMLSeqLine(lines[i].text) {
		return parseYAMLSeq(lines, i, indent)
	}
	return parseYAMLMap(lines, i, indent)
}

func isYAMLSeqLine(s string) bool {
	return s == "-" || strings.HasPrefix(s, "- ")
}

func parseYAMLMap(lines []yamlLine, i, indent int) (any, int, error) {
	m := map[string]any{}
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("backup: YAML 第 %d 行缩进比上下文深", ln.num)
		}
		if isYAMLSeqLine(ln.text) {
			break
		}
		ni, err := assignYAMLKey(lines, m, i, indent)
		if err != nil {
			return nil, i, err
		}
		if ni <= i {
			return nil, i, fmt.Errorf("backup: YAML 第 %d 行解析没有前进（缩进错乱）", ln.num)
		}
		i = ni
	}
	return m, i, nil
}

func parseYAMLSeq(lines []yamlLine, i, indent int) (any, int, error) {
	out := []any{}
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent || !isYAMLSeqLine(ln.text) {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("backup: YAML 第 %d 行缩进比同级序列项深", ln.num)
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(ln.text, "-"), " ")

		if rest == "" {
			// `-` 单独一行，值在下一行。
			i++
			if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseYAMLNode(lines, i, lines[i].indent)
				if err != nil {
					return nil, i, err
				}
				out = append(out, v)
				i = ni
			} else {
				out = append(out, nil)
			}
			continue
		}

		// `- key: value`：序列项是一个 mapping，它的第一个键内联在这里，
		// 键的对齐列是 indent+1（因为 "- " 占两列 = 一层缩进）。
		if _, _, ok := splitYAMLKey(rest); ok {
			m := map[string]any{}
			// "- " 占两层字符 = 一层缩进，所以这个 mapping 的键对齐在 indent+1。
			keyIndent := indent + 1
			next, err := assignYAMLKeyText(lines, m, rest, ln.num, i, keyIndent)
			if err != nil {
				return nil, i, err
			}
			i = next
			for i < len(lines) && lines[i].indent == keyIndent && !isYAMLSeqLine(lines[i].text) {
				if _, _, ok := splitYAMLKey(lines[i].text); !ok {
					return nil, i, fmt.Errorf("backup: YAML 第 %d 行不是 `key: value` 形式", lines[i].num)
				}
				next, err = assignYAMLKey(lines, m, i, keyIndent)
				if err != nil {
					return nil, i, err
				}
				i = next
			}
			out = append(out, m)
			continue
		}

		// 纯标量项。
		sv, err := parseYAMLScalar(rest)
		if err != nil {
			return nil, i, fmt.Errorf("backup: YAML 第 %d 行：%w", ln.num, err)
		}
		out = append(out, sv)
		i++
	}
	return out, i, nil
}

// assignYAMLKey 处理第 i 行（一个 `key: ...`），把结果写进 m，返回下一行下标。
//
// keyIndent 是**该键自身的缩进层级**：值为空时要靠它判断"下一行是不是
// 这个键的嵌套块"（下一行 indent 必须 > keyIndent）。
//
// 返回值必须由调用方用起来：值为块时它指向块之后的第一行，
// 忽略了就会把子块当成同级键重复解析。
func assignYAMLKey(lines []yamlLine, m map[string]any, i, keyIndent int) (int, error) {
	ln := lines[i]
	next, err := assignYAMLKeyText(lines, m, ln.text, ln.num, i, keyIndent)
	return next, err
}

// assignYAMLKeyText 是 assignYAMLKey 的核心：text 与 lines[i] 分离，
// 是为了支持 `- key: value` 这种"序列项里内联了第一个键"的写法——
// 那种情况下键值文本已经去掉了 "- " 前缀，不能再从 lines[i] 里取。
func assignYAMLKeyText(lines []yamlLine, m map[string]any, text string, lineNum, i, keyIndent int) (int, error) {
	k, v, ok := splitYAMLKey(text)
	if !ok {
		return i, fmt.Errorf("backup: YAML 第 %d 行不是 `key: value` 形式", lineNum)
	}
	if strings.TrimSpace(v) != "" {
		sv, err := parseYAMLScalar(v)
		if err != nil {
			return i, fmt.Errorf("backup: YAML 第 %d 行：%w", lineNum, err)
		}
		m[k] = sv
		return i + 1, nil
	}
	ni := i + 1
	if ni < len(lines) && lines[ni].indent > keyIndent {
		sv, nn, err := parseYAMLNode(lines, ni, lines[ni].indent)
		if err != nil {
			return i, err
		}
		m[k] = sv
		return nn, nil
	}
	m[k] = nil
	return ni, nil
}

// splitYAMLKey 把一行拆成 key 与 raw value。
//
// 必须逐字符扫描并跟踪引号状态：`exportedAt: '2026-09-15T11:41:58+08:00'`
// 里的冒号在引号内，直接 Index 会把它当成键值分隔符。
func splitYAMLKey(s string) (key, value string, ok bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ':':
			// 必须是 `:` 后跟空格或行尾，否则可能是 `a:b` 这类裸值。
			if i+1 >= len(s) || s[i+1] == ' ' {
				k := strings.TrimSpace(s[:i])
				if k == "" {
					return "", "", false
				}
				return k, strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseYAMLScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	// 流式序列：[1, 2] / [] / [a, b]
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("流式序列没有闭合的 ]")
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}, nil
		}
		parts, err := splitFlow(inner)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			v, err := parseYAMLScalar(p)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if strings.HasPrefix(s, "{") {
		// 空 map 是我们唯一会写出的流式 mapping。
		if s == "{}" {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("流式 mapping 不支持（只支持 {}）")
	}

	// 引号字符串。
	if strings.HasPrefix(s, "'") {
		if !strings.HasSuffix(s, "'") || len(s) < 2 {
			return nil, fmt.Errorf("单引号字符串没有闭合")
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if strings.HasPrefix(s, "\"") {
		return unquoteYAMLDouble(s)
	}

	switch strings.ToLower(s) {
	case "null", "~":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	// 数字。用 float64 与 encoding/json 保持一致，后续由结构体标签
	// 决定转成什么具体类型。
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, nil
	}
	// 锚点与别名（& / *）、自定义标签（!）、块标量（| / >）。
	// 这些我们既写不出来也读不对，**必须明确失败**：当普通字符串吞下去
	// 会让值静默变成 "&x 1" 这种东西，用户看到的是一个坏掉的数据。
	switch s[0] {
	case '&', '*', '!', '|', '>':
		return nil, fmt.Errorf("不支持的 YAML 构造（锚点/别名/标签/块标量）：%q", s)
	}

	// 其余按裸字符串。
	return s, nil
}

// unquoteYAMLDouble 解析双引号字符串的转义。
func unquoteYAMLDouble(s string) (any, error) {
	if len(s) < 2 || !strings.HasSuffix(s, "\"") {
		return nil, fmt.Errorf("双引号字符串没有闭合")
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return nil, fmt.Errorf("字符串以孤立的反斜杠结尾")
		}
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '0':
			b.WriteByte(0)
		case 'u':
			if i+4 >= len(body) {
				return nil, fmt.Errorf("\\u 转义不完整")
			}
			n, err := strconv.ParseUint(body[i+1:i+5], 16, 32)
			if err != nil {
				return nil, fmt.Errorf("\\u 转义非法：%v", err)
			}
			b.WriteRune(rune(n))
			i += 4
		default:
			return nil, fmt.Errorf("不支持的转义：\\%c", body[i])
		}
	}
	return b.String(), nil
}

// splitFlow 按逗号切分流式序列，尊重引号。
func splitFlow(s string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
					cur.WriteByte(s[i+1])
					i++
					continue
				}
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ',':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("流式序列里的引号没有闭合")
	}
	out = append(out, cur.String())
	return out, nil
}
