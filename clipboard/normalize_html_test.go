package clipboard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cfHTMLSample 按 Windows "HTML Format" 的真实长相拼一段数据：
// 头部若干 "Key:Value<le>" + 正文，各偏移量都是**字节**偏移。
// le 为空时用规范默认的 CRLF。
func cfHTMLSample(html string, le ...string) ([]byte, string) {
	lineEnd := "\r\n"
	if len(le) > 0 && le[0] != "" {
		lineEnd = le[0]
	}
	frag := html
	head := "Version:0.9" + lineEnd +
		"StartHTML:%010d" + lineEnd +
		"EndHTML:%010d" + lineEnd +
		"StartFragment:%010d" + lineEnd +
		"EndFragment:%010d" + lineEnd +
		"SourceURL:https://example.com/page" + lineEnd
	prefix := "<html><body><!--StartFragment-->"
	suffix := "<!--EndFragment--></body></html>"

	headLen := len(fmt.Sprintf(head, 0, 0, 0, 0))
	startHTML := headLen
	startFrag := startHTML + len(prefix)
	endFrag := startFrag + len(frag)
	endHTML := endFrag + len(suffix)

	full := fmt.Sprintf(head, startHTML, endHTML, startFrag, endFrag) +
		prefix + frag + suffix + "\x00"
	return []byte(full), frag
}

func TestStripClipboardHTMLHeader_Typical(t *testing.T) {
	data, want := cfHTMLSample(`<b>加粗</b>`)
	got, err := StripClipboardHTMLHeader(data)
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if got != want {
		t.Fatalf("片段 = %q, want %q", got, want)
	}
}

func TestStripClipboardHTMLHeader_NoFragmentOffsets(t *testing.T) {
	// 有些生产者只给 StartHTML / EndHTML
	body := "<html><body><i>只有整页偏移</i></body></html>"
	head := "Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\n"
	headLen := len(fmt.Sprintf(head, 0, 0))
	full := fmt.Sprintf(head, headLen, headLen+len(body)) + body + "\x00"
	got, err := StripClipboardHTMLHeader([]byte(full))
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if !strings.Contains(got, "只有整页偏移") {
		t.Fatalf("片段 = %q", got)
	}
}

func TestStripClipboardHTMLHeader_LFOnlyLineEndings(t *testing.T) {
	// 生产者自己用 LF 行尾、并按 LF 算偏移量——这是自洽的，必须正确解析
	data, want := cfHTMLSample("<u>LF</u>", "\n")
	got, err := StripClipboardHTMLHeader(data)
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if got != want {
		t.Fatalf("片段 = %q, want %q", got, want)
	}
}

func TestStripClipboardHTMLHeader_MangledLineEndings(t *testing.T) {
	// CRLF 数据在途中被"规整"成 LF：头部每行少 1 字节，于是全部偏移量
	// 都整体前移了 6 字节。这时绝不能用偏移量切片（会从标签中间开始），
	// 必须靠"偏移量自洽性检查"退回兜底路径。
	data, want := cfHTMLSample("<b>内容</b>")
	mangled := []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
	got, err := StripClipboardHTMLHeader(mangled)
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if got != want {
		t.Fatalf("行尾被改写后片段 = %q, want %q", got, want)
	}
}

func TestStripClipboardHTMLHeader_NoHeaderAtAll(t *testing.T) {
	plain := []byte("<p>本来就是纯 HTML</p>")
	got, err := StripClipboardHTMLHeader(plain)
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if got != string(plain) {
		t.Fatalf("片段 = %q", got)
	}
}

