package backup

import (
	"archive/zip"
	"bytes"
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
	"path"
	"strings"
	"time"

	"github.com/zego/pawclip/store"
)

// 本文件是 .clipbak 的导入（BACKUP-FORMAT.md §8、§9）。
//
// §9 开篇那句话是整份规范里最要紧的一句：
//
//	**「.clipbak 是可以从任何地方下载到的文件。」**
//
// 所以这个文件里所有"防御性"的代码都不是洁癖。三条铁律：
//
//  1. **永远不要用包内的条目名去拼落盘路径。** 条目名只用于"从包里
//     定位读取"，目标路径一律由我们**根据 sha256 重新计算**得出。
//     这是 ZIP Slip 的根治办法——即使条目名校验漏了一处，也写不到包外。
//  2. **先预检、后写入**（§8.1）。用户在点"导入"之前必须看到会发生什么。
//  3. **流式处理，不全量载入**（§8.2 第 4 步）。逐条处理、每 500 条提交。

// 冲突策略（§8.3）。
const (
	ConflictMerge     = "merge"
	ConflictSkip      = "skip"
	ConflictOverwrite = "overwrite"
)

// 过期语义（§8.3）。
const (
	ExpiryAbsolute = "absolute"
	ExpiryReset    = "reset"
)

// 分类策略（§8.3）。
const (
	CategoryMergeByName = "mergeByName"
	CategoryCreateAll   = "createAll"
)

// importCommitEvery 是导入的提交间隔（§8.2 第 4g 步：每 500 条提交一次）。
//
// 不这么做的话，一个万级条目的包会攒出一个巨大的长事务，
// WAL 会一路涨到几个 GB。
const importCommitEvery = 500

// ImportOptions 是导入选项（§8.3）。
type ImportOptions struct {
	ConflictPolicy string
	ExpiryPolicy   string
	ImportExpired  bool
	CategoryPolicy string
	// ImportSettings 默认 false：迁移数据不该顺手改掉目标机器的保留策略（§3.1）。
	ImportSettings bool
	// ErrorTolerance 是累计失败上限，超过就中止（δ§8.3 默认 100），
	// 避免一个坏包把磁盘跑满。
	ErrorTolerance int
	// MaxSingleBytes 是单条目解压后的上限（§9：默认 capture.imageMaxBytes）。
	MaxSingleBytes int64
}

func (o ImportOptions) withDefaults() ImportOptions {
	out := o
	switch out.ConflictPolicy {
	case ConflictSkip, ConflictOverwrite, ConflictMerge:
	default:
		out.ConflictPolicy = ConflictMerge
	}
	switch out.ExpiryPolicy {
	case ExpiryReset, ExpiryAbsolute:
	default:
		out.ExpiryPolicy = ExpiryAbsolute
	}
	switch out.CategoryPolicy {
	case CategoryCreateAll, CategoryMergeByName:
	default:
		out.CategoryPolicy = CategoryMergeByName
	}
	if out.ErrorTolerance <= 0 {
		out.ErrorTolerance = 100
	}
	if out.MaxSingleBytes <= 0 {
		out.MaxSingleBytes = DefaultMaxSingleBytes
	}
	return out
}

// PrecheckResult 是阶段一的产物，也是确认页要展示的全部信息（§8.1 第 6 步）。
//
// 第 6 步是**不可省的**：用户点"导入"之前必须看到会发生什么，
// 尤其是"将新建 3 个分类""将跳过 412 条已过期"这类会改变预期的信息。
type PrecheckResult struct {
	Path         string `json:"path"`
	ManifestName string `json:"manifestName"`
	ManifestHash string `json:"manifestHash"`

	Format        string `json:"format"`
	FormatVersion int    `json:"formatVersion"`
	AppVersion    string `json:"appVersion"`
	ExportedAt    string `json:"exportedAt"`
	Platform      string `json:"platform"`
	Scope         string `json:"scope"`

	Total         int `json:"total"`
	WillImport    int `json:"willImport"`
	SkipDuplicate int `json:"skipDuplicate"`
	SkipExpired   int `json:"skipExpired"`
	Invalid       int `json:"invalid"`

	CategoriesNew   []string `json:"categoriesNew"`
	CategoriesReuse []string `json:"categoriesReuse"`
	TagsNew         int      `json:"tagsNew"`
	TagsReuse       int      `json:"tagsReuse"`

	UncompressedBytes int64 `json:"uncompressedBytes"`
	NeedBytes         int64 `json:"needBytes"`
	AvailableBytes    int64 `json:"availableBytes"`
	BlobCount         int   `json:"blobCount"`

	Warnings []string `json:"warnings,omitempty"`

	manifest *Manifest
}

