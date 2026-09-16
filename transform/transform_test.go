package transform

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestRegistry_IsWellFormed 钉住注册表本身的健全性。
//
// 防的是"加了一个 op 但 ID 拼错/重名/Label 空着"这类只在运行时才暴露的问题：
// 前端拿 OpIDs() 渲染菜单，重名会让两个菜单项调同一个转换。
func TestRegistry_IsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	if len(ops) == 0 {
		t.Fatal("注册表是空的")
	}
	for _, o := range ops {
		if o.ID == "" || o.Label == "" {
			t.Errorf("op %+v 缺少 ID 或 Label", o)
		}
		if o.Apply == nil {
			t.Errorf("op %q 没有 Apply", o.ID)
		}
		if seen[o.ID] {
			t.Errorf("op ID %q 重复了", o.ID)
		}
		seen[o.ID] = true
	}
	if got, want := len(OpIDs()), len(ops); got != want {
		t.Errorf("OpIDs() 返回 %d 项，注册表有 %d 项", got, want)
	}
	// 每个 op 都必须能被 Lookup 找到（防"清单与查找表不同步"）。
	for _, id := range OpIDs() {
		if _, ok := Lookup(id); !ok {
			t.Errorf("OpIDs() 里有 %q，但 Lookup 找不到", id)
		}
		if _, err := Apply(id, "x"); err != nil && errors.Is(err, ErrUnknownOp) {
			t.Errorf("Apply(%q) 报了 ErrUnknownOp —— 注册表与查找表不同步", id)
		}
	}
	if _, err := Apply("nope", "x"); !errors.Is(err, ErrUnknownOp) {
		t.Errorf("Apply 一个不存在的 op 返回 %v，想要 ErrUnknownOp", err)
	}
}

