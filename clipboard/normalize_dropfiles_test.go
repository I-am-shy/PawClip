package clipboard

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unsafe"
)

// 这个文件是 CF_HDROP 的"契约测试"。
//
// 它存在的唯一理由是：DROPFILES 的字段布局是对外协议，写错了**本进程内
// 完全不报错**——只会和其它 Windows 程序互相读不懂。而 Windows 后端在
// macOS 上跑不起来，所以布局与解析必须先在这里被钉死。
//
// ⚠️ 2026-10-31 的 Windows 真机验收证明这件事不能靠"结构体自洽"：
// 那一版把结构体定义成 16 字节（漏了 fNC），本文件的
// TestDROPFILES_LayoutMatchesWin32 也跟着断言 16 / fWide@12，于是测试
// 全绿而真实 Explorer 复制被拆成一串单字符"文件"。修复后这里改成：
//   - 布局断言以 Windows SDK（ShlObj_core.h）为准：20 字节、fWide@16；
//   - 关键回归用例用**显式偏移写出的字节**（explorerDROPFILES），
//     不再复用被测结构体，避免"实现错、测试跟着错"。
//
// 见 docs/ACCEPTANCE-WINDOWS.md §3.1。

// TestDROPFILES_LayoutMatchesWin32 把结构体布局与 Win32 的 C 定义对齐。
//
// 权威定义（Windows SDK ShlObj_core.h）：
//
//	typedef struct _DROPFILES {
//	  DWORD pFiles;   // 0
//	  POINT pt;       // 4  (8 字节)
//	  BOOL  fNC;      // 12
//	  BOOL  fWide;    // 16
//	} DROPFILES;       // 20
func TestDROPFILES_LayoutMatchesWin32(t *testing.T) {
	if got := unsafe.Sizeof(dropFiles{}); got != 20 {
		t.Fatalf("sizeof(DROPFILES) = %d，Win32 定义是 20（pFiles + pt + fNC + fWide）", got)
	}
	if got := DROPFILESHeaderSize; got != 20 {
		t.Fatalf("DROPFILESHeaderSize = %d，想要 20", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.PFiles); got != 0 {
		t.Errorf("pFiles 偏移 = %d，想要 0", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.Pt); got != 4 {
		t.Errorf("pt 偏移 = %d，想要 4", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.FNC); got != 12 {
		t.Errorf("fNC 偏移 = %d，想要 12", got)
	}
	if got := unsafe.Offsetof(dropFiles{}.FWide); got != 16 {
		t.Errorf("fWide 偏移 = %d，想要 16（少写 fNC 会让它滑到 12）", got)
	}
}

// TestBuildDROPFILES_HeaderAndPayload 检查我们写出去的字节长什么样。
func TestBuildDROPFILES_HeaderAndPayload(t *testing.T) {
	got := buildDROPFILES([]string{"C:\\a.txt"})

	if len(got) < DROPFILESHeaderSize {
		t.Fatalf("载荷太短：%d 字节", len(got))
	}
	df := (*dropFiles)(unsafe.Pointer(&got[0]))
	if df.PFiles != 20 {
		t.Errorf("pFiles = %d，想要 20（其它程序按 20 找载荷）", df.PFiles)
	}
	if df.FNC != 0 {
		t.Errorf("fNC = %d，想要 0（我们不在非客户区拖放）", df.FNC)
	}
	if df.FWide == 0 {
		t.Error("fWide = 0：中文路径会被当成 ANSI 写出去")
	}

	// 显式按偏移复核一遍：fNC @12、fWide @16，避免只信结构体。
	if v := binary.LittleEndian.Uint32(got[12:16]); v != 0 {
		t.Errorf("header[12:16]（fNC）= %d，想要 0", v)
	}
	if v := binary.LittleEndian.Uint32(got[16:20]); v != 1 {
		t.Errorf("header[16:20]（fWide）= %d，想要 1", v)
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

// TestParseDROPFILES_ExplorerGolden 是关键回归用例。
//
// 字节按 Windows SDK 的布局**用显式偏移**写出来（20 字节头、fNC@12=0、
// fWide@16=1），刻意不复用 dropFiles / buildDROPFILES。只要实现再把 fWide
// 读回偏移 12，就会读到 fNC=0，于是 UTF-16 路径被按 ANSI 逐字节切开——
// 正是 2026-10-31 真机验收里"一个文件变成 52 个单字符文件"的那个 bug。
func TestParseDROPFILES_ExplorerGolden(t *testing.T) {
	paths := []string{"C:\\Users\\shy\\报告.txt", "D:\\照片\\🐾.png"}
	blob := explorerDROPFILES(t, paths)

	// 先按偏移独立复核头部，确认测试数据本身是 SDK 布局。
	if got := binary.LittleEndian.Uint32(blob[0:4]); got != 20 {
		t.Fatalf("测试数据 pFiles = %d，想要 20", got)
	}
	if got := binary.LittleEndian.Uint32(blob[12:16]); got != 0 {
		t.Fatalf("测试数据 fNC = %d，想要 0", got)
	}
	if got := binary.LittleEndian.Uint32(blob[16:20]); got != 1 {
		t.Fatalf("测试数据 fWide = %d，想要 1", got)
	}

	// 结构体必须读出同样的三个字段（fWide 不能在 12）。
	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if df.PFiles != 20 || df.FNC != 0 || df.FWide != 1 {
		t.Fatalf("结构体读到 pFiles=%d fNC=%d fWide=%d，想要 20/0/1",
			df.PFiles, df.FNC, df.FWide)
	}

	got := parseDROPFILES(blob[20:], df.FWide != 0)
	if len(got) != len(paths) {
		t.Fatalf("解析出 %d 条：%v（想要 %d 条）", len(got), got, len(paths))
	}
	for i := range paths {
		if got[i] != paths[i] {
			t.Errorf("第 %d 条 = %q，想要 %q", i, got[i], paths[i])
		}
	}
}

// TestParseDROPFILES_AsWrittenByExplorer 是上面的"结构体版"兄弟：
// 用 dropFiles 写头，确认解析链路在自洽之外也成立。
func TestParseDROPFILES_AsWrittenByExplorer(t *testing.T) {
	paths := []string{"C:\\Users\\zego\\文件.txt", "D:\\照片\\🐾.png"}

	blob := handBuildDROPFILES(t, paths, 20 /* pFiles */, 1 /* fWide */)

	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if df.PFiles != 20 || df.FWide != 1 {
		t.Fatalf("测试数据构造错误：pFiles=%d fWide=%d", df.PFiles, df.FWide)
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
// 它专门抓"fWide 读错位置"：fWide 在偏移 16，若从 12 去读，读到的就是
// fNC（0），于是 ANSI 载荷会被当成 UTF-16LE 解析成乱码（或者反过来）。
func TestParseDROPFILES_AsWrittenByExplorerANSI(t *testing.T) {
	paths := []string{"C:\\a.txt", "D:\\b c\\d.dat"}

	blob := handBuildDROPFILES(t, paths, 20 /* pFiles */, 0 /* fWide: ANSI */)

	df := (*dropFiles)(unsafe.Pointer(&blob[0]))
	if df.FWide != 0 {
		t.Fatalf("fWide 读成了 %d，应当是 0——说明读的位置不是偏移 16", df.FWide)
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

// explorerDROPFILES 按 Windows SDK 的布局、用**显式偏移**写一份 CF_HDROP。
//
// 刻意不复用 dropFiles / buildDROPFILES：这组字节是"外部世界（Explorer）
// 会写出来的东西"，用它才能发现"实现自己和自己的测试一起错"。
func explorerDROPFILES(t *testing.T, paths []string) []byte {
	t.Helper()

	const headerSize = 20 // ShlObj_core.h 的 sizeof(DROPFILES)
	blob := make([]byte, headerSize)
	// pFiles @0 = 20；pt @4..11 全 0；fNC @12..15 = 0；fWide @16..19 = 1。
	binary.LittleEndian.PutUint32(blob[0:4], headerSize)
	binary.LittleEndian.PutUint32(blob[16:20], 1)

	body := make([]byte, 0, 128)
	for _, p := range paths {
		body = append(body, encodeUTF16LE(p)...)
	}
	body = append(body, 0, 0) // 列表结束的双 NUL

	return append(blob, body...)
}

// handBuildDROPFILES 按 Win32 规范手写一份 CF_HDROP，刻意不复用 buildDROPFILES，
// 这样"被测实现"与"测试数据"才是两条独立的路径。
func handBuildDROPFILES(t *testing.T, paths []string, pFiles uint32, fWide int32) []byte {
	t.Helper()

	header := make([]byte, 20)
	// pFiles @0, pt @4..11（全 0）, fNC @12（0）, fWide @16
	binary.LittleEndian.PutUint32(header[0:4], pFiles)
	binary.LittleEndian.PutUint32(header[16:20], uint32(fWide))

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
