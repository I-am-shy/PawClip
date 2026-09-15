// Package backup 实现 DESIGN.md §6 与 BACKUP-FORMAT.md 定义的 .clipbak 备份包。
//
// 一句话原则（BACKUP-FORMAT.md §11 的最终裁判）：
// **包必须能被三行 Python 脚本读出来。** 所以它用 ZIP 容器、纯文本清单、
// 无私有二进制编码，不用 tar.zst。
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 格式常量。
const (
	// FormatTag 是清单里的格式标识，导入时用来确认"这确实是我们家的包"。
	FormatTag = "pawclip.backup"
	// FormatVersion 是本实现写出的版本。读取方规则见 BACKUP-FORMAT.md §10。
	FormatVersion = 1

	// ManifestJSON / ManifestYAML 是清单在包内的两个可能文件名。
	ManifestJSON = "manifest.json"
	ManifestYAML = "manifest.yaml"
	// ReadmeName 是包内的人可读说明。
	ReadmeName = "README.txt"
	// BlobDirPrefix 是包内 blob 的目录前缀。
	BlobDirPrefix = "blobs/"

	// InlineMaxBytes 是内联与走 blob 的分界线（§4，常量 65536）。
	InlineMaxBytes = 65536

	// MaxTotalUncompressed 是整包解压后的绝对上限（§9 解压炸弹防护）。
	MaxTotalUncompressed int64 = 20 << 30 // 20 GB
	// BombRatio 是"声明总量 × 系数"这个相对上限（§9）。
	BombRatio = 1.2
	// MaxCompressionRatio 是单条目压缩比上限（§9）。
	MaxCompressionRatio = 1000
	// MaxSingleUncompressed 是单条目解压后上限（§9：超过 imageMaxBytes 跳过）。
	DefaultMaxSingleBytes int64 = 10 << 20
)

// 错误值。导入预检失败时用它区分"哪种坏包"，便于 UI 给出具体提示。
var (
	ErrManifestMissing  = errors.New("backup: 包里找不到 manifest.json / manifest.yaml")
	ErrManifestUnread   = errors.New("backup: 清单既不是合法 JSON 也不是合法 YAML")
	ErrNotPawClipBackup = errors.New("backup: 这不是 PawClip 备份包（format 字段不符）")
	ErrBackupTooNew     = errors.New("backup: 备份包版本高于本应用支持的版本，请先升级")
	ErrUnsafeEntry      = errors.New("backup: 包内含不安全的条目名（ZIP Slip）")
	ErrBomb             = errors.New("backup: 解压后体积异常，疑似解压炸弹")
	ErrNoSpace          = errors.New("backup: 磁盘可用空间不足")
)

// ── 清单结构（BACKUP-FORMAT.md §3）──────────────────────────────

