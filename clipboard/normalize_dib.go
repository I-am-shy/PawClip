package clipboard

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/bits"

	_ "golang.org/x/image/webp"
)

// 本文件是 Windows 专属逻辑里**纯计算**的那部分（docs/DESIGN.md §2 要求把它们抽成
// 无平台依赖的纯函数，好在 macOS 上直接单测）：
//
//	CF_DIBV5 → PNG（读取方向）
//	PNG → CF_DIBV5（回写方向：Windows 只有 DIB，其它 App 也只认 DIB）
//
// Windows 侧三个必做转换里的另外两个在 normalize_html.go（CF_HTML 头部剥离）
// 与 normalize.go（类型归并、去抖）。

// BITMAPINFOHEADER 及之后各版本头部长度。
const (
	dibHeaderInfoHeader = 40
	dibHeaderV4Header   = 108
	dibHeaderV5Header   = 124

	// 压缩方式（biCompression）
	biRGB            = 0
	biRLE8           = 1
	biRLE4           = 2
	biBitFields      = 3
	biJPEG           = 4
	biPNG            = 5
	biAlphaBitFields = 6

	// LCS_sRGB：'sRGB' 在内存里按小端存放就是 0x73524742。
	lcsSRGB = 0x73524742
)

// PNG 魔数。
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// JPEG SOI。
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// ImageFromPNG 校验 PNG 并取出像素尺寸；PNG 字节**原样保留**。
//
// 原样保留很关键：items.fingerprint 定义成存储内容的 sha256
// （docs/BACKUP-FORMAT.md §3.5 要求 fingerprint 与 blob 的 sha256 一致），
// 重新编码会改变字节、进而改变指纹、进而破坏 SelfWriteGuard。
// macOS 侧 public.png 直接走这条路。
func ImageFromPNG(data []byte) (*Image, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("clipboard: empty PNG payload")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("clipboard: decode PNG header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("clipboard: PNG has invalid size %dx%d", cfg.Width, cfg.Height)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return &Image{PNG: out, Width: cfg.Width, Height: cfg.Height}, nil
}

// ImageFromEncoded 把任意已注册编码（PNG / JPEG / WebP）转成 PNG。
func ImageFromEncoded(data []byte) (*Image, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("clipboard: empty image payload")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("clipboard: decode image: %w", err)
	}
	return encodePNG(img)
}