// TestJSONBeautify_PreservesBigNumbers 是本文件里最要紧的一条。
//
// 用 encoding/json 把 JSON 解成 any 再 Marshal 时，数字**默认**变成
// float64 —— 9007199254740993（2^53+1）会被静默改成 9007199254740992。
// 也就是说"美化"会把用户的数据改坏，而且看起来完全正常。
// 这条用例就是用那个具体的数字把它钉住。
func TestJSONBeautify_PreservesBigNumbers(t *testing.T) {
	in := `{"id":9007199254740993,"tiny":1e400,"neg":-9223372036854775808}`
	out, err := Apply("json.beautify", in)
	if err != nil {
		t.Fatalf("json.beautify: %v", err)
	}
	for _, want := range []string{"9007199254740993", "-9223372036854775808"} {
		if !strings.Contains(out, want) {
			t.Errorf("美化后丢了 %s：\n%s", want, out)
		}
	}
	// 1e400 超出 float64 范围，一旦走 float64 会变成 +Inf 或报错。
	if strings.Contains(out, "Inf") || !strings.Contains(out, "1e400") {
		t.Errorf("超大指数被改了：\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("美化后还是一行：%q", out)
	}
}

func TestJSON_OursAndErrors(t *testing.T) {
	pretty, err := Apply("json.beautify", `{"a":1,"b":[1,2]}`)
	if err != nil {
		t.Fatalf("beautify: %v", err)
	}
	if !strings.Contains(pretty, "  \"a\": 1") {
		t.Errorf("缩进不是 2 空格：\n%s", pretty)
	}
	min, err := Apply("json.minify", pretty)
	if err != nil {
		t.Fatalf("minify: %v", err)
	}
	if min != `{"a":1,"b":[1,2]}` {
		t.Errorf("minify 结果 = %q", min)
	}
	// minify 的结果不该带结尾换行（Encoder 会自己加一个）。
	if strings.HasSuffix(min, "\n") {
		t.Error("minify 结果带着结尾换行")
	}

	// 非 JSON：必须报错，不能"失败时返回原文"。
	if _, err := Apply("json.beautify", "not json at all"); err == nil {
		t.Error("对非 JSON 输入没有报错")
	}
	// 多个值同上。
	if _, err := Apply("json.beautify", `{"a":1} {"b":2}`); err == nil {
		t.Error("对多个 JSON 值没有报错")
	}
	// 空输入。
	if _, err := Apply("json.minify", "   "); err == nil {
		t.Error("对空输入没有报错")
	}
}

// TestJSONMinify_DoesNotHTMLEscape 是一条具体的"读起来不对"的回归。
//
// json.Encoder 默认 SetEscapeHTML(true)，会把 `<` `>` `&` 写成 `\u003c` …
// 用户拿它去压一段含 URL 的配置时会发现链接变成了 \u0026。
func TestJSONMinify_DoesNotHTMLEscape(t *testing.T) {
	out, err := Apply("json.minify", `{"u":"https://x.dev/?a=1&b=2","t":"<b>"}`)
	if err != nil {
		t.Fatalf("minify: %v", err)
	}
	if strings.Contains(out, `\u0026`) || strings.Contains(out, `\u003c`) {
		t.Errorf("压缩结果里出现了 HTML 转义：%s", out)
	}
	if !strings.Contains(out, "a=1&b=2") {
		t.Errorf("& 没了：%s", out)
	}
}

// TestRemoveBlankLines 覆盖真实粘贴场景。
func TestRemoveBlankLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\n\n\nb", "a\nb"},
		{"a\n   \nb", "a\nb"},
		{"a  \nb", "a\nb"}, // 行尾空白也去掉
		{"\n\n\na\n", "a"}, // 首尾空行
		{"  indented\n\t\ttabbed", "  indented\n\t\ttabbed"}, // 行首缩进必须保留
		{"a\r\n\r\nb", "a\nb"},                               // Windows 换行
		{"", ""},
		{"\n\n", ""},
	}
	for _, c := range cases {
		got, err := Apply("lines.removeBlank", c.in)
		if err != nil {
			t.Fatalf("removeBlank(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("removeBlank(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// TestStripUTM 覆盖"只做减法"这条硬承诺。
func TestStripUTM(t *testing.T) {
	cases := []struct{ in, want string }{
		// 只剥 utm_*，保留其余参数的**原顺序**。
		{"https://a.dev/p?utm_source=x&id=7&utm_medium=y", "https://a.dev/p?id=7"},
		{"https://a.dev/p?id=7&utm_source=x", "https://a.dev/p?id=7"},
		// 全被剥光 → 连问号一起去掉。
		{"https://a.dev/p?utm_source=x&utm_campaign=y", "https://a.dev/p"},
		// 没有 query：原样返回。
		{"https://a.dev/p", "https://a.dev/p"},
		// fragment 里的 & 不是参数分隔符，必须保住。
		{"https://a.dev/p?utm_source=x#sec=1&k=2", "https://a.dev/p#sec=1&k=2"},
		// 空 query：逐字节原样返回（没有可减的东西就别动它）。
		{"https://a.dev/p?", "https://a.dev/p?"},
		// 大小写不敏感。
		{"https://a.dev/p?UTM_Source=x&k=1", "https://a.dev/p?k=1"},
		// 其它平台跟踪参数。
		{"https://a.dev/p?gclid=abc&k=1", "https://a.dev/p?k=1"},
		// 名字前缀相同但不同名的不该误伤。
		{"https://a.dev/p?utm=keep&utma=keep2", "https://a.dev/p?utm=keep&utma=keep2"},
		// 无值参数。
		{"https://a.dev/p?utm_source&k=1", "https://a.dev/p?k=1"},
	}
	for _, c := range cases {
		got, err := Apply("url.stripUTM", c.in)
		if err != nil {
			t.Fatalf("stripUTM(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("stripUTM(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}

	// 百分号转义与 `+` 必须逐字节保留：用 net/url 重新编码会改掉它们。
	raw := "https://a.dev/%E4%B8%AD%E6%96%87?q=a+b&utm_source=x"
	got, err := Apply("url.stripUTM", raw)
	if err != nil {
		t.Fatalf("stripUTM: %v", err)
	}
	if got != "https://a.dev/%E4%B8%AD%E6%96%87?q=a+b" {
		t.Errorf("转义被改了：%q", got)
	}

	// 多行 / 空输入要报错而不是猜。
	if _, err := Apply("url.stripUTM", "https://a.dev\nhttps://b.dev"); err == nil {
		t.Error("多行输入没有报错")
	}
	if _, err := Apply("url.stripUTM", "   "); err == nil {
		t.Error("空输入没有报错")
	}
}

// TestBase64_RoundTrip 覆盖编码 → 解码的往返，含中文。
func TestBase64_RoundTrip(t *testing.T) {
	for _, s := range []string{"hello", "中文文档", "a\nb\tc", "🎉 emoji"} {
		enc, err := Apply("base64.encode", s)
		if err != nil {
			t.Fatalf("encode(%q): %v", s, err)
		}
		dec, err := Apply("base64.decode", enc)
		if err != nil {
			t.Fatalf("decode(%q): %v", enc, err)
		}
		if dec != s {
			t.Errorf("往返后变成 %q，原本 %q", dec, s)
		}
	}

	// 网页/邮件里复制来的 Base64 常带换行，必须能解。
	wrapped := "5Lit5paH\n5paH5qGj"
	got, err := Apply("base64.decode", wrapped)
	if err != nil {
		t.Fatalf("带换行的 Base64 解码失败：%v", err)
	}
	if got != "中文文档" {
		t.Errorf("解码结果 = %q", got)
	}
	// 无填充形式（JWT 那种）。
	if got, err := Apply("base64.decode", "5Lit5paH5paH5qGj"); err != nil || got != "中文文档" {
		t.Errorf("无填充解码 = (%q, %v)", got, err)
	}
}

// TestBase64Decode_RefusesInsteadOfPassingOriginal 钉住"失败不返回原文"。
//
// 这条防的是一个很自然但有害的写法：解码失败就把原文返回，
// 于是界面上"转换结果"和原文一模一样，用户以为成功了。
func TestBase64Decode_RefusesInsteadOfPassingOriginal(t *testing.T) {
	in := "这不是 base64！！！"
	out, err := Apply("base64.decode", in)
	if err == nil {
		t.Fatalf("非法输入没有报错，返回 %q", out)
	}
	if out != "" {
		t.Errorf("出错时还返回了内容 %q", out)
	}

	// 合法 Base64 但解出来是二进制（非 UTF-8）→ 也必须报错。
	// "//8=" 解出来是 0xFF 0xFF。
	if _, err := Apply("base64.decode", "//8="); err == nil {
		t.Error("解出非 UTF-8 内容时没有报错")
	}
	if _, err := Apply("base64.encode", ""); err == nil {
		t.Error("空输入编码没有报错")
	}
}

func TestCaseOps(t *testing.T) {
	cases := []struct{ op, in, want string }{
		{"case.upper", "Hello 世界 abc", "HELLO 世界 ABC"},
		{"case.lower", "Hello 世界 ABC", "hello 世界 abc"},
		{"case.title", "hello world", "Hello World"},
		{"case.title", "hello-world", "Hello-World"},
		{"case.title", "the QUICK brown", "The Quick Brown"},
		// 数字也算词首，且数字本身不被改变。
		{"case.title", "v2 release", "V2 Release"},
		// 中文没有大小写，必须原样保留（不是被吃掉）。
		{"case.title", "中文 docs", "中文 Docs"},
		{"case.upper", "", ""},
	}
	for _, c := range cases {
		got, err := Apply(c.op, c.in)
		if err != nil {
			t.Fatalf("%s(%q): %v", c.op, c.in, err)
		}
		if got != c.want {
			t.Errorf("%s(%q) = %q，想要 %q", c.op, c.in, got, c.want)
		}
	}
}

// TestKindOf_IsStableAcrossWrapping 钉住"种类"这条前后端契约。
//
// 前端靠 Kind 决定显示哪条本地化文案，所以：
//   - 每种真实失败都要有**具体**的 Kind（不能全是 KindUnknown，
//     否则前端只能显示一句"转换失败"，用户不知道错在哪）；
//   - 上层用 %w 包一层之后仍要能取出种类（绑定层会给错误加上下文）。
func TestKindOf_IsStableAcrossWrapping(t *testing.T) {
	cases := []struct {
		op   string
		in   string
		want string
	}{
		{"json.beautify", "nope", KindInvalidJSON},
		{"json.beautify", "", KindEmpty},
		{"json.beautify", `{"a":1} {"b":2}`, KindMultipleJSON},
		{"base64.decode", "!!!not base64!!!", KindInvalidBase64},
		{"base64.decode", "   ", KindEmpty},
		{"base64.decode", "//8=", KindBinaryResult},
		{"url.stripUTM", "a\nb", KindMultipleLines},
		{"url.stripUTM", "", KindEmpty},
		{"lines.removeBlank", strings.Repeat("a", maxTransformInput+1), KindTooLarge},
		{"no.such.op", "x", KindUnknownOp},
	}
	for _, c := range cases {
		_, err := Apply(c.op, c.in)
		if err == nil {
			t.Errorf("%s(%q) 没有报错，期望 Kind=%s", c.op, trunc(c.in), c.want)
			continue
		}
		if got := KindOf(err); got != c.want {
			t.Errorf("%s(%q) 的 Kind = %q，想要 %q（%v）", c.op, trunc(c.in), got, c.want, err)
		}
		// 包一层之后种类不变。
		wrapped := fmt.Errorf("binding: %w", err)
		if got := KindOf(wrapped); got != c.want {
			t.Errorf("包装后 Kind = %q，想要 %q —— 说明 KindOf 用了类型断言而不是 errors.As", got, c.want)
		}
	}
	if got := KindOf(nil); got != "" {
		t.Errorf("KindOf(nil) = %q，想要空串", got)
	}
	if got := KindOf(errors.New("别的包的错误")); got != KindUnknown {
		t.Errorf("外部错误的 Kind = %q，想要 %q", got, KindUnknown)
	}
}

func trunc(s string) string {
	if len(s) > 20 {
		return s[:20] + "…"
	}
	return s
}

// TestGuardSize_RejectsHugeInput 保证"粘进来 100 MB"是个明确错误。
func TestGuardSize_RejectsHugeInput(t *testing.T) {
	huge := strings.Repeat("a", maxTransformInput+1)
	for _, id := range OpIDs() {
		if _, err := Apply(id, huge); err == nil {
			t.Errorf("%s 对超大输入没有报错", id)
		}
	}
}
