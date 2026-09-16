package clipboard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 Windows 侧必须做的第二个转换：CF_HTML 头部剥离（docs/DESIGN.md §8 第 7 条）。
// 同样是纯函数，在 macOS 上直接单测。

// ErrBadClipboardHTML 表示 "HTML Format" 数据里找不到可用的 HTML 片段。
var ErrBadClipboardHTML = errors.New("clipboard: cannot locate HTML fragment in CF_HTML payload")

// cfHTMLKeys 是规范里定义的头部字段。除 Version 外全部是**字节偏移量**，
// 从整块数据的第 0 字节起算，写成 8~10 位零填充十进制。
type cfHTMLHeader struct {
	version      string
	startHTML    int
	endHTML      int
	startFrag    int
	endFrag      int
	hasStartFrag bool
	hasEndFrag   bool
}

// StripClipboardHTMLHeader 剥离 Windows "HTML Format" 的
// `Version:0.9\r\nStartHTML:...` 头部，取出真正的 HTML 片段。
//
// 容错策略（每一步都能独立兜底，因为生产这个格式的 App 五花八门）：
//
//  1. 优先用 StartFragment / EndFragment 的偏移量；
//  2. 偏移量不可用或明显不对时退回 StartHTML / EndHTML；
//  3. 再不行就退到第一个 '<' 起；
//  4. 最后若片段里还留着 `<!--StartFragment-->` 标记，再按标记裁一次。
//
// 输入没有头部（纯 HTML）时直接返回原文本。
func StripClipboardHTMLHeader(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrBadClipboardHTML
	}
	// CF_HTML 用 NUL 结尾，偏移量不含它；先剥掉再算偏移，避免尾部多余 NUL。
	body := data
	for len(body) > 0 && body[len(body)-1] == 0 {
		body = body[:len(body)-1]
	}
	if len(body) == 0 {
		return "", ErrBadClipboardHTML
	}

	hdr, headerEnd := parseCFHTMLHeader(body)

	// 头部偏移量只有在"自洽"时才可信。
	//
	// 为什么必须校验：偏移量是按字节算的，一旦数据在传输途中被换了行尾
	// （\r\n → \n，头部每行短 1 字节），全部偏移量都会整体错位，
	// 按它切片会得到一段从标签中间开始的垃圾。
	//
	// 判据是"我们解析出的头部结束位置"必须正好落在 StartHTML 上——这是规范
	// 隐含的自洽性（头部之后紧接着就是 HTML 本体）。不自洽就整个放弃偏移量、
	// 走兜底路径；兜底路径靠片段标记裁剪，对这类错位反而更稳。
	trustOffsets := hdr.startHTML >= 0 &&
		headerEnd >= hdr.startHTML-2 && headerEnd <= hdr.startHTML+2 &&
		hdr.startHTML < len(body) &&
		(body[hdr.startHTML] == '<' || isSpaceByte(body[hdr.startHTML]))

	var frag string
	if trustOffsets &&
		hdr.hasStartFrag && hdr.hasEndFrag &&
		hdr.startFrag >= 0 && hdr.endFrag > hdr.startFrag && hdr.endFrag <= len(body) {
		frag = string(body[hdr.startFrag:hdr.endFrag])
	}
	if !looksLikeHTML(frag) && trustOffsets &&
		hdr.endHTML > hdr.startHTML && hdr.endHTML <= len(body) {
		if cand := string(body[hdr.startHTML:hdr.endHTML]); looksLikeHTML(cand) {
			frag = cand
		}
	}
	if !looksLikeHTML(frag) {
		// 偏移量都不可用：从头扫第一个 '<'。起点至少跳过已识别的头部，
		// 防止从 "Version:0.9" 里的字符误判。
		rest := body
		if headerEnd > 0 && headerEnd < len(body) {
			rest = body[headerEnd:]
		}
		if i := indexByte(rest, '<'); i >= 0 {
			frag = string(rest[i:])
		}
	}
	if !looksLikeHTML(frag) {
		return "", fmt.Errorf("%w (%d bytes)", ErrBadClipboardHTML, len(data))
	}

	// 片段里残留的标记再裁一次。
	if i := strings.Index(frag, "<!--StartFragment-->"); i >= 0 {
		frag = frag[i+len("<!--StartFragment-->"):]
		if j := strings.Index(frag, "<!--EndFragment-->"); j >= 0 {
			frag = frag[:j]
		}
	}
	frag = strings.Trim(frag, "\x00")
	if strings.TrimSpace(frag) == "" {
		return "", ErrBadClipboardHTML
	}
	return frag, nil
}