// ImportResult 是阶段二的产物。
//
// ⚠️ 字段之间的从属关系（很容易看错，这里写死）：
//
//	Imported    = 处理完并**存在于库中**的条数 = 新插入 + Merged + Overwritten
//	Merged      ⊂ Imported —— 冲突策略 merge 命中已有条目的那部分
//	Overwritten ⊂ Imported —— 冲突策略 overwrite 先软删旧行再插新的那部分
//	Skipped     与 Imported 互斥（策略要求跳过、或条目已过期）
//	Failed      与上面都互斥（处理这条时报错了）
//
// 之所以不把 Imported 定义成"纯新插入"：用户在确认页看到的主数字是
// "这些内容现在在我库里了"，把 merge 掉的那部分排除掉会让数字对不上账
// （包里有 1000 条、库里多了 1000 条，但 Imported 显示 400）。
// 想知道真正新插了几行，用 Imported - Merged - Overwritten。
type ImportResult struct {
	ImportID       int64    `json:"importId"`
	Imported       int      `json:"imported"`
	Skipped        int      `json:"skipped"`
	Failed         int      `json:"failed"`
	Merged         int      `json:"merged"`
	Overwritten    int      `json:"overwritten"`
	CategoriesMade int      `json:"categoriesMade"`
	TagsMade       int      `json:"tagsMade"`
	BlobsWritten   int      `json:"blobsWritten"`
	BlobsSkipped   int      `json:"blobsSkipped"`
	ThumbsMade     int      `json:"thumbsMade"`
	Status         string   `json:"status"`
	Errors         []string `json:"errors,omitempty"`
	// Warnings 是非致命问题（例如缩略图重建失败）。与 Errors 分开：
	// Errors 里的每一条都对应一条真没导进来的条目，Warnings 里的
	// 只是"数据进来了，但有点小瑕疵"。混在一起用户无法判断要不要重导。
	Warnings         []string `json:"warnings,omitempty"`
	TookMs           int64    `json:"tookMs"`
	RollbackPossible bool     `json:"rollbackPossible"`
}

// ── 阶段一：预检（不写库）─────────────────────────────────────

// Precheck 打开包、校验、统计，**不写任何数据**。
func Precheck(ctx context.Context, db *store.DB, pkgPath string, opt ImportOptions) (*PrecheckResult, error) {
	if db == nil {
		return nil, errors.New("backup: Precheck 需要 DB")
	}
	opt = opt.withDefaults()

	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("backup: 打开备份包：%w", err)
	}
	defer func() { _ = zr.Close() }()

	// §8.1 第 1 步：定位清单。
	var mf *zip.File
	for _, f := range zr.File {
		if f.Name == ManifestJSON || f.Name == ManifestYAML {
			mf = f
			break
		}
	}
	if mf == nil {
		return nil, ErrManifestMissing
	}

	// §8.1 第 2 步：解析（JSON → YAML 回退）。
	raw, err := readZipEntryLimited(mf, 512<<20)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeManifest(mf.Name, raw)
	if err != nil {
		return nil, err
	}

	// §8.1 第 3 步：格式标识。
	if manifest.Format != FormatTag {
		return nil, fmt.Errorf("%w（format=%q）", ErrNotPawClipBackup, manifest.Format)
	}
	// §8.1 第 4 步：版本。比本机新的包要中止。
	if manifest.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("%w（包 %d > 本机 %d）",
			ErrBackupTooNew, manifest.FormatVersion, FormatVersion)
	}

	pc := &PrecheckResult{
		Path:          pkgPath,
		ManifestName:  mf.Name,
		Format:        manifest.Format,
		FormatVersion: manifest.FormatVersion,
		AppVersion:    manifest.AppVersion,
		ExportedAt:    manifest.ExportedAt.Format(time.RFC3339),
		Platform:      manifest.Platform,
		Scope:         manifest.Scope,
		Total:         len(manifest.Items),
		manifest:      manifest,
	}
	{
		sum := sha256.Sum256(raw)
		pc.ManifestHash = "sha256:" + hex.EncodeToString(sum[:])
	}

	// §8.1 第 5 步：安全校验。
	if err := validateZipEntries(ctx, zr.File, manifest, pc, opt); err != nil {
		return nil, err
	}

	// §8.1 第 6 步：统计预检结果，供确认页展示。
	now := time.Now().Unix()
	for i := range manifest.Items {
		it := &manifest.Items[i]
		if err := it.Validate(); err != nil {
			pc.Invalid++
			continue
		}
		if !opt.ImportExpired && it.ExpiresAt != nil && it.ExpiresAt.Unix() <= now && !it.Pinned {
			pc.SkipExpired++
			continue
		}
		// ⚠️ GetByFingerprint 的契约是"取不到就返回 ErrNotFound"，
		// **不是** (nil, nil)。当成错误往上抛的话，预检会在第一个
		// 新指纹上直接失败——而"库里没有这条"恰恰是导入最常见的情况。
		existing, err := db.GetByFingerprint(ctx, it.Fingerprint)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if existing != nil {
			switch opt.ConflictPolicy {
			case ConflictSkip:
				pc.SkipDuplicate++
			case ConflictOverwrite:
				pc.WillImport++ // 覆盖：先软删旧的再插新的
			default: // merge
				pc.WillImport++
			}
			continue
		}
		pc.WillImport++
	}

	// 分类与标签的映射预览（按名字匹配，§8.2 第 2 步）。
	for _, c := range manifest.Categories {
		hit, err := db.CategoryByName(ctx, c.Name)
		if err != nil {
			return nil, err
		}
		if hit != nil {
			pc.CategoriesReuse = append(pc.CategoriesReuse, c.Name)
		} else {
			pc.CategoriesNew = append(pc.CategoriesNew, c.Name)
		}
	}
	existingTags, err := db.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	haveTag := make(map[string]struct{}, len(existingTags))
	for _, t := range existingTags {
		haveTag[t.Name] = struct{}{}
	}
	for _, t := range manifest.Tags {
		if _, ok := haveTag[t.Name]; ok {
			pc.TagsReuse++
		} else {
			pc.TagsNew++
		}
	}

	// 磁盘空间（§9 最后一条）：预检时检查可用空间 ≥ 解压总大小 × 1.3。
	pc.NeedBytes = int64(float64(pc.UncompressedBytes) * 1.3)
	if avail := availableDiskBytes(pkgPath); avail >= 0 {
		pc.AvailableBytes = avail
		if pc.UncompressedBytes > 0 && avail < pc.NeedBytes {
			return nil, fmt.Errorf("%w：需要约 %d MB，可用 %d MB",
				ErrNoSpace, pc.NeedBytes>>20, avail>>20)
		}
	}

	return pc, nil
}

