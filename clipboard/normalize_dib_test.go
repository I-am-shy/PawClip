package clipboard

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// ── DIB 构造器 ──────────────────────────────────────────────────
//
// macOS 上没法用系统 API 生成 CF_DIBV5，而 Windows 后端又必须在 mac 上单测。
// 所以用例由构造器按 Windows 的 DIB 规范拼出真实字节流：行按 4 字节对齐补齐、
// 默认自底向上存储、头部字段一个不少。构造器本身是本文件的被测对象之外的东西，
// 因此它只做"按规范拼字节"，不做任何归一化。

type dibSpec struct {
	W, H        int
	Bpp         int
	TopDown     bool
	Compression uint32
	HdrSize     int
	// ClrUsed 对应 biClrUsed：调色板条数。8bpp 不写的话按规范要读 256 条。
	ClrUsed int
	Masks   [4]uint32 // r, g, b, a
	Palette []byte    // RGBQUAD 序列（B,G,R,0）
	// Rows[0] 是图像**顶部**那一行；是否自底向上由 TopDown 决定。
	Rows [][]byte
}

func buildDIB(s dibSpec) []byte {
	hdrSize := s.HdrSize
	if hdrSize == 0 {
		hdrSize = dibHeaderV5Header
	}
	rowBytes := ((s.W*s.Bpp + 31) / 32) * 4

	out := make([]byte, hdrSize)
	putLE32(out, 0, uint32(hdrSize))
	putLE32(out, 4, uint32(int32(s.W)))
	hh := int32(s.H)
	if s.TopDown {
		hh = -hh
	}
	putLE32(out, 8, uint32(hh))
	putLE16(out, 12, 1)
	putLE16(out, 14, uint16(s.Bpp))
	putLE32(out, 16, s.Compression)
	putLE32(out, 20, uint32(rowBytes*s.H))
	putLE32(out, 32, uint32(s.ClrUsed))

	if hdrSize >= dibHeaderV4Header {
		putLE32(out, 40, s.Masks[0])
		putLE32(out, 44, s.Masks[1])
		putLE32(out, 48, s.Masks[2])
		if hdrSize >= 56 {
			putLE32(out, 52, s.Masks[3])
		}
	} else {
		// 40 字节头：掩码紧跟头部，视压缩方式决定 3 个还是 4 个
		n := 0
		switch s.Compression {
		case biBitFields:
			n = 3
		case biAlphaBitFields:
			n = 4
		}
		for i := 0; i < n; i++ {
			var b [4]byte
			putLE32(b[:], 0, s.Masks[i])
			out = append(out, b[:]...)
		}
	}

	out = append(out, s.Palette...)

	// 补齐到 rowBytes，再按上下顺序写入
	rows := make([][]byte, s.H)
	for y := 0; y < s.H; y++ {
		row := make([]byte, rowBytes)
		if y < len(s.Rows) {
			copy(row, s.Rows[y])
		}
		rows[y] = row
	}
	if !s.TopDown {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// bgRow32 生成一行 32bpp BGRA 像素。
func bgRow32(px ...[4]byte) []byte {
	out := make([]byte, 0, len(px)*4)
	for _, p := range px {
		out = append(out, p[0], p[1], p[2], p[3])
	}
	return out
}

func decodePNGImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码产物 PNG 失败：%v", err)
	}
	return img
}