// parseCFHTMLHeader 解析头部，返回字段与"头部结束位置"（头部之后第一个字节）。
// 没有头部时返回零值 headerEnd = 0。
func parseCFHTMLHeader(b []byte) (cfHTMLHeader, int) {
	var h cfHTMLHeader
	h.startHTML, h.endHTML, h.startFrag, h.endFrag = -1, -1, -1, -1

	pos := 0
	sawAny := false
	for pos < len(b) {
		// 取一行（\r\n 或 \n 都可）
		end := -1
		for i := pos; i < len(b); i++ {
			if b[i] == '\n' {
				end = i
				break
			}
			if b[i] == '<' {
				// 还没换行就撞上 HTML 本体，说明没有头部
				break
			}
		}
		if end < 0 {
			break
		}
		line := string(b[pos:end])
		line = strings.TrimRight(line, "\r")
		pos = end + 1

		colon := strings.IndexByte(line, ':')
		if colon <= 0 || !isAlphaString(line[:colon]) {
			break
		}
		key := strings.ToLower(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		sawAny = true

		switch key {
		case "version":
			h.version = val
		case "starthtml":
			h.startHTML = atoiSafe(val)
		case "endhtml":
			h.endHTML = atoiSafe(val)
		case "startfragment":
			h.startFrag = atoiSafe(val)
			h.hasStartFrag = true
		case "endfragment":
			h.endFrag = atoiSafe(val)
			h.hasEndFrag = true
		}
	}
	if !sawAny {
		return h, 0
	}
	return h, pos
}

func looksLikeHTML(s string) bool {
	return strings.Contains(s, "<") && strings.TrimSpace(s) != ""
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isAlphaString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func atoiSafe(s string) int {
	// 偏移量可能是 "0000000105" 也可能是 "105" 或空串
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimLeft(s, "0"))
	if err != nil {
		if strings.Trim(s, "0") == "" {
			return 0
		}
		return -1
	}
	return n
}

func indexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// BuildClipboardHTML 生成带 CF_HTML 头部的字节流（回写方向）。
//
// 头部各字段固定 10 位零填充，因此头部长度与偏移量无关、一次算准，
// 不需要"先占位再回填"的迭代。sourceURL 为空时省略 SourceURL 行。
func BuildClipboardHTML(fragment, sourceURL string) []byte {
	const tmpl = "Version:0.9\r\n" +
		"StartHTML:%010d\r\n" +
		"EndHTML:%010d\r\n" +
		"StartFragment:%010d\r\n" +
		"EndFragment:%010d\r\n"

	const prefix = "<html><body><!--StartFragment-->"
	const suffix = "<!--EndFragment--></body></html>"

	// 先按占位符量出头部长度（10 位数字不会因取值变化而改变长度）
	head := fmt.Sprintf(tmpl, 0, 0, 0, 0)
	if sourceURL != "" {
		head += "SourceURL:" + sourceURL + "\r\n"
	}

	startHTML := len(head)
	startFrag := startHTML + len(prefix)
	endFrag := startFrag + len(fragment)
	endHTML := endFrag + len(suffix)

	head = fmt.Sprintf(tmpl, startHTML, endHTML, startFrag, endFrag)
	if sourceURL != "" {
		head += "SourceURL:" + sourceURL + "\r\n"
	}
	if len(head) != startHTML {
		// 理论上不会发生；发生了说明模板被改动，宁可让测试炸掉也不要发出坏偏移量
		panic(fmt.Sprintf("clipboard: CF_HTML header length drift: %d != %d", len(head), startHTML))
	}

	var sb strings.Builder
	sb.Grow(len(head) + len(prefix) + len(fragment) + len(suffix) + 1)
	sb.WriteString(head)
	sb.WriteString(prefix)
	sb.WriteString(fragment)
	sb.WriteString(suffix)
	sb.WriteByte(0)
	return []byte(sb.String())
}
