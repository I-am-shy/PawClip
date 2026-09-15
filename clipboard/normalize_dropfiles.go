package clipboard

import (
	"unicode/utf16"
	"unsafe"
)

// 本文件实现 CF_HDROP（文件列表）载荷的组装与解析。
//
// 为什么放在**无 build tag** 的文件里：这是纯字节运算，和操作系统 API 无关。
// 放在这里就能在 macOS 上直接跑单测（HANDOFF-PROMPT §五的明确要求），
// 而 Windows 后端只负责"把 HGLOBAL 里的字节递进来"。
//
// ⚠️ 这个文件的存在是有代价换来的：早期版本把 DROPFILES 结构体直接写在
// windows.go 里，结构体多了一个字段（`FNC int32`），于是
//   - sizeof = 20 而不是 16
//   - fWide 落在偏移 16 而不是 12
// 后果是双向的：读 Explorer 写的 CF_HDROP 会拿到错误的 fWide（wide 路径被
// 当成 ANSI 按 Latin-1 透传，中文路径直接变乱码），而我们写出去的 CF_HDROP
// 头部 pFiles=20，其它程序按规范从 16 开始读，第一个路径的字符会被吃掉。
// 抽成纯函数 + 单测之后，这类错误在 mac 上就会被拦住。

// DROPFILESHeaderSize 是 Win32 DROPFILES 结构体的大小（字节）。
//
//	typedef struct _DROPFILES {
//	  DWORD  pFiles;   // offset 0
//	  POINT  pt;       // offset 4  (两个 LONG，共 8 字节)
//	  BOOL   fWide;    // offset 12
//	} DROPFILES;       // sizeof = 16
//
// 字段顺序与偏移是对外契约的一部分：写错了不会在本进程内报错，只会和其它
// 程序（以及资源管理器）互相读不懂。normalize_dropfiles_test.go 会把它钉死。
const DROPFILESHeaderSize = 16

// winPoint 是 Win32 的 POINT（两个 int32）。
//
// 命名不叫 wndPoint 是因为它不只在窗口消息里用：DROPFILES 里也有一个。
type winPoint struct {
	X, Y int32
}

// dropFiles 是 Win32 DROPFILES 的内存布局。
//
// 字段必须与 C 定义**逐一对应**，不允许插入任何额外字段——
// Go 的结构体对齐恰好与 C 一致（都是 4 字节对齐），所以这里是安全的。
type dropFiles struct {
	PFiles uint32   // pFiles：载荷相对本结构体起点的字节偏移
	Pt     winPoint // pt：拖放发生时的鼠标位置（我们不用，但必须占位）
	FWide  int32    // fWide：非 0 表示路径是 UTF-16LE，否则是 ANSI
}

// encodeUTF16LE 把字符串编成 NUL 结尾的 UTF-16LE 字节串。
//
// NUL 终结符占两个字节（UTF-16 的一个 code unit），不是一字节。
func encodeUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(u16)*2+2)
	for _, v := range u16 {
		out = append(out, byte(v), byte(v>>8))
	}
	return append(out, 0, 0)
}

// buildDROPFILES 组装一份 CF_HDROP 载荷。
//
// 布局：16 字节头部 + 每个路径一段 NUL 结尾的 UTF-16LE + 一个额外的 NUL
// 作为列表结束符（即整体以"双 NUL"收尾）。
func buildDROPFILES(paths []string) []byte {
	header := make([]byte, DROPFILESHeaderSize)
	df := (*dropFiles)(unsafe.Pointer(&header[0]))
	df.PFiles = uint32(DROPFILESHeaderSize) // 载荷紧跟在头部之后
	df.FWide = 1                            // 一律 UTF-16LE，中文路径才不会丢

	body := make([]byte, 0, 64)
	for _, p := range paths {
		body = append(body, encodeUTF16LE(p)...)
	}
	// 每个路径已经自带一个 NUL 终结符，这里再补一个就是列表结束的双 NUL。
	body = append(body, 0, 0)

	return append(header, body...)
}

// parseDROPFILES 解析头部之后的载荷：一串 NUL 分隔、双 NUL 结束的路径。
//
// payload 是**已经按 pFiles 偏移切好**的字节（调用方负责切片）。
func parseDROPFILES(payload []byte, wide bool) []string {
	if wide {
		return parseDROPFILESWide(payload)
	}
	return parseDROPFILESANSI(payload)
}

func parseDROPFILESWide(payload []byte) []string {
	var (
		out []string
		cur []uint16
	)
	for i := 0; i+1 < len(payload); i += 2 {
		v := uint16(payload[i]) | uint16(payload[i+1])<<8
		if v != 0 {
			cur = append(cur, v)
			continue
		}
		if len(cur) == 0 {
			break // 双 NUL：列表结束
		}
		out = append(out, string(utf16.Decode(cur)))
		cur = cur[:0]
	}
	if len(cur) > 0 {
		// 载荷提前结束（没写双 NUL）：能拿到几个算几个，别整个丢掉
		out = append(out, string(utf16.Decode(cur)))
	}
	return out
}

// parseDROPFILESANSI 只在 fWide == 0 时才走。按字节透传：CF_HDROP 的 ANSI
// 变体用的是当前系统代码页，这里不做猜测性转码，避免把 UTF-8 路径改坏。
func parseDROPFILESANSI(payload []byte) []string {
	var (
		out []string
		cur []byte
	)
	for _, b := range payload {
		if b != 0 {
			cur = append(cur, b)
			continue
		}
		if len(cur) == 0 {
			break // 双 NUL：列表结束
		}
		out = append(out, string(cur))
		cur = cur[:0]
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}
