package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nfnt/resize"
)

// BlobStore 管理 blobs/ 下的内容寻址文件。
//
// 布局（DESIGN.md §5.3）：
//
//	blobs/9f/2a/9f2a1c…e4.png         sha256 前 2 位 / 第 3-4 位 两级分片
//	blobs/9f/2a/9f2a1c…e4.thumb.png   派生缩略图，长边 ≤ 160
//
// 二级分片的意义：最多 65536 个目录，避免单目录堆到万级文件后，
// Windows 上目录枚举明显变慢。
type BlobStore struct {
	root string
}

// ThumbMaxEdge 是缩略图长边上限（DESIGN.md §4.1 的 thumb_path 注释、§14 第 15 条）。
const ThumbMaxEdge = 160

// blob 目录与文件权限。收紧到 0700 / 0600 是 BACKUP-FORMAT.md §9 的要求。
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// NewBlobStore 准备 blob 根目录（不存在则创建，权限 0700）。
func NewBlobStore(root string) (*BlobStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("store: empty blob root")
	}
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("store: create blob root %s: %w", root, err)
	}
	return &BlobStore{root: root}, nil
}

// Root 返回 blob 根目录绝对路径。
func (b *BlobStore) Root() string { return b.root }

// Abs 把数据库里存的相对路径（相对 blobs/）转成绝对路径。
func (b *BlobStore) Abs(rel string) string {
	return filepath.Join(b.root, filepath.FromSlash(rel))
}

// SafeRel 校验相对路径。数据库里的路径理论上都是我们自己写的，但
// blob 路径也可能来自导入包，所以读取前一律校验，拒绝穿越与绝对路径。
func SafeRel(rel string) error {
	if rel == "" {
		return errors.New("store: empty blob path")
	}
	if strings.ContainsRune(rel, 0) {
		return fmt.Errorf("store: blob path contains NUL: %q", rel)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return fmt.Errorf("store: blob path is absolute: %q", rel)
	}
	if strings.Contains(rel, ":") {
		return fmt.Errorf("store: blob path contains volume separator: %q", rel)
	}
	for _, seg := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." || seg == "." {
			return fmt.Errorf("store: blob path escapes root: %q", rel)
		}
	}
	return nil
}

// ShardPath 由 sha256 十六进制串与扩展名算出两级分片相对路径。
//
// 例：("9f2a1c…e4", "png") → "9f/2a/9f2a1c…e4.png"
func ShardPath(shaHex, ext string) (string, error) {
	sha := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(shaHex), "sha256:"))
	if len(sha) != 64 || !isHex(sha) {
		return "", fmt.Errorf("store: invalid sha256 %q", shaHex)
	}
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if !isToken(ext) {
		return "", fmt.Errorf("store: invalid blob extension %q", ext)
	}
	return fmt.Sprintf("%s/%s/%s.%s", sha[0:2], sha[2:4], sha, ext), nil
}

// ThumbRel 由原图相对路径推出缩略图相对路径：
// "9f/2a/xxx.png" → "9f/2a/xxx.thumb.png"
func ThumbRel(rel string) (string, error) {
	if err := SafeRel(rel); err != nil {
		return "", err
	}
	ext := filepath.Ext(rel)
	if ext == "" {
		return "", fmt.Errorf("store: blob path without extension: %q", rel)
	}
	return strings.TrimSuffix(rel, ext) + ".thumb" + ext, nil
}

// Put 内容寻址写入：计算 sha256，按分片路径落盘，返回相对路径与 sha256 十六进制。
// 目标已存在时直接复用（内容寻址天然幂等）。
func (b *BlobStore) Put(data []byte, ext string) (rel, shaHex string, err error) {
	if len(data) == 0 {
		return "", "", errors.New("store: refusing to store empty blob")
	}
	sum := sha256.Sum256(data)
	shaHex = hex.EncodeToString(sum[:])
	rel, err = ShardPath(shaHex, ext)
	if err != nil {
		return "", "", err
	}
	if err := b.putAt(rel, data); err != nil {
		return "", "", err
	}
	return rel, shaHex, nil
}

// PutThumb 由原图 PNG 生成缩略图并落盘，返回相对路径与缩略图尺寸。
//
// 同步生成（DESIGN.md §14 第 15 条）：异步生成会让列表出现缩略图空窗，
// 体验明显变差。10 MB 图约 20–40 ms，可以接受。
func (b *BlobStore) PutThumb(pngBytes []byte, shaHex string) (rel string, w, h int, err error) {
	thumb, w, h, err := MakeThumbPNG(pngBytes, ThumbMaxEdge)
	if err != nil {
		return "", 0, 0, err
	}
	base, err := ShardPath(shaHex, "png")
	if err != nil {
		return "", 0, 0, err
	}
	rel, err = ThumbRel(base)
	if err != nil {
		return "", 0, 0, err
	}
	if err := b.putAt(rel, thumb); err != nil {
		return "", 0, 0, err
	}
	return rel, w, h, nil
}