func TestStripClipboardHTMLHeader_BadOffsetsFallBack(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			"偏移量超出缓冲",
			"Version:0.9\r\nStartHTML:0000009999\r\nEndHTML:0000009999\r\n" +
				"StartFragment:0000009999\r\nEndFragment:0000009999\r\n<body>兜底内容</body>",
			"<body>兜底内容</body>",
		},
		{
			"偏移量不是数字",
			"Version:0.9\r\nStartHTML:abc\r\nEndHTML:def\r\n" +
				"StartFragment:ghi\r\nEndFragment:jkl\r\n<b>兜底内容</b>",
			"<b>兜底内容</b>",
		},
		{
			"EndFragment 小于 StartFragment",
			"Version:0.9\r\nStartFragment:0000000200\r\nEndFragment:0000000100\r\n<i>兜底内容</i>",
			"<i>兜底内容</i>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := StripClipboardHTMLHeader([]byte(c.data))
			if err != nil {
				t.Fatalf("StripClipboardHTMLHeader: %v", err)
			}
			if got != c.want {
				t.Fatalf("片段 = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripClipboardHTMLHeader_LeavesResidualMarkers(t *testing.T) {
	// 生产者把片段标记留在偏移范围之内时的再裁剪
	body := "<!--StartFragment-->真正的内容<!--EndFragment-->"
	head := "Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\nStartFragment:%010d\r\nEndFragment:%010d\r\n"
	headLen := len(fmt.Sprintf(head, 0, 0, 0, 0))
	full := fmt.Sprintf(head, headLen, headLen+len(body), headLen, headLen+len(body)) + body + "\x00"
	got, err := StripClipboardHTMLHeader([]byte(full))
	if err != nil {
		t.Fatalf("StripClipboardHTMLHeader: %v", err)
	}
	if got != "真正的内容" {
		t.Fatalf("片段 = %q, want %q", got, "真正的内容")
	}
}

func TestStripClipboardHTMLHeader_Errors(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"空", nil},
		{"只有 NUL", []byte{0, 0}},
		{"全是头部没有正文", []byte("Version:0.9\r\nStartHTML:0000000099\r\n")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := StripClipboardHTMLHeader(c.data); !errors.Is(err, ErrBadClipboardHTML) {
				t.Fatalf("err = %v, want ErrBadClipboardHTML", err)
			}
		})
	}
}

// 回写方向的头部必须自洽：Build 出来的东西，Strip 回去必须一模一样。
// 偏移量算错的话这里会立刻炸。
func TestBuildAndStripClipboardHTML_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fragment string
		url      string
	}{
		{"简单", "<b>hi</b>", ""},
		{"带 URL", "<b>hi</b>", "https://example.com/x"},
		{"多字节中文", "<p>喵喵贴 · PawClip</p>", ""},
		{"含引号与换行", "<a href=\"https://a.b/c?d=1\">链\n接</a>", "https://a.b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := BuildClipboardHTML(tc.fragment, tc.url)
			got, err := StripClipboardHTMLHeader(data)
			if err != nil {
				t.Fatalf("StripClipboardHTMLHeader: %v", err)
			}
			if got != tc.fragment {
				t.Fatalf("往返后片段 = %q, want %q", got, tc.fragment)
			}
		})
	}
}

func TestBuildClipboardHTML_OffsetsAreByteAccurate(t *testing.T) {
	frag := "中文片段" // 每个字 3 字节，字节数 != 字符数
	data := BuildClipboardHTML(frag, "")
	hdr, headerEnd := parseCFHTMLHeader(data)
	if hdr.version != "0.9" {
		t.Fatalf("Version = %q", hdr.version)
	}
	if headerEnd != hdr.startHTML {
		t.Fatalf("头部结束于 %d，但 StartHTML = %d", headerEnd, hdr.startHTML)
	}
	got := string(data[hdr.startFrag:hdr.endFrag])
	if got != frag {
		t.Fatalf("按偏移量取片段 = %q, want %q", got, frag)
	}
	if hdr.endHTML != len(data)-1 { // 末尾有个 NUL
		t.Fatalf("EndHTML = %d, 数据长度 %d", hdr.endHTML, len(data))
	}
}