func encodePNG(img image.Image) (*Image, error) {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("clipboard: image has invalid size %dx%d", b.Dx(), b.Dy())
	}
	var buf bytes.Buffer
	buf.Grow(b.Dx() * b.Dy() / 2)
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("clipboard: encode PNG: %w", err)
	}
	return &Image{PNG: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

// DIBToPNG 把 Windows 剪贴板里的 CF_DIB / CF_DIBV5 字节流转成 PNG + 像素尺寸。
//
// 支持：BITMAPINFOHEADER / V4 / V5 头；BI_RGB、BI_BITFIELDS、
// BI_ALPHABITFIELDS；BI_PNG / BI_JPEG（DIB 里直接塞了图片流）；自顶向下与
// 自底向上；32 / 24 / 16 / 8 / 4 / 1 bpp；行尾 4 字节对齐补齐。
func DIBToPNG(dib []byte) (*Image, error) {
	if len(dib) < dibHeaderInfoHeader {
		return nil, fmt.Errorf("clipboard: DIB too short (%d bytes)", len(dib))
	}
	hdrSize := int(binaryLE32(dib, 0))
	if hdrSize < dibHeaderInfoHeader || hdrSize > len(dib) {
		return nil, fmt.Errorf("clipboard: DIB header size %d out of range (buffer %d)", hdrSize, len(dib))
	}
	rawWidth := int32(binaryLE32(dib, 4))
	rawHeight := int32(binaryLE32(dib, 8))
	planes := binaryLE16(dib, 12)
	bpp := int(binaryLE16(dib, 14))
	compression := binaryLE32(dib, 16)
	clrUsed := int(binaryLE32(dib, 32))

	if rawWidth <= 0 {
		return nil, fmt.Errorf("clipboard: DIB width %d invalid", rawWidth)
	}
	width := int(rawWidth)
	if rawHeight == 0 {
		return nil, fmt.Errorf("clipboard: DIB height 0 invalid")
	}
	if planes != 1 {
		return nil, fmt.Errorf("clipboard: DIB planes %d unsupported", planes)
	}
	topDown := rawHeight < 0
	height := int(rawHeight)
	if topDown {
		height = -height
	}

	// BI_PNG / BI_JPEG：头后面直接跟图片文件流。不同生产者会（或不）带
	// BITMAPFILEHEADER，所以按魔数搜索而不是按固定偏移。
	switch compression {
	case biPNG:
		if off := bytes.Index(dib, pngMagic); off >= 0 {
			return ImageFromEncoded(dib[off:])
		}
		return nil, fmt.Errorf("%w: BI_PNG DIB without PNG magic", ErrUnsupportedImage)
	case biJPEG:
		if off := bytes.Index(dib, jpegMagic); off >= 0 {
			return ImageFromEncoded(dib[off:])
		}
		return nil, fmt.Errorf("%w: BI_JPEG DIB without JPEG magic", ErrUnsupportedImage)
	case biRLE4, biRLE8:
		return nil, fmt.Errorf("%w: RLE-compressed DIB (biCompression=%d)", ErrUnsupportedImage, compression)
	}

	// 掩码位置：V4/V5 头里掩码在头部内偏移 40；40 字节头则紧跟其后。
	var rMask, gMask, bMask, aMask uint32
	maskBlock := 0
	switch compression {
	case biBitFields:
		maskBlock = 12
	case biAlphaBitFields:
		maskBlock = 16
	}
	if maskBlock > 0 {
		if hdrSize >= dibHeaderV4Header {
			// 头部自带掩码
			maskBlock = 0
			if len(dib) < 56 {
				return nil, fmt.Errorf("clipboard: DIB header truncated before masks")
			}
			rMask = binaryLE32(dib, 40)
			gMask = binaryLE32(dib, 44)
			bMask = binaryLE32(dib, 48)
			if compression == biAlphaBitFields || hdrSize >= 56 {
				aMask = binaryLE32(dib, 52)
			}
		} else {
			if len(dib) < hdrSize+maskBlock {
				return nil, fmt.Errorf("clipboard: DIB truncated before masks")
			}
			rMask = binaryLE32(dib, hdrSize)
			gMask = binaryLE32(dib, hdrSize+4)
			bMask = binaryLE32(dib, hdrSize+8)
			if maskBlock >= 16 {
				aMask = binaryLE32(dib, hdrSize+12)
			}
		}
	}

	// 调色板（仅 bpp <= 8 有）
	paletteEntries := 0
	if bpp <= 8 {
		paletteEntries = clrUsed
		if paletteEntries <= 0 {
			paletteEntries = 1 << uint(bpp)
		}
	}
	paletteOffset := hdrSize + maskBlock
	pixelOffset := paletteOffset + paletteEntries*4

	var palette color.Palette
	if paletteEntries > 0 {
		if len(dib) < pixelOffset {
			return nil, fmt.Errorf("clipboard: DIB truncated inside palette")
		}
		palette = make(color.Palette, paletteEntries)
		for i := 0; i < paletteEntries; i++ {
			o := paletteOffset + i*4
			palette[i] = color.RGBA{R: dib[o+2], G: dib[o+1], B: dib[o], A: 0xFF}
		}
	}

	// 默认掩码（BI_RGB 时按位深推导）
	if rMask == 0 && gMask == 0 && bMask == 0 {
		switch bpp {
		case 32, 24:
			rMask, gMask, bMask = 0x00FF0000, 0x0000FF00, 0x000000FF
			if bpp == 32 {
				aMask = 0xFF000000
			}
		case 16:
			rMask, gMask, bMask = 0x7C00, 0x03E0, 0x001F
		}
	}

	switch bpp {
	case 32, 24, 16, 8, 4, 1:
	default:
		return nil, fmt.Errorf("%w: %d bpp DIB", ErrUnsupportedImage, bpp)
	}

	rowBytes := ((width*int(bpp) + 31) / 32) * 4
	if rowBytes <= 0 {
		return nil, fmt.Errorf("clipboard: DIB row size invalid (width=%d bpp=%d)", width, bpp)
	}
	if len(dib) < pixelOffset {
		return nil, fmt.Errorf("clipboard: DIB truncated before pixel data (need %d, have %d)", pixelOffset, len(dib))
	}
	avail := len(dib) - pixelOffset
	if avail < rowBytes {
		return nil, fmt.Errorf("clipboard: DIB has no complete pixel row (%d bytes available)", avail)
	}
	if rows := avail / rowBytes; rows < height {
		// 有些生产者尾部被截断，能解多少算多少（与 Windows 自带解码器行为一致）
		height = rows
	}

	rShift, rMax, rOK := maskGeom(rMask)
	gShift, gMax, gOK := maskGeom(gMask)
	bShift, bMax, bOK := maskGeom(bMask)
	aShift, aMax, aOK := maskGeom(aMask)

	// 32bpp 的一个真实世界怪癖：alpha 通道整片为 0 时表示"不透明"，
	// 直接当透明会得到一张全透明的图。这在 CF_DIBV5（BI_BITFIELDS）里最常见，
	// 所以判据是"有 alpha 通道"而不是"压缩方式是 BI_RGB"。
	alphaIsOpaque := false
	if bpp == 32 && aOK {
		alphaIsOpaque = allZeroAlpha(dib[pixelOffset:pixelOffset+rowBytes*height], rowBytes, height)
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		src := y
		if !topDown {
			src = height - 1 - y
		}
		row := dib[pixelOffset+src*rowBytes : pixelOffset+src*rowBytes+rowBytes]
		dstOff := y * img.Stride
		dst := img.Pix[dstOff : dstOff+width*4]

		switch bpp {
		case 32, 24, 16:
			for x := 0; x < width; x++ {
				var v uint64
				switch bpp {
				case 32:
					v = uint64(binaryLE32(row, x*4))
				case 24:
					o := x * 3
					v = uint64(row[o]) | uint64(row[o+1])<<8 | uint64(row[o+2])<<16
				case 16:
					v = uint64(binaryLE16(row, x*2))
				}
				var r, g, b, a uint8
				if rOK {
					r = expandChannel((v&uint64(rMask))>>rShift, rMax)
				}
				if gOK {
					g = expandChannel((v&uint64(gMask))>>gShift, gMax)
				}
				if bOK {
					b = expandChannel((v&uint64(bMask))>>bShift, bMax)
				}
				switch {
				case alphaIsOpaque:
					a = 0xFF
				case aOK:
					a = expandChannel((v&uint64(aMask))>>aShift, aMax)
				case bpp == 32:
					// 32bpp 但没给 alpha 掩码：取高位字节
					a = uint8(v >> 24)
				default:
					a = 0xFF
				}
				o := x * 4
				dst[o], dst[o+1], dst[o+2], dst[o+3] = r, g, b, a
			}
		default:
			// 1 / 4 / 8 bpp 调色板
			for x := 0; x < width; x++ {
				idx := paletteIndex(row, x, bpp)
				var cr, cg, cb, ca uint8 = 0, 0, 0, 0xFF
				if idx < len(palette) {
					r16, g16, b16, a16 := palette[idx].RGBA()
					cr, cg, cb, ca = uint8(r16>>8), uint8(g16>>8), uint8(b16>>8), uint8(a16>>8)
				}
				o := x * 4
				dst[o], dst[o+1], dst[o+2], dst[o+3] = cr, cg, cb, ca
			}
		}
	}
	return encodePNG(img)
}

// PNGToDIBV5 把 PNG 转成 32bpp 自顶向下的 BITMAPV5HEADER DIB。
//
// 回写方向用：Windows 的剪贴板图片就是 DIB，不转的话别的 App 拿不到图。
// 头部无调色板，像素紧跟在 124 字节头之后。
func PNGToDIBV5(pngBytes []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("clipboard: decode PNG for DIB: %w", err)
	}
	nrgba := toNRGBA(src)
	w, h := nrgba.Bounds().Dx(), nrgba.Bounds().Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("clipboard: image has invalid size %dx%d", w, h)
	}

	out := make([]byte, dibHeaderV5Header+w*h*4)
	// BITMAPV5HEADER
	putLE32(out, 0, dibHeaderV5Header) // bV5Size
	putLE32(out, 4, uint32(int32(w)))  // bV5Width
	putLE32(out, 8, uint32(int32(-h))) // bV5Height：负数 = 自顶向下
	putLE16(out, 12, 1)                // bV5Planes
	putLE16(out, 14, 32)               // bV5BitCount
	putLE32(out, 16, biBitFields)      // bV5Compression
	putLE32(out, 20, uint32(w*h*4))    // bV5SizeImage
	putLE32(out, 40, 0x00FF0000)       // bV5RedMask
	putLE32(out, 44, 0x0000FF00)       // bV5GreenMask
	putLE32(out, 48, 0x000000FF)       // bV5BlueMask
	putLE32(out, 52, 0xFF000000)       // bV5AlphaMask
	putLE32(out, 56, lcsSRGB)          // bV5CSType

	// 像素：BGRA，无行尾补齐（32bpp 天然 4 字节对齐）
	pix := out[dibHeaderV5Header:]
	for y := 0; y < h; y++ {
		srcRow := nrgba.Pix[y*nrgba.Stride : y*nrgba.Stride+w*4]
		dstRow := pix[y*w*4 : (y+1)*w*4]
		for x := 0; x < w; x++ {
			o := x * 4
			dstRow[o] = srcRow[o+2]   // B
			dstRow[o+1] = srcRow[o+1] // G
			dstRow[o+2] = srcRow[o]   // R
			dstRow[o+3] = srcRow[o+3] // A
		}
	}
	return out, nil
}

