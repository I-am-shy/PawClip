package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zego/pawclip/store"
)

// 本文件是 .clipbak 的导出（BACKUP-FORMAT.md §7）。
//
// 两条硬约束贯穿全文件：
//
//  1. **条目顺序必须是 README.txt → blobs/** → manifest.**（§2）。
//     清单最后写，因为它里面的 stats 要等全部处理完才知道。
//  2. **内存占用与条目数无关**（§7）。items 走两遍游标扫描：第一遍写 blob，
//     第二遍流式写清单。绝不把 items collect 进切片，也绝不把 blob 内容
//     整个读进内存（用 64 KB 块流式搬运）。

// Pauser 是"导出期间暂停 GC"的最小接口（§7 第 1 步）。
//
// 用接口而不是直接依赖 retention 包：backup 不该知道 GC 长什么样，
// 它只需要"能让对方先停下"。retention.GC 天然满足这个接口。
type Pauser interface {
	Pause()
	Resume()
}

// ExportScope 是导出范围（§7 的 scope 选项）。
type ExportScope string

const (
	ScopeFull     ExportScope = "full"
	ScopePinned   ExportScope = "pinned"
	ScopeCategory ExportScope = "category"
	ScopeRange    ExportScope = "range"
)

// blob role 取值（§3.4）。
const (
	roleImage = "image"
	roleRTF   = "rtf"
	roleHTML  = "html"
	roleFile  = "file"
)

// ExportOptions 是导出选项（§7 表格）。
type ExportOptions struct {
	Scope      ExportScope
	CategoryID *int64
	Since      *int64
	Until      *int64
	// ScopeLabel 是文件名里那个 scope 片段；为空时按 Scope 自动生成。
	ScopeLabel string

	ExcludeKinds   []string
	IncludeExpired bool
	ManifestFormat string // json | yaml

	// EmbedFiles 默认 false。开启后内嵌文件类条目的引用文件内容（§5）。
	// UI 必须明确提示"包体积可能显著增大"。
	EmbedFiles        bool
	EmbedFileMaxBytes int64

	OutputDir  string
	AppVersion string
	Platform   string
	Settings   map[string]any

	// Preamble 是 README.txt 开头那段**给人看的**提示（按调用方的界面语言
	// 生成，见 §14 第 24 条"备份包内 README.txt"）。
	//
	// 为什么由调用方传而不是在这里翻译：backup 包是纯格式层，
	// 它不该知道用户选了哪种语言；同样的理由，它也不该 import i18n。
	// 传进来一段现成的文本，职责边界最干净。
	Preamble string
}

func (o ExportOptions) withDefaults() ExportOptions {
	out := o
	if out.Scope == "" {
		out.Scope = ScopeFull
	}
	switch strings.ToLower(out.ManifestFormat) {
	case "yaml", "yml":
		out.ManifestFormat = "yaml"
	default:
		out.ManifestFormat = "json"
	}
	if out.EmbedFileMaxBytes <= 0 {
		out.EmbedFileMaxBytes = 50 << 20
	}
	if out.OutputDir == "" {
		out.OutputDir = "."
	}
	if out.Platform == "" {
		out.Platform = "macos"
	}
	return out
}

// ExportResult 是一次导出的结果。
type ExportResult struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	Items      int64  `json:"items"`
	Categories int64  `json:"categories"`
	Tags       int64  `json:"tags"`
	BlobBytes  int64  `json:"blobBytes"`
	Blobs      int64  `json:"blobs"`
	// MissingBlobs 是"清单引用到了、但磁盘上找不到"的 blob 数（§7 第 9 步）。
	// 大于 0 必须告诉用户：那个包是不完整的。
	MissingBlobs int64    `json:"missingBlobs"`
	Warnings     []string `json:"warnings,omitempty"`
	TookMs       int64    `json:"tookMs"`
}