func nrgbaAt(t *testing.T, img image.Image, x, y int) color.NRGBA {
	t.Helper()
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func assertPixel(t *testing.T, img image.Image, x, y int, want color.NRGBA) {
	t.Helper()
	got := nrgbaAt(t, img, x, y)
	if got != want {
		t.Errorf("像素 (%d,%d) = %+v, want %+v", x, y, got, want)
	}
}

// ── 32bpp ───────────────────────────────────────────────────────

func TestDIBToPNG_32bppBitFieldsBottomUp(t *testing.T) {
	// 标准 V5 掩码（就是 BGRA 字节序）
	red := [4]byte{0x00, 0x00, 0xFF, 0xFF}
	green := [4]byte{0x00, 0xFF, 0x00, 0xFF}
	blue := [4]byte{0xFF, 0x00, 0x00, 0xFF}
	// bgRow32 按 BGRA 字节序写，{B,G,R,A} = {0x10,0x20,0x30,0x80}，
	// 所以读回来应是 NRGBA{0x30,0x20,0x10,0x80}——半透明，验证 alpha 没被丢
	semi := [4]byte{0x10, 0x20, 0x30, 0x80}

	dib := buildDIB(dibSpec{
		W: 2, H: 2, Bpp: 32, Compression: biBitFields,
		Masks: [4]uint32{0x00FF0000, 0x0000FF00, 0x000000FF, 0xFF000000},
		Rows: [][]byte{
			bgRow32(red, green),
			bgRow32(blue, semi),
		},
	})

	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	if got.Width != 2 || got.Height != 2 {
		t.Fatalf("尺寸 = %dx%d, want 2x2", got.Width, got.Height)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{G: 255, A: 255})
	assertPixel(t, img, 0, 1, color.NRGBA{B: 255, A: 255})
	assertPixel(t, img, 1, 1, color.NRGBA{R: 0x30, G: 0x20, B: 0x10, A: 0x80})
}

func TestDIBToPNG_32bppTopDown(t *testing.T) {
	top := [4]byte{0xFF, 0x00, 0x00, 0xFF}    // 蓝
	bottom := [4]byte{0x00, 0xFF, 0x00, 0xFF} // 绿
	dib := buildDIB(dibSpec{
		W: 1, H: 2, Bpp: 32, TopDown: true, Compression: biRGB,
		Rows: [][]byte{bgRow32(top), bgRow32(bottom)},
	})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	// 自顶向下第一行就是第一行像素；写反了这里会红/绿互换
	assertPixel(t, img, 0, 0, color.NRGBA{B: 255, A: 255})
	assertPixel(t, img, 0, 1, color.NRGBA{G: 255, A: 255})
}

func TestDIBToPNG_32bppAllZeroAlphaBecomesOpaque(t *testing.T) {
	// 真实世界里极常见：CF_DIBV5 带 alpha 掩码，但 alpha 整片是 0。
	// 当成透明会得到一张全透明的图。
	px := [4]byte{0x11, 0x22, 0x33, 0x00}
	dib := buildDIB(dibSpec{
		W: 2, H: 1, Bpp: 32, Compression: biBitFields,
		Masks: [4]uint32{0x00FF0000, 0x0000FF00, 0x000000FF, 0xFF000000},
		Rows:  [][]byte{bgRow32(px, px)},
	})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 0x33, G: 0x22, B: 0x11, A: 0xFF})
}

// ── 24bpp ───────────────────────────────────────────────────────

func TestDIBToPNG_24bppRowPadding(t *testing.T) {
	// 宽度 3 → 每行 9 字节 → 必须补齐到 12 字节。不处理补齐会整行错位。
	row0 := []byte{
		0x01, 0x02, 0x03, // 像素 0：B1 G2 R3
		0x11, 0x12, 0x13, // 像素 1
		0x21, 0x22, 0x23, // 像素 2
	}
	row1 := []byte{
		0x31, 0x32, 0x33,
		0x41, 0x42, 0x43,
		0x51, 0x52, 0x53,
	}
	dib := buildDIB(dibSpec{W: 3, H: 2, Bpp: 24, Compression: biRGB, Rows: [][]byte{row0, row1}})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	if got.Width != 3 || got.Height != 2 {
		t.Fatalf("尺寸 = %dx%d", got.Width, got.Height)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 0x03, G: 0x02, B: 0x01, A: 0xFF})
	assertPixel(t, img, 2, 0, color.NRGBA{R: 0x23, G: 0x22, B: 0x21, A: 0xFF})
	// 第二行本来是底部，自底向上存储后应出现在输出第 1 行（y=1）
	assertPixel(t, img, 0, 1, color.NRGBA{R: 0x33, G: 0x32, B: 0x31, A: 0xFF})
	assertPixel(t, img, 2, 1, color.NRGBA{R: 0x53, G: 0x52, B: 0x51, A: 0xFF})
}