// decodeManifest 解析清单：JSON 优先，失败回退 YAML（§3.6）。
//
// 回退之后的路径与 YAML 测试里那条一致：YAML → 通用值 → JSON →
// 结构体。这样两种格式共用同一套 struct tag，不会出现
// "JSON 读得进、YAML 读不进"的偏差。
func decodeManifest(name string, raw []byte) (*Manifest, error) {
	var m Manifest
	trimmed := bytes.TrimSpace(raw)

	if name == ManifestJSON || (len(trimmed) > 0 && trimmed[0] == '{') {
		if err := json.Unmarshal(trimmed, &m); err == nil {
			return &m, nil
		} else {
			// 显式 .json 名却解析不了 → 再试 YAML（有人会把 yaml 改名）。
			// 两条都失败才算坏包。
			jsonErr := err
			if v, yerr := parseYAMLSubset(raw); yerr == nil {
				if b, e := json.Marshal(v); e == nil {
					if e := json.Unmarshal(b, &m); e == nil {
						return &m, nil
					}
				}
			}
			return nil, fmt.Errorf("%w：JSON 解析失败（%v）", ErrManifestUnread, jsonErr)
		}
	}

	v, err := parseYAMLSubset(raw)
	if err != nil {
		return nil, fmt.Errorf("%w：YAML 解析失败（%v）", ErrManifestUnread, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrManifestUnread, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w：清单结构不符（%v）", ErrManifestUnread, err)
	}
	return &m, nil
}

// validateZipEntries 做 §9 的条目级安全校验。
func validateZipEntries(
	ctx context.Context,
	files []*zip.File,
	m *Manifest,
	pc *PrecheckResult,
	opt ImportOptions,
) error {
	var totalUncompressed int64

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safeZipEntryName(f.Name); err != nil {
			return err
		}
		// 只统计 blob：清单与 README 不计入解压总量。
		if !strings.HasPrefix(f.Name, BlobDirPrefix) {
			continue
		}
		pc.BlobCount++

		usize := int64(f.UncompressedSize64)
		csize := int64(f.CompressedSize64)
		totalUncompressed += usize

		// 单条目炸弹：解压后超过上限就跳过并计数（§9）。
		if usize > opt.MaxSingleBytes {
			pc.Warnings = append(pc.Warnings,
				fmt.Sprintf("%s 解压后 %d 字节，超过单条目上限 %d，已跳过",
					f.Name, usize, opt.MaxSingleBytes))
			continue
		}
		// 压缩比异常：> 1000 且未压缩 > 10 MB → 拒绝（§9）。
		if csize > 0 && usize > 10<<20 && usize/csize > MaxCompressionRatio {
			return fmt.Errorf("%w：%s 压缩比 %d:1 异常",
				ErrBomb, f.Name, usize/csize)
		}
	}

	pc.UncompressedBytes = totalUncompressed

	// 整包炸弹：声明的总量 × 1.2 或绝对上限 20 GB（§9）。
	cap1 := int64(float64(m.Stats.BlobBytes) * BombRatio)
	if totalUncompressed > MaxTotalUncompressed {
		return fmt.Errorf("%w：解压总量 %d 超过绝对上限 %d",
			ErrBomb, totalUncompressed, MaxTotalUncompressed)
	}
	// 只有当清单声明了 blobBytes 时才用相对上限：某些精简包可能
	// stats 为 0 但确实带 blob，那种情况下硬套 ×1.2 会拒掉合法包。
	if m.Stats.BlobBytes > 0 && totalUncompressed > cap1 {
		return fmt.Errorf("%w：解压总量 %d 超过清单声明 %d 的 %v 倍",
			ErrBomb, totalUncompressed, m.Stats.BlobBytes, BombRatio)
	}
	return nil
}