// ScopeClause 生成条目过滤条件与参数。被导出与"预计导出多少条"共用。
func (o ExportOptions) ScopeClause(now int64) (string, []any) {
	where := []string{"deleted_at IS NULL"}
	var args []any

	if !o.IncludeExpired {
		// 已过期但还没被 GC 清掉的条目不该进包：用户预期是"我的历史"，
		// 不是"外加那些本来就要消失的"。
		where = append(where, "(expires_at IS NULL OR expires_at > ?)")
		args = append(args, now)
	}
	switch o.Scope {
	case ScopePinned:
		where = append(where, "pinned = 1")
	case ScopeCategory:
		if o.CategoryID != nil {
			where = append(where, "category_id = ?")
			args = append(args, *o.CategoryID)
		}
	case ScopeRange:
		if o.Since != nil {
			where = append(where, "first_seen_at >= ?")
			args = append(args, *o.Since)
		}
		if o.Until != nil {
			where = append(where, "first_seen_at <= ?")
			args = append(args, *o.Until)
		}
	}
	if len(o.ExcludeKinds) > 0 {
		ph := make([]string, 0, len(o.ExcludeKinds))
		for _, k := range o.ExcludeKinds {
			ph = append(ph, "?")
			args = append(args, k)
		}
		where = append(where, "kind NOT IN ("+strings.Join(ph, ",")+")")
	}
	return strings.Join(where, " AND "), args
}

// blobCopyBufSize 是分块读写的块大小（§7：64 KB）。
const blobCopyBufSize = 64 << 10