func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	b := img.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst
}

// paletteIndex 从调色板行里取第 x 个像素的索引。
func paletteIndex(row []byte, x, bpp int) int {
	switch bpp {
	case 8:
		if x >= len(row) {
			return 0
		}
		return int(row[x])
	case 4:
		i := x / 2
		if i >= len(row) {
			return 0
		}
		if x%2 == 0 {
			return int(row[i] >> 4)
		}
		return int(row[i] & 0x0F)
	default: // 1
		i := x / 8
		if i >= len(row) {
			return 0
		}
		return int((row[i] >> uint(7-x%8)) & 1)
	}
}

func allZeroAlpha(data []byte, rowBytes, height int) bool {
	if len(data) < rowBytes*height {
		return false
	}
	for y := 0; y < height; y++ {
		row := data[y*rowBytes : y*rowBytes+rowBytes]
		for i := 3; i < len(row); i += 4 {
			if row[i] != 0 {
				return false
			}
		}
	}
	return true
}

func maskGeom(mask uint32) (shift uint, max uint64, ok bool) {
	if mask == 0 {
		return 0, 0, false
	}
	s := uint(bits.TrailingZeros32(mask))
	return s, uint64(mask >> s), true
}

func expandChannel(v uint64, max uint64) uint8 {
	if max == 0 {
		return 0
	}
	return uint8((v*255 + max/2) / max)
}

func binaryLE32(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

func binaryLE16(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

func putLE32(b []byte, off int, v uint32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}

func putLE16(b []byte, off int, v uint16) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
}

// 让 jpeg 包被显式引用：DIB 的 BI_JPEG 分支与 image.Decode 都依赖它注册。
var _ = jpeg.Decode