// safeZipEntryName 拒绝一切非法条目名（§9 的 ZIP Slip 防护）。
//
// 注意：**即使全过了，我们也不拿这个名字去拼落盘路径。**
// 这一层只是尽早报错、把坏包挡在门外；真正的安全来自"目标路径
// 永远由 sha256 重算"。两层都要有。
func safeZipEntryName(name string) error {
	if name == "" {
		return fmt.Errorf("%w：空条目名", ErrUnsafeEntry)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w：条目名含 NUL：%q", ErrUnsafeEntry, name)
	}
	// 反斜杠在 ZIP 里不合规，且 Windows 上会被当分隔符——直接拒。
	if strings.ContainsRune(name, '\\') {
		return fmt.Errorf("%w：条目名含反斜杠：%q", ErrUnsafeEntry, name)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("%w：条目名是绝对路径：%q", ErrUnsafeEntry, name)
	}
	// 盘符（C:）与 Windows 保留写法。
	if len(name) >= 2 && name[1] == ':' {
		return fmt.Errorf("%w：条目名含盘符：%q", ErrUnsafeEntry, name)
	}
	// 逐段检查 `..` 与 `.`。用 path（而不是 filepath）是因为 ZIP 条目名
	// 规范上是斜杠分隔的，filepath 在 Windows 上不认 `/`。
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("%w：条目名含 %q 段：%q", ErrUnsafeEntry, seg, name)
		}
	}
	// 我们只认识这三类条目；别的一律拒（包括符号链接——ZIP 用
	// 外部属性位表达 symlink，我们干脆不收任何非预期条目）。
	if name == ReadmeName || name == ManifestJSON || name == ManifestYAML {
		return nil
	}
	if !strings.HasPrefix(name, BlobDirPrefix) {
		return fmt.Errorf("%w：出现预期外的条目：%q", ErrUnsafeEntry, name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("%w：条目名不规范：%q", ErrUnsafeEntry, name)
	}
	return nil
}

// readZipEntryLimited 读取 zip 条目的内容，带上限（防止清单本身是个炸弹）。
func readZipEntryLimited(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("backup: 打开 %s：%w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("backup: 读取 %s：%w", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w：%s 超过 %d 字节", ErrBomb, f.Name, limit)
	}
	return data, nil
}

// ── 阶段二：写入 ────────────────────────────────────────────────