// Export 写出一个 .clipbak。
func Export(
	ctx context.Context,
	db *store.DB,
	blobs *store.BlobStore,
	opt ExportOptions,
	gc Pauser,
	log *slog.Logger,
) (*ExportResult, error) {
	if db == nil || blobs == nil {
		return nil, errors.New("backup: Export 需要 DB 与 BlobStore")
	}
	if log == nil {
		log = slog.Default()
	}
	opt = opt.withDefaults()
	start := time.Now()

	// §7 第 1 步：暂停 GC。否则 GC 的孤儿扫描可能在两遍扫描之间
	// 把刚要写入包的 blob 当成孤儿删掉。
	if gc != nil {
		gc.Pause()
		defer gc.Resume()
	}

	// §7 第 2 步：只读事务拿稳定快照。两遍扫描之间数据必须一致，
	// 否则第一遍写的 blob 与第二遍写的清单会对不上。
	tx, err := db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("backup: 开启只读事务：%w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res := &ExportResult{}
	whereSQL, args := opt.ScopeClause(start.Unix())

	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM items WHERE "+whereSQL, args...).Scan(&res.Items); err != nil {
		return nil, fmt.Errorf("backup: 统计条目数：%w", err)
	}

	cats, err := loadCategories(ctx, tx)
	if err != nil {
		return nil, err
	}
	tags, err := loadTags(ctx, tx)
	if err != nil {
		return nil, err
	}
	res.Categories = int64(len(cats))
	res.Tags = int64(len(tags))

	// 输出文件。
	scopeLabel := opt.ScopeLabel
	if scopeLabel == "" {
		scopeLabel = deriveScopeLabel(opt, cats)
	}
	name := fmt.Sprintf("pawclip-backup-%s-%s.clipbak", scopeLabel, start.Format("20060102-150405"))
	path := filepath.Join(opt.OutputDir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("backup: 创建备份文件：%w", err)
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			// 中途失败不要留半截包在那儿——用户会以为它是好的。
			_ = os.Remove(path)
		}
	}()

	zw := zip.NewWriter(f)

	// ── ① README.txt（最先写，§2 的顺序）────────────────────────
	if err := writeEntry(zw, ReadmeName, Method("text/plain"), func(w io.Writer) error {
		_, err := io.WriteString(w, readmeText(opt, start))
		return err
	}); err != nil {
		return nil, err
	}

	// ── ② blobs/**（第一遍扫描）─────────────────────────────────
	//
	// imgDims 只缓存**图片条目**的像素尺寸。为什么要缓存：items 表没有
	// image_width/height 列（DESIGN §4.1 的 DDL 里确实没有），尺寸只能从
	// PNG 头读；第一遍本来就要打开每个图片文件，顺手读个头几乎免费，
	// 而第二遍再打开一次就是成倍的文件 I/O。
	//
	// 内存代价：每个图片条目 16 字节 + map 开销，1 万张图约 300 KB。
	// 与"条目数无关"的严格 O(1) 有偏差，但只与**图片条数**相关，
	// 且不持有任何内容字节——这里如实记下来。
	imgDims := make(map[int64][2]int, 64)
	seen := make(map[string]struct{}, 64)
	// blobSizes 记下"每个 blob 实际写进包的字节数"，供清单里的
	// blobs[].bytes 回填。这是权威值——磁盘上的文件大小与真实写入量
	// 可能不一致（文件正在被写、或读的时候已被替换）。
	blobSizes := make(map[string]int64, 64)

	n, bytes, missing, warns, err := writeBlobsPass(ctx, zw, blobs, tx, whereSQL, args, opt, imgDims, seen, blobSizes)
	if err != nil {
		return nil, err
	}
	res.Blobs, res.BlobBytes, res.MissingBlobs = n, bytes, missing
	res.Warnings = append(res.Warnings, warns...)

	// ── ③ manifest.*（第二遍扫描，流式）─────────────────────────
	header := Header{
		Format:        FormatTag,
		FormatVersion: FormatVersion,
		AppVersion:    opt.AppVersion,
		ExportedAt:    start,
		Platform:      opt.Platform,
		Scope:         string(opt.Scope),
		Stats: Stats{
			Items:      res.Items,
			Categories: res.Categories,
			Tags:       res.Tags,
			BlobBytes:  res.BlobBytes,
		},
		Settings:   opt.Settings,
		Categories: cats,
		Tags:       tags,
	}

	manifestName := ManifestJSON
	if opt.ManifestFormat == "yaml" {
		manifestName = ManifestYAML
	}
	manifestMime := "application/json"
	if opt.ManifestFormat == "yaml" {
		manifestMime = "text/yaml"
	}

	err = writeEntry(zw, manifestName, Method(manifestMime), func(w io.Writer) error {
		var out manifestWriter
		if opt.ManifestFormat == "yaml" {
			out = &yamlManifestWriter{w: w}
		} else {
			out = &jsonManifestWriter{w: w, first: true}
		}
		if err := out.WriteHeader(header); err != nil {
			return err
		}
		if err := out.BeginItems(); err != nil {
			return err
		}
		written, err := streamItems(ctx, tx, whereSQL, args, imgDims, blobSizes, out.WriteItem)
		if err != nil {
			return err
		}
		if written != res.Items {
			// 两遍扫描之间条目数变了 = 只读快照没生效（不该发生）。
			// 报出来，因为清单里的 stats.items 会与真实条目数不符。
			return fmt.Errorf("backup: 条目数在两遍扫描之间变化（%d → %d）", res.Items, written)
		}
		return out.EndItems()
	})
	if err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("backup: 关闭 zip：%w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("backup: fsync：%w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("backup: 关闭文件：%w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("backup: 提交只读事务：%w", err)
	}
	committed = true

	if fi, err := os.Stat(path); err == nil {
		res.Bytes = fi.Size()
	}
	res.Path = path
	res.TookMs = time.Since(start).Milliseconds()
	if res.MissingBlobs > 0 {
		log.Warn("导出完成，但有 blob 缺失", "missing", res.MissingBlobs, "path", path)
	}
	return res, nil
}

// ── manifestWriter：JSON / YAML 两个出口 ────────────────────────

type manifestWriter interface {
	WriteHeader(h Header) error
	BeginItems() error
	WriteItem(it Item) error
	EndItems() error
}

