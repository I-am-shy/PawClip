package clipboard

import (
	"bytes"
	"testing"
	"unsafe"
)

// 这个文件是 CF_HDROP 的"契约测试"。
//
// 它存在的唯一理由是：DROPFILES 的字段布局是对外协议，写错了**本进程内
// 完全不报错**——只会和其它 Windows 程序互相读不懂。而 Windows 后端在
// macOS 上跑不起来，所以布局与解析必须先在这里被钉死。

// TestDROPFILES_LayoutMatchesWin32 把结构体布局与 Win32 的 C 定义对齐。
//
// 一旦有人往 dropFiles 里加字段（历史上真的发生过：多了一个 FNC int32），
// sizeof 会从 16 变成 20，fWide 会从偏移 12 滑到 16，这个测试立刻红。
func TestDROPFILES_LayoutMatchesWin32(t *testing.T) {
	if got := unsafe.Sizeof(dropFiles{}); got != 16 {
		t.Fatalf("sizeof(DROPFILES) = %d，Win32 定义是 16", got)
	}
	if got := DROPFILESHeaderSize; got != 16 {
		t.Fatalf("DROPFILESHeaderSize = %d，想要 16", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.PFiles); got != 0 {
		t.Errorf("pFiles 偏移 = %d，想要 0", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.Pt); got != 4 {
		t.Errorf("pt 偏移 = %d，想要 4", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.FWide); got != 12 {
		t.Errorf("fWide 偏移 = %d，想要 12（这正是当年写错的那个字段）", got)
	}
}

// TestBuildDROPFILES_HeaderAndPayload 检查我们写出去的字节长什么样。
func TestBuildDROPFILES_HeaderAndPayload(t *testing.T) {
	got := buildDROPFILES([]string{"C:\\a.txt"})

	if len(got) < DROPFILESHeaderSize {
		t.Fatalf("载荷太短：%d 字节", len(got))
	}
	df := (*dropFiles)(unsafe.Pointer(&got[0]))
	if df.PFiles != 16 {
		t.Errorf("pFiles = %d，想要 16（其它程序按 16 找载荷，写成 20 会吃掉第一个字符）", df.PFiles)
	}
	if df.FWide == 0 {
		t.Error("fWide = 0：中文路径会被当成 ANSI 写出去")
	}

	// 载荷：'C',':','\\','a','.','t','x','t' 的 UTF-16LE + NUL + 列表结束 NUL
	want := []byte{
		'C', 0, ':', 0, '\\', 0, 'a', 0, '.', 0, 't', 0, 'x', 0, 't', 0,
		0, 0, // 路径结束
		0, 0, // 列表结束（双 NUL）
	}
	if body := got[DROPFILESHeaderSize:]; !bytes.Equal(body, want) {
		t.Errorf("载荷 =\n  %v\n想要\n  %v", body, want)
	}
}

// TestDROPFILES_RoundTrip 自产自销：写出去再读回来必须一模一样。
func TestDROPFILES_RoundTrip(t *testing.T) {
	cases := [][]string{
		{"C:\\Users\\zego\\报告.txt"},
		{"/tmp/a.txt", "/tmp/b c/d.png"},
		{"C:\\含 空格\\喵喵贴 🐾.png"},
		{"C:\\one.txt", "D:\\two\\中文目录\\three.dat", "E:\\four"},
	}
	for _, paths := range cases {
		blob := buildDROPFILES(paths)
		df := (*dropFiles)(unsafe.Pointer(&blob[0]))
		got := parseDROPFILES(blob[df.PFiles:], df.FWide != 0)
		if len(got) != len(paths) {
			t.Fatalf("%v → 解析出 %d 条：%v", paths, len(got), got)
		}
		for i := range paths {
			if got[i] != paths[i] {
				t.Errorf("第 %d 条 = %q，想要 %q", i, got[i], paths[i])
			}
		}
	}
}

// TestParseDROPFILES_AsWrittenByExplorer 是关键回归用例。
//
// 这里的字节是**手写的**，完全照资源管理器（以及任何守规范的 Windows 程序）
// 的写法：16 字节头部、pFiles = 16、fWide = 1。当年的实现因为结构体多了一个
// 字段，会从偏移 16 读 fWide（读到的是第一个路径的前两个字符），于是
// 中文路径被按 Latin-1 透传成乱码。既然不能靠真机发现，就在这里拦住。
func TestParseDROPFILES_AsWrittenByExplorer(t *testing.T) {
	paths := []string{"C:\\Users\\zego\\文件.txt", "D:\\照片\\🐾.png"}

	blob := handBuildDROPFILES(t, paths, 16 /* pFiles */, 1 /* fWide */)

	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if df.PFiles != 16 {
		t.Fatalf("测试数据构造错误：pFiles = %d", df.PFiles)
	}
	got := parseDROPFILES(blob[df.PFiles:], df.FWide != 0)
	if len(got) != 2 {
		t.Fatalf("解析出 %d 条：%v", len(got), got)
	}
	if got[0] != paths[0] {
		t.Errorf("第 0 条 = %q，想要 %q", got[0], paths[0])
	}
	if got[1] != paths[1] {
		t.Errorf("第 1 条 = %q，想要 %q（代理对 emoji 也要能过）", got[1], paths[1])
	}
}

// TestParseDROPFILES_AsWrittenByExplorerANSI 是上面那条的 ANSI 兄弟。
//
// 它专门抓"fWide 读错位置"：fWide 落在偏移 12，若从 16 去读，读到的就是
// 第一个路径的前 4 个字节（'C' ':' '\\' 'a' …），恒为非 0，于是 ANSI 载荷
// 会被当成 UTF-16LE 解析成乱码。
func TestParseDROPFILES_AsWrittenByExplorerANSI(t *testing.T) {
	paths := []string{"C:\\a.txt", "D:\\b c\\d.dat"}

	blob := handBuildDROPFILES(t, paths, 16 /* pFiles */, 0 /* fWide: ANSI */)

	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if df.FWide != 0 {
		t.Fatalf("fWide 读成了 %d，应当是 0——说明读的位置不是偏移 12", df.FWide)
	}
	got := parseDROPFILES(blob[df.PFiles:], false)
	if len(got) != 2 || got[0] != paths[0] || got[1] != paths[1] {
		t.Fatalf("解析出 %v，想要 %v", got, paths)
	}
}

// TestParseDROPFILES_ANSI 覆盖 fWide = 0 的分支：按字节透传，不做猜测性转码。
func TestParseDROPFILES_ANSI(t *testing.T) {
	payload := []byte("C:\\a.txt\x00D:\\b c\\d.dat\x00\x00")
	got := parseDROPFILES(payload, false)
	want := []string{"C:\\a.txt", "D:\\b c\\d.dat"}
	if len(got) != len(want) {
		t.Fatalf("解析出 %d 条：%v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q，想要 %q", i, got[i], want[i])
		}
	}
}

// TestParseDROPFILES_MissingFinalNull 载荷没有以双 NUL 收尾时也要把最后一条取到。
func TestParseDROPFILES_MissingFinalNull(t *testing.T) {
	payload := encodeUTF16LE("C:\\only.txt") // 只有一个 NUL，不是双 NUL
	if got := parseDROPFILES(payload, true); len(got) != 1 || got[0] != "C:\\only.txt" {
		t.Fatalf("解析出 %v，想要 [C:\\only.txt]", got)
	}
}

// TestBuildDROPFILES_Empty 空列表也要产出合法的空 CF_HDROP（而不是 nil）。
func TestBuildDROPFILES_Empty(t *testing.T) {
	blob := buildDROPFILES(nil)
	if len(blob) != DROPFILESHeaderSize+2 {
		t.Fatalf("长度 = %d，想要 %d（头部 + 一个双 NUL 列表结束符）",
			len(blob), DROPFILESHeaderSize+2)
	}
	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if got := parseDROPFILES(blob[df.PFiles:], df.FWide != 0); len(got) != 0 {
		t.Fatalf("解析出 %v，想要空", got)
	}
}

// TestEncodeUTF16LE 检查编码本身：NUL 结尾占 2 字节，代理对占 4 字节。
func TestEncodeUTF16LE(t *testing.T) {
	if got := encodeUTF16LE("A"); !bytes.Equal(got, []byte{'A', 0, 0, 0}) {
		t.Errorf("encodeUTF16LE(\"A\") = %v，想要 [65 0 0 0]", got)
	}
	if got := encodeUTF16LE(""); !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("encodeUTF16LE(\"\") = %v，想要 [0 0]", got)
	}
	// 🐾 = U+1F43E → 代理对 D83D DC3E
	if got := encodeUTF16LE("🐾"); !bytes.Equal(got, []byte{0x3D, 0xD8, 0x3E, 0xDC, 0, 0}) {
		t.Errorf("encodeUTF16LE(\"🐾\") = %v，想要 [61 216 62 220 0 0]", got)
	}
}

// handBuildDROPFILES 按 Win32 规范手写一份 CF_HDROP，刻意不复用 buildDROPFILES，
// 这样"被测实现"与"测试数据"才是两条独立的路径。
func handBuildDROPFILES(t *testing.T, paths []string, pFiles uint32, fWide int32) []byte {
	t.Helper()

	header := make([]byte, 16)
	// pFiles @0, pt @4..11（全 0）, fWide @12
	header[0] = byte(pFiles)
	header[12] = byte(fWide)

	if fWide == 0 {
		var body []byte
		for _, p := range paths {
			body = append(body, []byte(p)...)
			body = append(body, 0)
		}
		return append(header, append(body, 0)...)
	}

	var body []byte
	for _, p := range paths {
		for _, r := range p {
			u := uint32(r)
			if u > 0xFFFF {
				u -= 0x10000
				hi := uint16(0xD800 + (u >> 10))
				lo := uint16(0xDC00 + (u & 0x3FF))
				body = append(body, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
				continue
			}
			body = append(body, byte(u), byte(u>>8))
		}
		body = append(body, 0, 0)
	}
	return append(header, append(body, 0, 0)...)
}