// Import 执行导入。
//
// pc 可以传 nil（内部会自己跑一次预检）；传进来可以省一次扫描，
// 也保证"确认页看到的"与"实际执行的"是同一份数据。
func Import(
	ctx context.Context,
	db *store.DB,
	blobs *store.BlobStore,
	pkgPath string,
	opt ImportOptions,
	pc *PrecheckResult,
	log *slog.Logger,
) (*ImportResult, error) {
	if db == nil || blobs == nil {
		return nil, errors.New("backup: Import 需要 DB 与 BlobStore")
	}
	if log == nil {
		log = slog.Default()
	}
	opt = opt.withDefaults()
	start := time.Now()

	if pc == nil {
		var err error
		pc, err = Precheck(ctx, db, pkgPath, opt)
		if err != nil {
			return nil, err
		}
	}
	manifest := pc.manifest
	if manifest == nil {
		return nil, errors.New("backup: 预检结果没有清单（请重新运行 Precheck）")
	}

	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("backup: 打开备份包：%w", err)
	}
	defer func() { _ = zr.Close() }()

	res := &ImportResult{}

	// §8.2 第 1 步：先建批次行。中途崩溃留下的是 running 记录，
	// 用户能看到"上次导入没跑完"，而不是无声无息。
	importID, err := db.CreateImport(ctx, path.Base(pkgPath), pc.ManifestHash)
	if err != nil {
		return nil, err
	}
	res.ImportID = importID

	// §8.2 第 2/3 步：分类与标签映射（按名字，不按 ID）。
	catMap, catsMade, err := mapCategories(ctx, db, manifest, opt)
	if err != nil {
		_ = db.FinishImport(ctx, importID, 0, 0, 0, store.ImportFailed)
		return nil, err
	}
	res.CategoriesMade = catsMade
	tagMap, tagsMade, err := mapTags(ctx, db, manifest)
	if err != nil {
		_ = db.FinishImport(ctx, importID, 0, 0, 0, store.ImportFailed)
		return nil, err
	}
	res.TagsMade = tagsMade

	// blob 索引：条目名 → zip.File。只用于**读取定位**。
	blobIndex := make(map[string]*zip.File, pc.BlobCount)
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, BlobDirPrefix) {
			blobIndex[f.Name] = f
		}
	}

	// §8.2 第 4 步：逐条处理。每 500 条提交一次。
	now := time.Now().Unix()
	batchCount := 0
	var tx *sql.Tx

	beginTx := func() error {
		var err error
		tx, err = db.Writer().BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("backup: 开启写入事务：%w", err)
		}
		batchCount = 0
		return nil
	}
	commitTx := func() error {
		if tx == nil {
			return nil
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("backup: 提交写入事务：%w", err)
		}
		tx = nil
		return nil
	}

	if err := beginTx(); err != nil {
		_ = db.FinishImport(ctx, importID, 0, 0, 0, store.ImportFailed)
		return nil, err
	}

	for i := range manifest.Items {
		if err := ctx.Err(); err != nil {
			_ = tx.Rollback()
			_ = db.FinishImport(ctx, importID,
				int64(res.Imported), int64(res.Skipped), int64(res.Failed), store.ImportPartial)
			return nil, err
		}

		it := &manifest.Items[i]
		outcome, err := importOne(ctx, db, blobs, tx, blobIndex, it, catMap, tagMap, importID, opt, now, res)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("条目 %d：%v", it.ID, err))
			// 累计失败超过容忍度就中止：一个坏包不该把磁盘跑满（§8.3）。
			if res.Failed >= opt.ErrorTolerance {
				_ = tx.Rollback()
				status := store.ImportPartial
				if res.Imported == 0 {
					status = store.ImportFailed
				}
				_ = db.FinishImport(ctx, importID,
					int64(res.Imported), int64(res.Skipped), int64(res.Failed), status)
				return nil, fmt.Errorf("backup: 失败数达到容忍上限 %d，已中止", opt.ErrorTolerance)
			}
			continue
		}
		switch outcome {
		case outcomeImported:
			res.Imported++
		case outcomeMerged:
			res.Merged++
			res.Imported++
		case outcomeOverwritten:
			res.Overwritten++
			res.Imported++
		case outcomeSkipped:
			res.Skipped++
		}

		batchCount++
		if batchCount >= importCommitEvery {
			if err := commitTx(); err != nil {
				_ = db.FinishImport(ctx, importID,
					int64(res.Imported), int64(res.Skipped), int64(res.Failed), store.ImportPartial)
				return nil, err
			}
			if err := beginTx(); err != nil {
				_ = db.FinishImport(ctx, importID,
					int64(res.Imported), int64(res.Skipped), int64(res.Failed), store.ImportPartial)
				return nil, err
			}
		}
	}

	if err := commitTx(); err != nil {
		_ = db.FinishImport(ctx, importID,
			int64(res.Imported), int64(res.Skipped), int64(res.Failed), store.ImportPartial)
		return nil, err
	}

	// §8.2 第 5 步：收尾状态。
	status := store.ImportOK
	switch {
	case res.Failed > 0 && res.Imported > 0:
		status = store.ImportPartial
	case res.Failed > 0 && res.Imported == 0:
		status = store.ImportFailed
	}
	if err := db.FinishImport(ctx, importID,
		int64(res.Imported), int64(res.Skipped), int64(res.Failed), status); err != nil {
		return nil, err
	}
	res.Status = status
	res.RollbackPossible = res.Imported > 0

	// §8.2 第 6 步：FTS 行数比对（触发器应当已经处理）。
	//
	// ⚠️ 对照量必须是 **items 总行数**，不是存活条目数。
	// 这是 external content 表的一个反直觉之处：软删除走的是 UPDATE，
	// 而 items_au 触发器会把那一行"先删后插"回索引里 —— 也就是
	//
	//	软删除一条 → items 行数不变、存活数 -1、**FTS 行数不变**
	//
	// 第一版这里比的是 CountAlive，于是在 overwrite 策略（先软删旧行再插
	// 新行）下必然误报 "fts=14 alive=7"。误报的危害不只是噪音：
	// 它会把真正的不一致淹掉。
	//
	// 检索侧不受影响：查询里 JOIN items 时会带 deleted_at IS NULL 过滤，
	// 软删除的条目查不出来（见 store/query.go 的 buildFilters）。
	if n, err := db.FTSRowCount(ctx); err == nil {
		if total, err := db.CountAll(ctx); err == nil && n != total {
			res.Errors = append(res.Errors,
				fmt.Sprintf("FTS 索引行数（%d）与 items 总行数（%d）不一致", n, total))
			log.Warn("导入后 FTS 行数与 items 总行数不一致", "fts", n, "items", total)
		}
	}

	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