// jsonManifestWriter 按 §7 的"流式 JSON 写法"逐条追加。
//
// 关键细节：**先写分隔符、再写内容**（第一条不加逗号）。写成
// "每条写完追加逗号"会让最后一条多出一个尾随逗号，整个清单就不是
// 合法 JSON 了——而那时文件已经写了一半，用户拿到的是个坏包。
type jsonManifestWriter struct {
	w     io.Writer
	first bool
}

func (j *jsonManifestWriter) WriteHeader(h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("backup: 编码清单头部：%w", err)
	}
	// 头部编码成一个对象（末尾是 `}`），去掉它以便后面接着追加 items。
	trimmed := strings.TrimRight(string(b), "}")
	if _, err := io.WriteString(j.w, trimmed+`,"items":[`); err != nil {
		return err
	}
	j.first = true
	return nil
}

func (j *jsonManifestWriter) BeginItems() error { return nil }

func (j *jsonManifestWriter) WriteItem(it Item) error {
	if !j.first {
		if _, err := io.WriteString(j.w, ","); err != nil {
			return err
		}
	}
	j.first = false
	b, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("backup: 编码条目：%w", err)
	}
	_, err = j.w.Write(b)
	return err
}

func (j *jsonManifestWriter) EndItems() error {
	_, err := io.WriteString(j.w, "]}")
	return err
}

type yamlManifestWriter struct {
	w       io.Writer
	started bool
}

func (y *yamlManifestWriter) WriteHeader(h Header) error { return writeYAMLHeader(y.w, h) }
func (y *yamlManifestWriter) BeginItems() error          { return nil }

func (y *yamlManifestWriter) WriteItem(it Item) error {
	y.started = true
	return writeYAMLItem(y.w, it, 1)
}

func (y *yamlManifestWriter) EndItems() error {
	if !y.started {
		// 一条都没有：`items:` 后面必须跟个合法节点，否则清单读不出来。
		_, err := io.WriteString(y.w, "  []\n")
		return err
	}
	return nil
}

// ── 第一遍：写 blobs ────────────────────────────────────────────