// MakeThumbPNG 生成长边不超过 maxEdge 的 PNG 缩略图。
// 源图本身就小于上限时原样重编码（不放大）。
func MakeThumbPNG(pngBytes []byte, maxEdge int) ([]byte, int, int, error) {
	if maxEdge <= 0 {
		maxEdge = ThumbMaxEdge
	}
	src, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: decode PNG for thumbnail: %w", err)
	}
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, 0, 0, fmt.Errorf("store: PNG has invalid size %dx%d", bounds.Dx(), bounds.Dy())
	}
	thumb := resize.Thumbnail(uint(maxEdge), uint(maxEdge), src, resize.Lanczos3)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, thumb); err != nil {
		return nil, 0, 0, fmt.Errorf("store: encode thumbnail: %w", err)
	}
	tb := thumb.Bounds()
	return buf.Bytes(), tb.Dx(), tb.Dy(), nil
}

// putAt 把内容写到相对路径。已存在则跳过；否则临时文件 + fsync + rename 原子替换。
func (b *BlobStore) putAt(rel string, data []byte) error {
	if err := SafeRel(rel); err != nil {
		return err
	}
	abs := b.Abs(rel)
	if st, err := os.Stat(abs); err == nil && !st.IsDir() && st.Size() == int64(len(data)) {
		// 内容寻址：路径即哈希，同路径必然同内容，直接复用
		return nil
	}
	return WriteFileAtomic(abs, data, filePerm)
}

// WriteFileAtomic 用「同目录临时文件 + fsync + rename」写文件。
//
// 为什么要 fsync 临时文件再 rename：rename 本身是原子的，但不 fsync 的话
// 崩溃后可能出现"新文件名 + 旧内容"甚至空文件。先 fsync 内容、再 rename、
// 最后 fsync 目录，才能保证崩溃后看到的要么是完整旧文件、要么是完整新文件。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: create blob shard %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: create temp blob in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("store: write temp blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("store: fsync temp blob: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("store: chmod temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: close temp blob: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: rename temp blob: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Get 读取 blob 内容。
func (b *BlobStore) Get(rel string) ([]byte, error) {
	if err := SafeRel(rel); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(b.Abs(rel))
	if err != nil {
		return nil, fmt.Errorf("store: read blob %s: %w", rel, err)
	}
	return data, nil
}

// Exists 判断 blob 是否存在。
func (b *BlobStore) Exists(rel string) bool {
	if err := SafeRel(rel); err != nil {
		return false
	}
	st, err := os.Stat(b.Abs(rel))
	return err == nil && !st.IsDir()
}

// Remove 删除 blob 与其缩略图（缺一不可，否则会留孤儿缩略图）。
func (b *BlobStore) Remove(rel string) error {
	if err := SafeRel(rel); err != nil {
		return err
	}
	if err := os.Remove(b.Abs(rel)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove blob %s: %w", rel, err)
	}
	if thumb, err := ThumbRel(rel); err == nil {
		if err := os.Remove(b.Abs(thumb)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: remove thumb %s: %w", thumb, err)
		}
	}
	return nil
}

// BlobInfo 是 Walk 回调看到的一个 blob 文件。
type BlobInfo struct {
	// Rel 是相对 blobs/ 的**斜杠分隔**路径，与数据库里存的形式一致。
	// 这一点很关键：GC 拿它与 AllBlobRefs 的集合直接比对，
	// 若这里返回 OS 原生分隔符（Windows 上是 `\`），比对会全部落空、
	// 于是把所有 blob 都当孤儿删掉。
	Rel string
	// Abs 是绝对路径（删除时用）。
	Abs string
	// Size 是字节数。
	Size int64
	// ModTime 是修改时间（孤儿判定的"mtime > 1 天"用它）。
	ModTime time.Time
}

// Walk 遍历 blobs/ 下的全部文件（不递归进隐藏目录之外的任何特殊处理）。
//
// 回调返回非 nil 错误会中止遍历并把它透传出去；回调可以返回 fs.SkipAll
// 之类的哨兵，但那属于调用方的自由——本函数不解释错误语义。
func (b *BlobStore) Walk(fn func(BlobInfo) error) error {
	err := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个目录读不了不该让整轮 GC 失败（权限、竞态删除都可能），
			// 跳过继续走。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 刚被删掉/改名的竞态，跳过
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, b.root+string(filepath.Separator)))
		if rel == path { // TrimPrefix 没生效（root 带尾分隔符等），退一步用 Rel
			if r, err2 := filepath.Rel(b.root, path); err2 == nil {
				rel = filepath.ToSlash(r)
			}
		}
		return fn(BlobInfo{Rel: rel, Abs: path, Size: info.Size(), ModTime: info.ModTime()})
	})
	if err != nil {
		return fmt.Errorf("store: walk blobs: %w", err)
	}
	return nil
}

// DecodePNGSize 读 PNG 的像素尺寸（不做完整解码）。
func DecodePNGSize(pngBytes []byte) (w, h int, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		return 0, 0, fmt.Errorf("store: decode PNG header: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// DecodePNGSizeFromFile 只读文件开头的 PNG 头拿像素尺寸。
//
// 与 DecodePNGSize 的区别：它从**文件**读，且不需要调用方先把整张图
// 读进内存。导出时 items 表没有 image_width 列，尺寸只能这么拿；
// 用 DecodeConfig 而不是 Decode，是因为后者要解出全部像素行
// （10 MB 的图会真的分配几十 MB）。
func DecodePNGSizeFromFile(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("store: decode PNG header from %s: %w", path, err)
	}
	return cfg.Width, cfg.Height, nil
}

// DecodePNG 完整解码（缩略图与校验用）。
func DecodePNG(pngBytes []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("store: decode PNG: %w", err)
	}
	return img, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isToken(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