type importOutcome int

const (
	outcomeImported importOutcome = iota
	outcomeMerged
	outcomeOverwritten
	outcomeSkipped
)

// importOne 处理一条条目（§8.2 第 4 步的 a–f）。
func importOne(
	ctx context.Context,
	db *store.DB,
	blobs *store.BlobStore,
	tx *sql.Tx,
	blobIndex map[string]*zip.File,
	src *Item,
	catMap, tagMap map[int64]int64,
	importID int64,
	opt ImportOptions,
	now int64,
	res *ImportResult,
) (importOutcome, error) {
	// a. 校验指纹。
	if err := src.Validate(); err != nil {
		return outcomeSkipped, fmt.Errorf("清单校验不通过：%w", err)
	}

	// b. 计算最终 expires_at。
	var expiresAt *int64
	switch opt.ExpiryPolicy {
	case ExpiryReset:
		if src.TTLSeconds != nil && !src.Pinned {
			v := now + *src.TTLSeconds
			expiresAt = &v
		}
	default: // absolute
		if src.ExpiresAt != nil && !src.Pinned {
			v := src.ExpiresAt.Unix()
			expiresAt = &v
		}
	}
	if expiresAt != nil && *expiresAt <= now && !opt.ImportExpired {
		return outcomeSkipped, nil
	}

	// c. 查重 + 冲突策略。
	//
	// ⚠️ 这三步全部走 **tx**，一次都不能落回 db.*：
	//   - GetByFingerprintTx：走 d.r（另一个连接池）会看不见本事务刚插入的
	//     行，于是同一批里两条同指纹的条目第一次查不到、第二次才撞唯一索引；
	//   - SoftDeleteTx / MergeIntoExistingTx：走 d.w 会向
	//     SetMaxOpenConns(1) 的池再要一条连接，而那条已经被本事务占住 ——
	//     **永久死锁**（第二次导入全部走 merge，所以正好在这里挂死）。
	//
	// ErrNotFound 表示"本机没有这条"，是正常路径而不是错误。
	existing, err := store.GetByFingerprintTx(ctx, tx, src.Fingerprint)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return outcomeSkipped, err
	}
	overwriteID := int64(0)
	if existing != nil {
		switch opt.ConflictPolicy {
		case ConflictSkip:
			return outcomeSkipped, nil
		case ConflictOverwrite:
			if _, err := db.SoftDeleteTx(ctx, tx, []int64{existing.ID}); err != nil {
				return outcomeSkipped, err
			}
			overwriteID = existing.ID
		default: // merge
			// "累加 use_count、取较新的 lastUsedAt、**保留本机分类**"。
			// 保留本机分类容易被写漏：那条已经在用户的分类体系里了，
			// 拿包里的 categoryId 覆盖会把用户的整理结果冲掉。
			lu := int64(0)
			if src.LastUsedAt != nil {
				lu = src.LastUsedAt.Unix()
			}
			if err := db.MergeIntoExistingTx(ctx, tx, existing.ID, src.UseCount, lu); err != nil {
				return outcomeSkipped, err
			}
			return outcomeMerged, nil
		}
	}

	// e. blob：逐个读出、核对 sha256、按 sha 重算落盘路径。
	var (
		imageRel, rtfRel, thumbRel string
		htmlInline                 *string
	)
	for _, ref := range src.Blobs {
		if ref.Role == roleHTML {
			// 外置的 HTML：读回来内联进 html_content（库里没有 html_path 列）。
			data, err := readAndVerifyBlob(blobIndex, ref, opt)
			if err != nil {
				res.BlobsSkipped++
				res.Errors = append(res.Errors, err.Error())
				continue
			}
			s := string(data)
			htmlInline = &s
			res.BlobsWritten++
			continue
		}

		data, err := readAndVerifyBlob(blobIndex, ref, opt)
		if err != nil {
			// 单条 blob 坏了不该让整条条目失败到"什么都没导入"：
			// 文本部分通常还在，用户至少能拿到它。
			res.BlobsSkipped++
			res.Errors = append(res.Errors, err.Error())
			continue
		}

		ext := ExtForMime(ref.Mime)
		rel, shaHex, err := blobs.Put(data, ext)
		if err != nil {
			return outcomeSkipped, err
		}
		res.BlobsWritten++

		switch ref.Role {
		case roleImage:
			imageRel = rel
			// 缩略图在导入后**重新生成**（§5：导出时不带，导入后自动重建）。
			// PutThumb 内部自己会缩并写盘，所以不用先调 MakeThumbPNG 再传进去——
			// 那样等于把缩略图算两遍。
			if trel, _, _, err := blobs.PutThumb(data, shaHex); err == nil {
				thumbRel = trel
				res.ThumbsMade++
			} else {
				// 缩略图失败不该让整条导入失败：原图已经落盘，
				// 没有缩略图只是列表里预览大一点。
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("条目 %d 的缩略图生成失败：%v", src.ID, err))
			}
		case roleRTF:
			rtfRel = rel
		}
	}

	// d. 重映射 categoryId / tagIds。
	var catID *int64
	if src.CategoryID != nil {
		if nv, ok := catMap[*src.CategoryID]; ok {
			v := nv
			catID = &v
		}
	}
	var tagIDs []int64
	for _, old := range src.TagIDs {
		if nv, ok := tagMap[old]; ok {
			tagIDs = append(tagIDs, nv)
		}
	}

	// f. 插入。
	it := store.Item{
		Kind:          src.Kind,
		TextContent:   src.Text,
		HTMLContent:   firstNonNil(src.HTML, htmlInline),
		FilePaths:     src.FilePaths,
		Preview:       src.Preview,
		Fingerprint:   src.Fingerprint,
		ByteSize:      src.ByteSize,
		SourceAppID:   src.SourceAppID,
		SourceAppName: src.SourceAppName,
		SourceURL:     src.SourceURL,
		CategoryID:    catID,
		Pinned:        src.Pinned,
		FirstSeenAt:   src.FirstSeenAt.Unix(),
		CreatedAt:     src.CreatedAt.Unix(),
		UseCount:      src.UseCount,
		ExpiresAt:     expiresAt,
		TTLSource:     ttlSourceFor(src, expiresAt),
		ImportID:      &importID,
	}
	if imageRel != "" {
		it.ImagePath = &imageRel
	}
	if thumbRel != "" {
		it.ThumbPath = &thumbRel
	}
	if rtfRel != "" {
		it.RTFPath = &rtfRel
	}
	if src.LastUsedAt != nil {
		v := src.LastUsedAt.Unix()
		it.LastUsedAt = &v
	}

	id, err := db.InsertImportedItem(ctx, tx, &it)
	if err != nil {
		if isUniqueViolation(err) {
			// 同一批次里出现了两条同指纹的条目：预检用的是只读连接，
			// 看不到本事务内还没提交的插入，所以这里会撞唯一索引。
			// 按"重复"处理而不是失败——这本来就是包自己的问题。
			return outcomeSkipped, nil
		}
		return outcomeSkipped, err
	}

	if len(tagIDs) > 0 {
		if err := db.SetItemTagsTx(ctx, tx, id, tagIDs); err != nil {
			return outcomeSkipped, err
		}
	}

	if overwriteID != 0 {
		return outcomeOverwritten, nil
	}
	return outcomeImported, nil
}