// writeBlobsPass 只扫 blob 相关列，把每个 blob 写进包。
//
// 三条规则：
//   - 缩略图**不导出**（§5）：它是派生数据，导入后重新生成，
//     导出只会让包白涨约 15%。thumb_path 根本不查。
//   - 内容寻址去重：同一份字节被两条条目引用（很常见：同一张图
//     先单复制、再"图+文字"复制）时只写一次。
//   - 大于 64 KB 的 HTML 外置成 blobs/*.html（§4 的 INLINE_MAX_BYTES）。
func writeBlobsPass(
	ctx context.Context,
	zw *zip.Writer,
	blobs *store.BlobStore,
	tx *sql.Tx,
	whereSQL string,
	args []any,
	opt ExportOptions,
	imgDims map[int64][2]int,
	seen map[string]struct{},
	sizes map[string]int64,
) (count int64, totalBytes int64, missing int64, warnings []string, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, image_path, rtf_path, html_content FROM items WHERE `+whereSQL, args...)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("backup: 扫描 blob 引用：%w", err)
	}
	defer rows.Close()

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return count, totalBytes, missing, warnings, err
		}
		var (
			id       int64
			imgPath  sql.NullString
			rtfPath  sql.NullString
			htmlText sql.NullString
		)
		if err := rows.Scan(&id, &imgPath, &rtfPath, &htmlText); err != nil {
			return count, totalBytes, missing, warnings, fmt.Errorf("backup: 扫描 blob 引用行：%w", err)
		}

		// 图片。
		if imgPath.Valid && imgPath.String != "" {
			rel := imgPath.String
			inPkg := BlobDirPrefix + rel
			if _, ok := seen[inPkg]; !ok {
				seen[inPkg] = struct{}{}
				abs := blobs.Abs(rel)
				if w, h, e := store.DecodePNGSizeFromFile(abs); e == nil {
					imgDims[id] = [2]int{w, h}
				}
				n, sum, e := writeFileBlob(zw, inPkg, "image/png", abs)
				if e != nil {
					if os.IsNotExist(unwrapAll(e)) {
						missing++
						warnings = append(warnings, fmt.Sprintf("条目 %d 的图片 %s 不在磁盘上", id, rel))
						continue
					}
					return count, totalBytes, missing, warnings, e
				}
				totalBytes += n
				count++
				sizes[inPkg] = n
				if want := shaFromShardPath(rel); want != "" && sum != want {
					warnings = append(warnings,
						fmt.Sprintf("图片 %s 的内容与其 sha256 不符（期望 %s，实际 %s）", rel, want, sum))
				}
			}
		}

		// RTF。
		if rtfPath.Valid && rtfPath.String != "" {
			rel := rtfPath.String
			inPkg := BlobDirPrefix + rel
			if _, ok := seen[inPkg]; !ok {
				seen[inPkg] = struct{}{}
				abs := blobs.Abs(rel)
				n, sum, e := writeFileBlob(zw, inPkg, "application/rtf", abs)
				if e != nil {
					if os.IsNotExist(unwrapAll(e)) {
						missing++
						warnings = append(warnings, fmt.Sprintf("条目 %d 的 RTF %s 不在磁盘上", id, rel))
						continue
					}
					return count, totalBytes, missing, warnings, e
				}
				totalBytes += n
				count++
				sizes[inPkg] = n
				if want := shaFromShardPath(rel); want != "" && sum != want {
					warnings = append(warnings,
						fmt.Sprintf("RTF %s 的内容与其 sha256 不符", rel))
				}
			}
		}

		// 超过内联上限的 HTML 外置（§4：INLINE_MAX_BYTES = 64 KB）。
		if htmlText.Valid && len(htmlText.String) > InlineMaxBytes {
			data := []byte(htmlText.String)
			sum := sha256.Sum256(data)
			hexSum := hex.EncodeToString(sum[:])
			inPkg, e := shardPathInPackage(hexSum, "html")
			if e != nil {
				return count, totalBytes, missing, warnings, e
			}
			if _, ok := seen[inPkg]; !ok {
				seen[inPkg] = struct{}{}
				if e := writeBytesBlob(zw, inPkg, "text/html", data); e != nil {
					return count, totalBytes, missing, warnings, e
				}
				totalBytes += int64(len(data))
				count++
				sizes[inPkg] = int64(len(data))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return count, totalBytes, missing, warnings, fmt.Errorf("backup: 遍历 blob 引用：%w", err)
	}
	return count, totalBytes, missing, warnings, nil
}

// ── 第二遍：流式写 items ────────────────────────────────────────

// streamItems 逐行读 items 并回调，**不把结果收集进切片**。
func streamItems(
	ctx context.Context,
	tx *sql.Tx,
	whereSQL string,
	args []any,
	imgDims map[int64][2]int,
	blobSizes map[string]int64,
	emit func(Item) error,
) (int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, kind, text_content, html_content, rtf_path, image_path, file_paths,
       preview, fingerprint, byte_size, source_app_id, source_app_name, source_url,
       category_id, pinned, first_seen_at, expires_at, ttl_source, created_at,
       last_used_at, use_count
  FROM items WHERE `+whereSQL+` ORDER BY id ASC`, args...)
	if err != nil {
		return 0, fmt.Errorf("backup: 扫描条目：%w", err)
	}
	defer rows.Close()

	var count int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		it, err := scanExportItem(ctx, rows, tx, imgDims)
		if err != nil {
			return count, err
		}
		attachBlobSizes(it, blobSizes)
		if err := emit(*it); err != nil {
			return count, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("backup: 遍历条目：%w", err)
	}
	return count, nil
}