func TestDIBToPNG_24bppTopDown(t *testing.T) {
	row0 := []byte{0x01, 0x01, 0x01}
	row1 := []byte{0x02, 0x02, 0x02}
	dib := buildDIB(dibSpec{W: 1, H: 2, Bpp: 24, TopDown: true, Compression: biRGB,
		Rows: [][]byte{row0, row1}})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 1, G: 1, B: 1, A: 255})
	assertPixel(t, img, 0, 1, color.NRGBA{R: 2, G: 2, B: 2, A: 255})
}

// ── 16bpp ───────────────────────────────────────────────────────

func TestDIBToPNG_16bpp565Masks(t *testing.T) {
	// BI_BITFIELDS + 5-6-5 掩码
	row := make([]byte, 0, 8)
	put := func(v uint16) { row = append(row, byte(v), byte(v>>8)) }
	put(0xF800) // 纯红
	put(0x07E0) // 纯绿
	put(0x001F) // 纯蓝
	put(0x0000) // 黑
	dib := buildDIB(dibSpec{
		W: 4, H: 1, Bpp: 16, Compression: biBitFields,
		Masks: [4]uint32{0xF800, 0x07E0, 0x001F, 0},
		Rows:  [][]byte{row},
	})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{G: 255, A: 255})
	assertPixel(t, img, 2, 0, color.NRGBA{B: 255, A: 255})
	assertPixel(t, img, 3, 0, color.NRGBA{A: 255})
}

func TestDIBToPNG_16bppRGBDefault555(t *testing.T) {
	row := []byte{0x00, 0x7C} // 0x7C00 = 555 的全红
	dib := buildDIB(dibSpec{W: 1, H: 1, Bpp: 16, Compression: biRGB, Rows: [][]byte{row}})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
}

// ── 调色板 ──────────────────────────────────────────────────────

func TestDIBToPNG_8bppPalette(t *testing.T) {
	palette := []byte{
		0x00, 0x00, 0x00, 0x00, // 0 → 黑
		0x00, 0x00, 0xFF, 0x00, // 1 → 红
		0xFF, 0x00, 0x00, 0x00, // 2 → 蓝
	}
	// 8bpp：宽度 3 → 3 字节补齐到 4。biClrUsed 必须写对：不写的话
	// 规范要求读满 1<<8 = 256 条调色板。
	row := []byte{2, 1, 0, 0xCC}
	dib := buildDIB(dibSpec{W: 3, H: 1, Bpp: 8, Compression: biRGB, ClrUsed: 3,
		Palette: palette, Rows: [][]byte{row}})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{B: 255, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{R: 255, A: 255})
	assertPixel(t, img, 2, 0, color.NRGBA{A: 255})
}

func TestDIBToPNG_4bppAnd1bppPalette(t *testing.T) {
	palette := []byte{
		0x00, 0x00, 0x00, 0x00, // 0 → 黑
		0x00, 0x00, 0xFF, 0x00, // 1 → 红
	}
	// 4bpp：字节 0x10 表示像素 1 与 0
	dib := buildDIB(dibSpec{W: 2, H: 1, Bpp: 4, Compression: biRGB, ClrUsed: 2,
		Palette: palette, Rows: [][]byte{{0x10}}})
	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("4bpp DIBToPNG: %v", err)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{A: 255})

	// 1bpp：字节 0x80 → 像素 0 为 1，其余为 0
	dib = buildDIB(dibSpec{W: 2, H: 1, Bpp: 1, Compression: biRGB, ClrUsed: 2,
		Palette: palette, Rows: [][]byte{{0x80}}})
	got, err = DIBToPNG(dib)
	if err != nil {
		t.Fatalf("1bpp DIBToPNG: %v", err)
	}
	img = decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{A: 255})
}

// ── 内嵌 PNG / 异常输入 ─────────────────────────────────────────

