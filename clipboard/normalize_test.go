package clipboard

import (
	"strings"
	"testing"
	"time"
)

func sp(s string) *string { return &s }

func TestClassifyRawType(t *testing.T) {
	cases := []struct {
		name string
		role Role
	}{
		// macOS
		{"public.utf8-plain-text", RoleText},
		{"public.UTF8-PLAIN-TEXT", RoleText}, // 大小写不敏感
		{"public.html", RoleHTML},
		{"public.rtf", RoleRTF},
		{"public.png", RoleImage},
		{"public.tiff", RoleImage},
		{"public.file-url", RoleFiles},
		{"NSFilenamesPboardType", RoleFiles},
		// Windows
		{"CF_UNICODETEXT", RoleText},
		{"HTML Format", RoleHTML},
		{"Rich Text Format", RoleRTF},
		{"CF_DIBV5", RoleImage},
		{"PNG", RoleImage},
		{"CF_HDROP", RoleFiles},
		// 保密
		{"org.nspasteboard.ConcealedType", RolePrivate},
		{"org.nspasteboard.TransientType", RolePrivate},
		{"ExcludeClipboardContentFromMonitorProcessing", RolePrivate},
		// 未知
		{"com.example.whatever", RoleUnknown},
		{"", RoleUnknown},
	}
	for _, c := range cases {
		if got := ClassifyRawType(c.name); got != c.role {
			t.Errorf("ClassifyRawType(%q) = %q, want %q", c.name, got, c.role)
		}
	}
}

func TestIsPrivateTypeNames(t *testing.T) {
	if IsPrivateTypeNames(nil) {
		t.Fatal("empty list must not be private")
	}
	if IsPrivateTypeNames([]string{"public.utf8-plain-text", "public.html"}) {
		t.Fatal("plain text/html must not be private")
	}
	if !IsPrivateTypeNames([]string{"public.utf8-plain-text", "org.nspasteboard.ConcealedType"}) {
		t.Fatal("ConcealedType must be detected")
	}
	if !IsPrivateTypeNames([]string{"Clipboard Viewer Ignore"}) {
		t.Fatal("Windows Clipboard Viewer Ignore must be detected")
	}
}