// scanExportItem 把一行数据库记录转成清单 Item。
func scanExportItem(ctx context.Context, rows *sql.Rows, tx *sql.Tx, imgDims map[int64][2]int) (*Item, error) {
	var (
		it                  Item
		text, html          sql.NullString
		rtfPath, imgPath    sql.NullString
		filePathsJSON       sql.NullString
		srcURL              sql.NullString
		catID, expiresAt    sql.NullInt64
		lastUsed            sql.NullInt64
		ttlSource           sql.NullString
		srcAppID, srcAppNam sql.NullString
		pinnedInt           sql.NullInt64
		// ⚠️ first_seen_at / created_at 在库里是 **INTEGER epoch 秒**，
		// 而清单里是带时区的 ISO-8601。所以必须先扫进 NullInt64 再转，
		// **不能**直接扫进 Item.FirstSeenAt（那是 time.Time）——
		// database/sql 不会把 int64 自动转成 time.Time，会直接报
		//
		//   unsupported Scan, storing driver.Value type int64 into type *time.Time
		//
		// 这类错误在"库里全空"时不会暴露（没有行进扫描路径），
		// 所以很容易一直绿到真正跑导出那一刻。
		firstSeen, createdAt sql.NullInt64
	)
	if err := rows.Scan(
		&it.ID, &it.Kind, &text, &html, &rtfPath, &imgPath, &filePathsJSON,
		&it.Preview, &it.Fingerprint, &it.ByteSize, &srcAppID, &srcAppNam, &srcURL,
		&catID, &pinnedInt, &firstSeen, &expiresAt, &ttlSource, &createdAt,
		&lastUsed, &it.UseCount,
	); err != nil {
		return nil, fmt.Errorf("backup: 扫描条目行：%w", err)
	}
	it.Pinned = pinnedInt.Int64 != 0
	it.FirstSeenAt = time.Unix(firstSeen.Int64, 0)
	it.CreatedAt = time.Unix(createdAt.Int64, 0)

	if text.Valid {
		s := text.String
		it.Text = &s
	}
	if html.Valid {
		// 超过 64 KB 的 HTML 已经外置成 blob，清单里内联字段留 null
		// （§4 的内容形态决策表）。
		if len(html.String) <= InlineMaxBytes {
			s := html.String
			it.HTML = &s
		} else {
			data := []byte(html.String)
			sum := sha256.Sum256(data)
			inPkg, err := shardPathInPackage(hex.EncodeToString(sum[:]), "html")
			if err != nil {
				return nil, err
			}
			p := inPkg
			it.RTFPath = nil
			it.Blobs = append(it.Blobs, BlobRef{
				Role:   roleHTML,
				Path:   p,
				Mime:   "text/html",
				Bytes:  int64(len(data)),
				SHA256: hex.EncodeToString(sum[:]),
			})
		}
	}
	if rtfPath.Valid && rtfPath.String != "" {
		inPkg := BlobDirPrefix + rtfPath.String
		it.RTFPath = &inPkg
		if sha := shaFromShardPath(rtfPath.String); sha != "" {
			it.Blobs = append(it.Blobs, BlobRef{
				Role: roleRTF, Path: inPkg, Mime: "application/rtf", SHA256: sha,
			})
		}
	}
	if imgPath.Valid && imgPath.String != "" {
		inPkg := BlobDirPrefix + imgPath.String
		if sha := shaFromShardPath(imgPath.String); sha != "" {
			it.Blobs = append(it.Blobs, BlobRef{
				Role: roleImage, Path: inPkg, Mime: "image/png", SHA256: sha,
			})
		}
		if d, ok := imgDims[it.ID]; ok {
			it.ImageWidth, it.ImageHeight = d[0], d[1]
		}
	}
	if filePathsJSON.Valid && filePathsJSON.String != "" {
		var paths []string
		if err := json.Unmarshal([]byte(filePathsJSON.String), &paths); err == nil {
			it.FilePaths = paths
		}
	}
	if srcURL.Valid {
		s := srcURL.String
		it.SourceURL = &s
	}
	it.SourceAppID = srcAppID.String
	it.SourceAppName = srcAppNam.String
	if catID.Valid {
		v := catID.Int64
		it.CategoryID = &v
	}

	// §3.5：pinned 为 true 时 expiresAt 必须为 null。
	if expiresAt.Valid && !it.Pinned {
		t := time.Unix(expiresAt.Int64, 0)
		it.ExpiresAt = &t
		// TTLSeconds 是**相对时长**。定义成"原始寿命" = 到期时刻 - 首次复制时刻；
		// 导入方按"从导入时刻重新起算"解释它（§3.5）。
		if life := expiresAt.Int64 - it.FirstSeenAt.Unix(); life > 0 {
			it.TTLSeconds = &life
		}
	}
	if lastUsed.Valid {
		t := time.Unix(lastUsed.Int64, 0)
		it.LastUsedAt = &t
	}

	// 标签：逐条查（导出是流式的，不做全表 JOIN 以免内存随条目数增长）。
	tagIDs, err := itemTagIDs(ctx, tx, it.ID)
	if err != nil {
		return nil, err
	}
	it.TagIDs = tagIDs

	return &it, nil
}