func TestDIBToPNG_EmbeddedPNGCompression(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{R: 4, G: 5, B: 6, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}

	// BI_PNG：头部之后直接跟 PNG 流
	hdr := buildDIB(dibSpec{W: 2, H: 1, Bpp: 32, Compression: biPNG, Rows: nil})
	dib := append(hdr, buf.Bytes()...)

	got, err := DIBToPNG(dib)
	if err != nil {
		t.Fatalf("DIBToPNG(BI_PNG): %v", err)
	}
	if got.Width != 2 || got.Height != 1 {
		t.Fatalf("尺寸 = %dx%d", got.Width, got.Height)
	}
	img := decodePNGImage(t, got.PNG)
	assertPixel(t, img, 0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	assertPixel(t, img, 1, 0, color.NRGBA{R: 4, G: 5, B: 6, A: 255})
}

func TestDIBToPNG_Errors(t *testing.T) {
	cases := []struct {
		name string
		dib  []byte
	}{
		{"空", nil},
		{"比 BITMAPINFOHEADER 还短", make([]byte, 20)},
		{"头部长度超出缓冲", func() []byte {
			b := make([]byte, 64)
			putLE32(b, 0, 124) // 声称 124 字节头，但只有 64
			return b
		}()},
		{"宽度为 0", func() []byte {
			b := make([]byte, dibHeaderV5Header)
			putLE32(b, 0, dibHeaderV5Header)
			putLE32(b, 4, 0)
			putLE32(b, 8, 4)
			putLE16(b, 12, 1)
			putLE16(b, 14, 32)
			return b
		}()},
		{"高度为 0", func() []byte {
			b := make([]byte, dibHeaderV5Header)
			putLE32(b, 0, dibHeaderV5Header)
			putLE32(b, 4, 4)
			putLE32(b, 8, 0)
			putLE16(b, 12, 1)
			putLE16(b, 14, 32)
			return b
		}()},
		{"不支持的位深 3bpp", func() []byte {
			b := make([]byte, dibHeaderV5Header+16)
			putLE32(b, 0, dibHeaderV5Header)
			putLE32(b, 4, 1)
			putLE32(b, 8, 1)
			putLE16(b, 12, 1)
			putLE16(b, 14, 3)
			return b
		}()},
		{"RLE 压缩", func() []byte {
			b := make([]byte, dibHeaderV5Header+16)
			putLE32(b, 0, dibHeaderV5Header)
			putLE32(b, 4, 1)
			putLE32(b, 8, 1)
			putLE16(b, 12, 1)
			putLE16(b, 14, 8)
			putLE32(b, 16, biRLE8)
			return b
		}()},
		{"像素数据不足一行", func() []byte {
			b := make([]byte, dibHeaderV5Header) // 头之后一个字节都没有
			putLE32(b, 0, dibHeaderV5Header)
			putLE32(b, 4, 8)
			putLE32(b, 8, 8)
			putLE16(b, 12, 1)
			putLE16(b, 14, 32)
			return b
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DIBToPNG(c.dib); err == nil {
				t.Fatal("期望报错，却成功了")
			}
		})
	}
}

func TestDIBToPNG_TruncatedTailKeepsCompleteRows(t *testing.T) {
	// 尾部被截断时，能解多少行解多少行，而不是整体失败
	full := buildDIB(dibSpec{
		W: 1, H: 4, Bpp: 32, Compression: biRGB,
		Rows: [][]byte{
			{0, 0, 0, 0xFF}, {0, 0, 0, 0xFF}, {0, 0, 0, 0xFF}, {0, 0, 0, 0xFF},
		},
	})
	truncated := full[:dibHeaderV5Header+8] // 只剩完整的两行
	got, err := DIBToPNG(truncated)
	if err != nil {
		t.Fatalf("DIBToPNG: %v", err)
	}
	if got.Height != 2 {
		t.Fatalf("高度 = %d, want 2", got.Height)
	}
}

// ── 回写方向：PNG → DIBV5 ───────────────────────────────────────

func TestPNGToDIBV5_HeaderAndLayout(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 10), G: uint8(y * 20), B: 7, A: 200})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}

	dib, err := PNGToDIBV5(buf.Bytes())
	if err != nil {
		t.Fatalf("PNGToDIBV5: %v", err)
	}
	if len(dib) != dibHeaderV5Header+3*2*4 {
		t.Fatalf("DIB 长度 = %d, want %d", len(dib), dibHeaderV5Header+3*2*4)
	}
	if got := binaryLE32(dib, 0); got != dibHeaderV5Header {
		t.Errorf("biSize = %d", got)
	}
	if got := int32(binaryLE32(dib, 4)); got != 3 {
		t.Errorf("biWidth = %d", got)
	}
	if got := int32(binaryLE32(dib, 8)); got != -2 {
		t.Errorf("biHeight = %d, want -2（自顶向下）", got)
	}
	if got := binaryLE16(dib, 14); got != 32 {
		t.Errorf("biBitCount = %d", got)
	}
	if got := binaryLE32(dib, 16); got != biBitFields {
		t.Errorf("biCompression = %d, want BI_BITFIELDS", got)
	}
	if got := binaryLE32(dib, 52); got != 0xFF000000 {
		t.Errorf("alpha 掩码 = %#x", got)
	}
	// 第一个像素必须是 BGRA
	if dib[dibHeaderV5Header] != 7 {
		t.Errorf("首像素 B = %d, want 7", dib[dibHeaderV5Header])
	}
}