func TestContentKind(t *testing.T) {
	cases := []struct {
		name string
		c    Content
		want string
	}{
		{"纯文本", Content{Text: sp("hi")}, KindText},
		{"浏览器复制（text+html）应判 html 而非 mixed", Content{Text: sp("hi"), HTML: sp("<b>hi</b>")}, KindHTML},
		{"Word 复制（text+html+rtf）应判 html", Content{Text: sp("hi"), HTML: sp("<b>hi</b>"), RTF: []byte("{\\rtf1 hi}")}, KindHTML},
		{"纯 RTF", Content{RTF: []byte("{\\rtf1 hi}")}, KindRTF},
		{"图片", Content{PNG: []byte{1}}, KindImage},
		{"文件", Content{Files: []string{"/a/b"}}, KindFiles},
		{"文本 + 图片 才是 mixed", Content{Text: sp("hi"), PNG: []byte{1}}, KindMixed},
		{"图片 + 文件 也是 mixed", Content{PNG: []byte{1}, Files: []string{"/a"}}, KindMixed},
		{"空", Content{}, ""},
		{"空字符串不算内容", Content{Text: sp("")}, ""},
	}
	for _, tc := range cases {
		if got := tc.c.Kind(); got != tc.want {
			t.Errorf("%s: Kind() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestContentGroups(t *testing.T) {
	browser := Content{Text: sp("hi"), HTML: sp("<b>hi</b>")}
	g := browser.Groups()
	if len(g) != 1 || g[0] != GroupText {
		t.Fatalf("浏览器复制应只属于 text 组，得到 %v", g)
	}

	mixed := Content{Text: sp("hi"), PNG: []byte{1}, Files: []string{"/a"}}
	g = mixed.Groups()
	if strings.Join(g, ",") != "text,image,files" {
		t.Fatalf("Groups() = %v, want [text image files]", g)
	}

	if got := (Content{}).Groups(); len(got) != 0 {
		t.Fatalf("空内容 Groups() 应为空，得到 %v", got)
	}
}

func TestFingerprintStabilityAndFraming(t *testing.T) {
	a := Content{Text: sp("中文文档 hello")}
	if Fingerprint(a) != Fingerprint(a) {
		t.Fatal("同一内容指纹必须稳定")
	}
	if !strings.HasPrefix(Fingerprint(a), "sha256:") {
		t.Fatal("指纹必须以 sha256: 开头")
	}
	if len(Fingerprint(a)) != len("sha256:")+64 {
		t.Fatalf("指纹长度不对：%q", Fingerprint(a))
	}

	// 长度前缀分帧：不同切分不能撞哈希
	x := Fingerprint(Content{Text: sp("ab"), HTML: sp("c")})
	y := Fingerprint(Content{Text: sp("a"), HTML: sp("bc")})
	if x == y {
		t.Fatal("长度前缀分帧失效：(\"ab\",\"c\") 与 (\"a\",\"bc\") 撞了")
	}

	// 类型标签：同样的字节放在不同角色下不能撞
	p := Fingerprint(Content{Text: sp("same")})
	q := Fingerprint(Content{HTML: sp("same")})
	if p == q {
		t.Fatal("角色标签失效：text 与 html 撞了")
	}

	// 文件列表顺序有意义
	f1 := Fingerprint(Content{Files: []string{"/a", "/b"}})
	f2 := Fingerprint(Content{Files: []string{"/b", "/a"}})
	if f1 == f2 {
		t.Fatal("文件列表顺序不同不应得到同一指纹")
	}
}

// 这是 SelfWriteGuard 的立身之本：回写载荷与采集快照必须算出同一个指纹。
func TestPayloadAndRawFingerprintMatch(t *testing.T) {
	text := "从面板回贴的一段中文文本"
	payload := &Payload{Text: &text}
	raw := &Raw{
		Text:        &text,
		RawTypes:    []string{"public.utf8-plain-text"},
		Image:       nil,
		SourceAppID: "com.pawclip.app",
	}
	if payload.Fingerprint() != raw.Fingerprint() {
		t.Fatalf("回写与采集算出的指纹不一致：%s vs %s",
			payload.Fingerprint(), raw.Fingerprint())
	}

	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	p2 := &Payload{PNG: png}
	r2 := &Raw{Image: &Image{PNG: png, Width: 1, Height: 1}}
	if p2.Fingerprint() != r2.Fingerprint() {
		t.Fatal("图片载荷与图片快照的指纹不一致")
	}
}

func TestTruncateTextByRunes(t *testing.T) {
	// 每个中文 3 字节；按字节截断会切碎 UTF-8
	s := "文档文档文档"
	got, truncated := TruncateText(s, 3)
	if !truncated || got != "文档文" {
		t.Fatalf("TruncateText = %q (truncated=%v), want \"文档文\"", got, truncated)
	}
	if !utf8Valid(got) {
		t.Fatal("截断结果不是合法 UTF-8")
	}
	// 不超限时原样返回且 truncated=false
	if got, tr := TruncateText("短", 10); tr || got != "短" {
		t.Fatalf("未超限不应截断：%q %v", got, tr)
	}
	// max<=0 视为不截断
	if got, tr := TruncateText(s, 0); tr || got != s {
		t.Fatal("max<=0 应原样返回")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestPreview(t *testing.T) {
	// 图片摘要的格式与 BACKUP-FORMAT.md §3.4 的示例一致
	if got := Preview(Content{PNG: []byte{1}}, KindImage, 1080, 1920); got != "图片 1080 × 1920" {
		t.Fatalf("图片摘要 = %q", got)
	}
	if got := Preview(Content{PNG: []byte{1}}, KindImage, 0, 0); got != "图片" {
		t.Fatalf("无尺寸的图片摘要 = %q", got)
	}
	if got := Preview(Content{Files: []string{"/Users/me/报告.pdf"}}, KindFiles, 0, 0); got != "报告.pdf" {
		t.Fatalf("单文件摘要 = %q", got)
	}
	if got := Preview(Content{Files: []string{"/a/x.txt", "/b/y.txt"}}, KindFiles, 0, 0); got != "x.txt 等 2 个文件" {
		t.Fatalf("多文件摘要 = %q", got)
	}

	// 空白折叠成单行
	got := Preview(Content{Text: sp("第一行\n\n  第二行\t第三行 ")}, KindText, 0, 0)
	if got != "第一行 第二行 第三行" {
		t.Fatalf("空白折叠 = %q", got)
	}

	// HTML 取文本
	got = Preview(Content{HTML: sp("<p>Hello&nbsp;<b>World</b></p>")}, KindHTML, 0, 0)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") || strings.Contains(got, "<") {
		t.Fatalf("HTML 摘要 = %q", got)
	}

	// 200 字符上限
	long := strings.Repeat("甲", 500)
	got = Preview(Content{Text: &long}, KindText, 0, 0)
	if n := len([]rune(got)); n != PreviewMaxChars {
		t.Fatalf("摘要长度 = %d, want %d", n, PreviewMaxChars)
	}

	// RTF 取文本
	got = Preview(Content{RTF: []byte(`{\rtf1\ansi 你好世界\par}`)}, KindRTF, 0, 0)
	if !strings.Contains(got, "你好世界") {
		t.Fatalf("RTF 摘要 = %q", got)
	}
}

func TestDebouncer(t *testing.T) {
	d := NewDebouncer(120 * time.Millisecond)
	base := time.Unix(1000, 0)

	if !d.ShouldProcess("fp1", base) {
		t.Fatal("第一次必须放行")
	}
	if d.ShouldProcess("fp1", base.Add(50*time.Millisecond)) {
		t.Fatal("窗口内同指纹必须丢弃")
	}
	if !d.ShouldProcess("fp2", base.Add(60*time.Millisecond)) {
		t.Fatal("不同指纹必须放行")
	}
	if !d.ShouldProcess("fp1", base.Add(200*time.Millisecond)) {
		t.Fatal("超出窗口必须放行")
	}

	d.Reset()
	if !d.ShouldProcess("fp1", base) {
		t.Fatal("Reset 后必须放行")
	}

	// 关闭去抖
	off := NewDebouncer(0)
	if !off.ShouldProcess("x", base) || !off.ShouldProcess("x", base) {
		t.Fatal("window<=0 必须全部放行")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"com.1password.*", "com.1password.mac", true},
		{"com.1password.*", "com.1password.mac.helper", true},
		{"com.1password.*", "com.1passwordx", false},
		{"com.apple.keychainaccess", "com.apple.keychainaccess", true},
		{"com.apple.keychainaccess", "com.apple.keychainaccess2", false},
		{"com.agilebits.*", "com.agilebits.onepassword7", true},
		{"*1password*", `c:\program files\1password\1password.exe`, true},
		{"com.a?ple.*", "com.apple.safari", true},
		{"com.a?ple.*", "com.aapple.safari", false},
		{"*", "anything", true},
		{"", "", true},
		{"", "x", false},
		{"com.exact", "COM.EXACT", true}, // 模式与输入都已小写化，这里验证已小写路径
	}
	for _, c := range cases {
		pat, s := strings.ToLower(c.pat), strings.ToLower(c.s)
		if got := globMatch(pat, s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestPayloadEmpty(t *testing.T) {
	if !(*Payload)(nil).Empty() {
		t.Fatal("nil payload 应为空")
	}
	if !(&Payload{}).Empty() {
		t.Fatal("零值 payload 应为空")
	}
	if (&Payload{Text: sp("")}).Empty() {
		t.Fatal("带非 nil Text 的 payload 不算空")
	}
}