// attachBlobSizes 用"实际写进包的字节数"补齐 BlobRef.Bytes。
//
// 为什么不用 os.Stat 的文件大小：文件可能正在被写入、也可能在导出过程中
// 被替换。真正的权威值是**我们写进 zip 的那几个字节**。
// sizes 只在第一遍扫描时填，内存代价是每个 blob 一个 map 项。
func attachBlobSizes(it *Item, sizes map[string]int64) {
	for i := range it.Blobs {
		if n, ok := sizes[it.Blobs[i].Path]; ok {
			it.Blobs[i].Bytes = n
		}
	}
}

// ── 辅助 ────────────────────────────────────────────────────────

// loadCategories 读全部分类。
//
// 导出**全部分类**而不是"只导出被引用的"：分类数量本来就是个位数，
// 少导会让 scope=pinned 这类包的清单里出现悬空 categoryId。
func loadCategories(ctx context.Context, tx *sql.Tx) ([]Category, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id, name, COALESCE(color,''), COALESCE(icon,''), COALESCE(rule,''),"+
			" ttl_seconds, sort_order FROM categories ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("backup: 读分类：%w", err)
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var (
			c   Category
			ttl sql.NullInt64
			raw string
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.Icon, &raw, &ttl, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("backup: 扫描分类：%w", err)
		}
		if ttl.Valid {
			v := ttl.Int64
			c.TTLSeconds = &v
		}
		if strings.TrimSpace(raw) != "" {
			c.Rule = json.RawMessage(raw)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadTags 读全部标签。
func loadTags(ctx context.Context, tx *sql.Tx) ([]Tag, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id, name, COALESCE(color,'') FROM tags ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("backup: 读标签：%w", err)
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color); err != nil {
			return nil, fmt.Errorf("backup: 扫描标签：%w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// itemTagIDs 读一条条目的标签 id。
//
// 逐条查而不是一次 JOIN 拉全表：导出是流式的，一次拉全表会让内存
// 随条目数增长。item_tags 的主键是 (item_id, tag_id)，这是前缀扫描，
// 每条极快。
func itemTagIDs(ctx context.Context, tx *sql.Tx, itemID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT tag_id FROM item_tags WHERE item_id = ? ORDER BY tag_id", itemID)
	if err != nil {
		return nil, fmt.Errorf("backup: 读条目标签：%w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("backup: 扫描条目标签：%w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// writeEntry 往 zip 里写一个条目。
func writeEntry(zw *zip.Writer, name string, method uint16, fn func(io.Writer) error) error {
	hdr := &zip.FileHeader{
		Name:   name,
		Method: method,
		// 2 秒粒度是 ZIP 格式的固有限制（DOS 时间戳）。
		Modified: time.Now(),
	}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("backup: 创建 zip 条目 %s：%w", name, err)
	}
	return fn(w)
}

// writeFileBlob 把磁盘上的 blob 分块写进 zip，返回写入字节数与实际 sha256。
func writeFileBlob(zw *zip.Writer, inPackagePath, mime, abs string) (int64, string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, "", fmt.Errorf("backup: 打开 blob %s：%w", abs, err)
	}
	defer func() { _ = f.Close() }()

	hdr := &zip.FileHeader{Name: inPackagePath, Method: Method(mime), Modified: time.Now()}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return 0, "", fmt.Errorf("backup: 创建 zip 条目 %s：%w", inPackagePath, err)
	}

	h := sha256.New()
	buf := make([]byte, blobCopyBufSize)
	n, err := io.CopyBuffer(io.MultiWriter(w, h), f, buf)
	if err != nil {
		return n, "", fmt.Errorf("backup: 写入 blob %s：%w", inPackagePath, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// writeBytesBlob 把内存里的字节写进 zip（只用于外置的 HTML）。
func writeBytesBlob(zw *zip.Writer, inPackagePath, mime string, data []byte) error {
	hdr := &zip.FileHeader{Name: inPackagePath, Method: Method(mime), Modified: time.Now()}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("backup: 创建 zip 条目 %s：%w", inPackagePath, err)
	}
	_, err = w.Write(data)
	return err
}

// shardPathInPackage 由 sha256 与扩展名算出**包内**路径：blobs/<a>/<b>/<sha>.<ext>。
//
// 复用 store.ShardPath 而不是自己拼：包内布局与 blobs/ 磁盘布局是同一套
// 规则（§11 第 4 条就是这么写的），两处各拼一份迟早会漂移。
func shardPathInPackage(shaHex, ext string) (string, error) {
	rel, err := store.ShardPath(shaHex, ext)
	if err != nil {
		return "", fmt.Errorf("backup: 计算 blob 路径：%w", err)
	}
	return BlobDirPrefix + rel, nil
}

// shaFromShardPath 从一个 blobs/<a>/<b>/<sha>.<ext> 形式的路径里取出 sha。
// 取不出来时返回空串（调用方跳过校验）。
func shaFromShardPath(rel string) string {
	base := rel
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	if ValidSHA256(base) {
		return base
	}
	return ""
}

// deriveScopeLabel 生成文件名里的 scope 片段。
func deriveScopeLabel(opt ExportOptions, cats []Category) string {
	switch opt.Scope {
	case ScopePinned:
		return "pinned"
	case ScopeCategory:
		name := "unknown"
		if opt.CategoryID != nil {
			for _, c := range cats {
				if c.ID == *opt.CategoryID {
					name = c.Name
					break
				}
			}
		}
		return "cat-" + sanitizeFileSegment(name)
	case ScopeRange:
		from, to := "start", "end"
		if opt.Since != nil {
			from = time.Unix(*opt.Since, 0).Format("20060102")
		}
		if opt.Until != nil {
			to = time.Unix(*opt.Until, 0).Format("20060102")
		}
		return "range-" + from + "-" + to
	default:
		return "full"
	}
}

// sanitizeFileSegment 把可能出现在文件名里的危险字符换掉。
//
// 分类名是用户自由输入的，可能含 `/`（会把文件写到别的目录）或
// Windows 的保留字符。文件名也是**i18n 易漏位置**之一（§14 第 24 条
// 明确点了"导出文件名中的分类名"）。
func sanitizeFileSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unnamed"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// 别让文件名太长（多数文件系统上限 255 字节）。
	if len(out) > 40 {
		out = strings.ToValidUTF8(out[:40], "")
	}
	return out
}

// unwrapAll 把包装过的错误剥到底，供 os.IsNotExist 判定。
func unwrapAll(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}