// firstNonNil 返回第一个非 nil 的字符串指针。
func firstNonNil(a, b *string) *string {
	if a != nil {
		return a
	}
	return b
}

// ttlSourceFor 推出 ttl_source 该填什么。
func ttlSourceFor(src *Item, expiresAt *int64) *string {
	v := store.TTLSourceGlobal
	switch {
	case src.Pinned || expiresAt == nil:
		v = store.TTLSourceNever
	case src.ExpiresAt != nil:
		// 包里带来了绝对到期时间 = 源机器上的显式设定。
		v = store.TTLSourceItem
	}
	return &v
}

// readAndVerifyBlob 从包里读出 blob 并核对 sha256（§8.2 第 4e 步）。
//
// 哈希不符就**弃用**（不写入半截 blob）——这是把"坏数据"挡在库外
// 的最后一道闸。
func readAndVerifyBlob(blobIndex map[string]*zip.File, ref BlobRef, opt ImportOptions) ([]byte, error) {
	f, ok := blobIndex[ref.Path]
	if !ok {
		return nil, fmt.Errorf("包里缺少 blob %s", ref.Path)
	}
	if int64(f.UncompressedSize64) > opt.MaxSingleBytes {
		return nil, fmt.Errorf("blob %s 解压后 %d 字节，超过单条目上限", ref.Path, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("打开 blob %s：%w", ref.Path, err)
	}
	defer func() { _ = rc.Close() }()

	// 用 LimitReader 兜住"声明的大小与实际不符"：zip 的解压流长度
	// 以中央目录声明为准，但恶意包可以声明得很小而实际吐很多。
	data, err := io.ReadAll(io.LimitReader(rc, opt.MaxSingleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 blob %s：%w", ref.Path, err)
	}
	if int64(len(data)) > opt.MaxSingleBytes {
		return nil, fmt.Errorf("blob %s 实际解压超过单条目上限", ref.Path)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, ref.SHA256) {
		return nil, fmt.Errorf("blob %s 的 sha256 不符（清单 %s，实际 %s），已弃用", ref.Path, ref.SHA256, got)
	}
	return data, nil
}

// mapCategories 建立 old_id → new_id 映射（§8.2 第 2 步）。
//
// 按 **name** 匹配而不是 ID：两个机器上的 ID 体系完全无关。
func mapCategories(ctx context.Context, db *store.DB, m *Manifest, opt ImportOptions) (map[int64]int64, int, error) {
	out := make(map[int64]int64, len(m.Categories))
	made := 0
	for _, c := range m.Categories {
		hit, err := db.CategoryByName(ctx, c.Name)
		if err != nil {
			return nil, 0, err
		}
		if hit != nil {
			// 同名复用。createAll 在这里也只有这一种可能：categories.name
			// 是 UNIQUE 约束，硬要再建一个同名行会被数据库拒绝。
			// 所以两种策略的实现是同一段代码，差别只在文档语义上。
			out[c.ID] = hit.ID
			continue
		}
		nc := store.Category{
			Name:       c.Name,
			Color:      c.Color,
			Icon:       c.Icon,
			SortOrder:  c.SortOrder,
			TTLSeconds: c.TTLSeconds,
		}
		if len(c.Rule) > 0 {
			nc.Rule = string(c.Rule)
		}
		id, err := db.CreateCategory(ctx, &nc)
		if err != nil {
			return nil, made, err
		}
		out[c.ID] = id
		made++
	}
	return out, made, nil
}

// mapTags 建立 old_id → new_id 映射（§8.2 第 3 步）。
func mapTags(ctx context.Context, db *store.DB, m *Manifest) (map[int64]int64, int, error) {
	out := make(map[int64]int64, len(m.Tags))
	made := 0
	for _, t := range m.Tags {
		id, created, err := db.EnsureTag(ctx, t.Name, t.Color)
		if err != nil {
			return nil, made, err
		}
		if created {
			made++
		}
		out[t.ID] = id
	}
	return out, made, nil
}

// ── 回滚（§8.5）────────────────────────────────────────────────

// Rollback 撤销一次导入：删掉该批次的条目。
//
// 只删**记录**，不删 blob 文件——blobs/ 是内容寻址的，同一份字节可能
// 被别的条目引用；文件回收交给 GC 的孤儿扫描。
func Rollback(ctx context.Context, db *store.DB, importID int64) (int64, error) {
	if db == nil {
		return 0, errors.New("backup: Rollback 需要 DB")
	}
	return db.DeleteImportItems(ctx, importID)
}

// isUniqueViolation 判断是否撞了唯一索引。
//
// store 包里有一个同名私有函数，但跨包用不了。这里用错误文本判定：
// mattn/go-sqlite3 对唯一约束报的是 "UNIQUE constraint failed: ..."。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: UNIQUE")
}

// availableDiskBytes 返回目标路径所在卷的可用字节数。
//
// 取不到时返回 -1（调用方据此跳过空间检查，而不是当成 0 拒绝导入——
// 有些平台的 syscall 在容器/网络盘上会失败，那时"拒绝"比"放行"糟糕得多）。
func availableDiskBytes(p string) int64 {
	dir := path.Dir(p)
	if dir == "" {
		dir = "."
	}
	if _, err := os.Stat(dir); err != nil {
		return -1
	}
	return diskFree(dir)
}