func TestPNGToDIBV5_RoundTripIsFixedPoint(t *testing.T) {
	// 这是 Windows 自写入守卫的数学前提之一：PNG → DIB → PNG 必须是同一个字节流，
	// 否则回写后读回来的指纹与 Arm 时不同，SelfWriteGuard 会失效。
	// （另外 Windows 侧回写时还会额外写一个注册格式 "PNG"，两条保险。）
	for _, alpha := range []uint8{255, 200, 0} {
		src := image.NewNRGBA(image.Rect(0, 0, 5, 4))
		for y := 0; y < 4; y++ {
			for x := 0; x < 5; x++ {
				src.SetNRGBA(x, y, color.NRGBA{
					R: uint8(x * 13), G: uint8(y * 29), B: uint8((x + y) * 7), A: alpha,
				})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		original := buf.Bytes()

		// alpha 全 0 时，DIB 解码会按 Windows 惯例判成"不透明"，
		// 这种情况下固定点不成立——这是规范允许的语义差异，不是 bug。
		dib, err := PNGToDIBV5(original)
		if err != nil {
			t.Fatalf("PNGToDIBV5: %v", err)
		}
		back, err := DIBToPNG(dib)
		if err != nil {
			t.Fatalf("DIBToPNG: %v", err)
		}
		if alpha == 0 {
			if bytes.Equal(back.PNG, original) {
				t.Fatal("全 0 alpha 应被解释为不透明，不应与原图字节相同")
			}
			continue
		}
		if !bytes.Equal(back.PNG, original) {
			t.Fatalf("alpha=%d 时 PNG→DIB→PNG 不是固定点（原始 %d 字节，回来 %d 字节）",
				alpha, len(original), len(back.PNG))
		}
	}
}

func TestImageFromPNG_PreservesBytes(t *testing.T) {
	// macOS 的 public.png 必须原样保留字节：fingerprint 就是这些字节的 sha256，
	// 重新编码会改变指纹并破坏自写入守卫。
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	got, err := ImageFromPNG(buf.Bytes())
	if err != nil {
		t.Fatalf("ImageFromPNG: %v", err)
	}
	if !bytes.Equal(got.PNG, buf.Bytes()) {
		t.Fatal("PNG 字节被改写了")
	}
	if got.Width != 2 || got.Height != 2 {
		t.Fatalf("尺寸 = %dx%d", got.Width, got.Height)
	}
	if _, err := ImageFromPNG([]byte("not a png")); err == nil {
		t.Fatal("非 PNG 数据应报错")
	}
	if _, err := ImageFromPNG(nil); err == nil {
		t.Fatal("空数据应报错")
	}
}