// Header 是清单里"非 items"的那部分。
//
// 之所以把 items 拆出去：导出是流式的（§7），items 在全过程中是逐行扫描、
// 从不整体载入内存，所以它不属于这个结构。
type Header struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"formatVersion"`
	AppVersion    string         `json:"appVersion"`
	ExportedAt    time.Time      `json:"exportedAt"`
	Platform      string         `json:"platform"`
	Scope         string         `json:"scope"`
	Stats         Stats          `json:"stats"`
	Settings      map[string]any `json:"settings,omitempty"`
	Categories    []Category     `json:"categories"`
	Tags          []Tag          `json:"tags"`
}

// Manifest 是**读取侧**的完整清单：头部 + items。
//
// 写入侧刻意不用它：导出是流式的（§7），items 必须逐条追加、
// 不能先攒成一个切片。这里能整体载入，是因为导入本来就按
// "预检一次 + 逐条写入"走，预检需要先知道全部条目才能给出确认页。
type Manifest struct {
	Header
	Items []Item `json:"items"`
}

// Stats 是清单里的统计块。
//
// BlobBytes 有两个用途：给用户看"这个包多大"，以及**导入时的解压炸弹
// 防护基准**（§9：预检累加的 uncompressed_size 不得超过 blobBytes × 1.2）。
type Stats struct {
	Items      int64 `json:"items"`
	Categories int64 `json:"categories"`
	Tags       int64 `json:"tags"`
	// BlobBytes 是未压缩总字节（只算真正写进包的 blob，不含缩略图）。
	BlobBytes int64 `json:"blobBytes"`
}

// Category 是清单里的一条分类（§3.2）。
//
// ID 只用于**包内交叉引用**。导入方绝不能沿用（§3.5）——两台机器的
// ID 体系完全无关，按 name 匹配。
type Category struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Color      string `json:"color,omitempty"`
	Icon       string `json:"icon,omitempty"`
	SortOrder  int    `json:"sortOrder"`
	TTLSeconds *int64 `json:"ttlSeconds"`
	// Rule 是原始 JSON 字符串（库里就是 TEXT 存的 JSON），导出时原样带上；
	// 用 RawMessage 是为了让它成为**对象**而不是被转义成字符串。
	Rule json.RawMessage `json:"rule,omitempty"`
}

// Tag 是清单里的一条标签（§3.3）。
type Tag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// BlobRef 是 items[].blobs[] 的一项（§3.4）。
type BlobRef struct {
	// Role 取值 image / rtf / html。
	Role string `json:"role"`
	// Path 是**包内**路径，形如 blobs/9f/2a/<sha>.png。
	Path string `json:"path"`
	Mime string `json:"mime"`
	// Bytes 是未压缩字节数。
	Bytes int64 `json:"bytes"`
	// SHA256 是 64 位小写十六进制（不带 "sha256:" 前缀）。
	SHA256 string `json:"sha256"`
}

// Item 是清单里的一条条目（§3.4）。
//
// 时间字段一律 ISO-8601 **带时区偏移**（§3.5）：不要导出裸 epoch，
// 跨时区迁移会出错。Go 的 time.Time 默认就按 RFC3339 输出带偏移的形式。
type Item struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Preview     string `json:"preview"`
	Fingerprint string `json:"fingerprint"`
	ByteSize    int64  `json:"byteSize"`

	Text *string `json:"text"`
	HTML *string `json:"html"`
	// RTFPath 指向**包内** blob（不是数据库里的分片路径）。
	RTFPath *string `json:"rtfPath"`

	FilePaths []string `json:"filePaths"`

	ImageWidth  int `json:"imageWidth"`
	ImageHeight int `json:"imageHeight"`

	SourceAppID   string  `json:"sourceAppId"`
	SourceAppName string  `json:"sourceAppName"`
	SourceURL     *string `json:"sourceUrl"`

	CategoryID *int64  `json:"categoryId"`
	TagIDs     []int64 `json:"tagIds"`

	Pinned bool `json:"pinned"`
	// TTLSeconds 是相对时长；ExpiresAt 是绝对时刻。
	// 两者都导出，导入方二选一（§3.5：默认按绝对时间，避免凭空续命）。
	TTLSeconds *int64     `json:"ttlSeconds"`
	ExpiresAt  *time.Time `json:"expiresAt"`

	FirstSeenAt time.Time  `json:"firstSeenAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
	UseCount    int64      `json:"useCount"`

	Blobs []BlobRef `json:"blobs"`
}

// Validate 检查条目里我们自己能自证的部分。
func (it *Item) Validate() error {
	if it.Kind == "" {
		return errors.New("kind 为空")
	}
	if !ValidFingerprint(it.Fingerprint) {
		return fmt.Errorf("fingerprint 格式非法：%q", it.Fingerprint)
	}
	if it.Pinned && it.ExpiresAt != nil {
		// §3.5：pinned 为 true 时 expiresAt 必须为 null。
		return errors.New("pinned=true 但 expiresAt 非 null")
	}
	for i, b := range it.Blobs {
		if !ValidSHA256(b.SHA256) {
			return fmt.Errorf("blobs[%d].sha256 格式非法：%q", i, b.SHA256)
		}
		if b.Role == "" {
			return fmt.Errorf("blobs[%d].role 为空", i)
		}
	}
	return nil
}

// ValidFingerprint 校验 fingerprint 的格式：sha256:<64 位小写十六进制>。
func ValidFingerprint(s string) bool {
	if !strings.HasPrefix(s, "sha256:") {
		return false
	}
	return ValidSHA256(strings.TrimPrefix(s, "sha256:"))
}

// ValidSHA256 校验 64 位小写十六进制。
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// FingerprintFromSHA 由 blob 的 sha256 造出条目指纹（导入时重算用）。
func FingerprintFromSHA(hex string) string { return "sha256:" + hex }

// MimeForExt 由扩展名推出 mime，用于 blobs[].mime 与压缩策略判定（§6）。
func MimeForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	case "tiff", "tif":
		return "image/tiff"
	case "rtf":
		return "application/rtf"
	case "html", "htm":
		return "text/html"
	case "txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// ExtForMime 由 mime 推出建议扩展名。
func ExtForMime(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	case "application/rtf":
		return "rtf"
	case "text/html":
		return "html"
	default:
		return "bin"
	}
}

// UseStore 判断某个 mime 在 ZIP 里该用 store 而不是 deflate（§6）。
//
// 依据：已压缩的图片格式再 deflate 只能省 0–2%，纯浪费 CPU；
// 但 image/bmp 与 image/tiff 是**未压缩位图**，deflate 收益很大，要压。
func UseStore(mime string) bool {
	if !strings.HasPrefix(mime, "image/") {
		return false
	}
	switch mime {
	case "image/bmp", "image/tiff":
		return false
	}
	return true
}

// Method 返回 zip 的压缩方法（0 = Store，8 = Deflate）。
func Method(mime string) uint16 {
	if UseStore(mime) {
		return 0
	}
	return 8
}
