package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zego/pawclip/pinyin"
)

// ErrNotFound 表示按条件没查到条目。
var ErrNotFound = errors.New("store: item not found")

// Item 是 items 表的一行（docs/DESIGN.md §4.1）。
//
// 可空列用指针表达，避免 0 / "" 与 NULL 混淆——expires_at 的 NULL 正是
// "永不过期"的语义，不能被压成 0。
type Item struct {
	ID            int64
	Kind          string
	TextContent   *string
	HTMLContent   *string
	RTFPath       *string
	ImagePath     *string
	ThumbPath     *string
	FilePaths     []string
	Preview       string
	Fingerprint   string
	ByteSize      int64
	SourceAppID   string
	SourceAppName string
	SourceURL     *string
	CategoryID    *int64
	Pinned        bool
	FirstSeenAt   int64
	ExpiresAt     *int64
	TTLSource     *string
	CreatedAt     int64
	LastUsedAt    *int64
	UseCount      int64
	DeletedAt     *int64
	ImportID      *int64
}

// itemColumns 与 scanItem 的顺序必须严格一致。
const itemColumns = `id, kind, text_content, html_content, rtf_path, image_path, thumb_path,
	file_paths, preview, fingerprint, byte_size, source_app_id, source_app_name, source_url,
	category_id, pinned, first_seen_at, expires_at, ttl_source, created_at, last_used_at,
	use_count, deleted_at, import_id`

// upsertItemSQL 是写入路径的核心（docs/DESIGN.md §4.3 / docs/HANDOFF-PROMPT.md §4 第 3 条）。
//
// 三个不能改的细节：
//
//  1. **冲突目标必须带 `WHERE deleted_at IS NULL`**。uq_items_fp_alive 是
//     partial unique index，不带这个 WHERE 子句 SQLite 匹配不到那个索引，
//     会直接报 "ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint"。
//  2. **不更新 first_seen_at**。它是"首次复制时间"，重复复制（置顶）不该改写它。
//  3. **不更新 expires_at / ttl_source / pinned**。用户单条设定的过期策略
//     必须凌驾于自动重算之上，而同一指纹的内容本来就一样，没有重算的必要。
//
// use_count 的初值是**写死的 1**，不由调用方传：一次成功落库本身就代表
// "这份内容被复制了一次"。于是"同一内容复制 N 次 → use_count = N"
// （验收判据 2）成为存储层的不变量，调用方忘了带计数也不会算错。
//
// created_at 被复用为排序键（列表按 created_at DESC），重复复制时把它刷新为
// now，条目就"置顶"到时间线最前。
const upsertItemSQL = `
INSERT INTO items (
  kind, text_content, html_content, rtf_path, image_path, thumb_path, file_paths,
  preview, pinyin, fingerprint, byte_size, source_app_id, source_app_name, source_url,
  category_id, pinned, first_seen_at, expires_at, ttl_source, created_at,
  last_used_at, use_count
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 1)
ON CONFLICT(fingerprint) WHERE deleted_at IS NULL DO UPDATE SET
  created_at      = excluded.created_at,
  last_used_at    = excluded.last_used_at,
  use_count       = items.use_count + 1,
  source_app_id   = excluded.source_app_id,
  source_app_name = excluded.source_app_name
RETURNING id, use_count, first_seen_at
`

// itemPinyin 算出该写进 items.pinyin 的首字母串。
//
// 只取 preview：拼音搜索是"想起来大概怎么念"的模糊入口，preview 已经足够
// 承载它，而且 preview 有长度上界，pinyin 列不会因此无界膨胀
// （10 万条 @ 100 字符 ≈ 10 MB，可接受；把 text_content 全长算进去就不一定了）。
func itemPinyin(it *Item) string {
	return pinyin.Initials(it.Preview)
}

// RowQuerier 是"查一行"的最小接口。
//
// 单独提出来（而不是复用 Execer）是为了让**只读**函数的签名诚实：
// GetByFingerprintTx 只需要 QueryRowContext，不该顺带要求写能力——
// 一个只读的调用点拿着一个"能写"的句柄，是误写的温床。
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Execer 抽象 *sql.DB 与 *sql.Tx，让写入路径既能单条提交也能进合批事务。
//
// ⚠️ 事务内的写操作**必须**收 Execer（而不是在内部直接用 d.w）：
// 写句柄是 SetMaxOpenConns(1) 的单连接池，事务已经占住了那唯一一条连接，
// 内部再走 d.w 就是向池里要第二条连接 —— 永久死锁，且不报错、不超时。
type Execer interface {
	RowQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// PutResult 是 upsert 的结果。
type PutResult struct {
	ID          int64
	UseCount    int64
	FirstSeenAt int64
}

// PutItem 执行一次去重式写入：指纹命中存活条目则置顶并累加计数，否则新插入。
func PutItem(ctx context.Context, ex Execer, it *Item) (PutResult, error) {
	if it == nil {
		return PutResult{}, errors.New("store: nil item")
	}
	if it.Fingerprint == "" {
		return PutResult{}, errors.New("store: item without fingerprint")
	}
	if it.Kind == "" {
		return PutResult{}, errors.New("store: item without kind")
	}

	filePaths, err := encodeFilePaths(it.FilePaths)
	if err != nil {
		return PutResult{}, err
	}

	var res PutResult
	err = ex.QueryRowContext(ctx, upsertItemSQL,
		it.Kind,
		nullableString(it.TextContent),
		nullableString(it.HTMLContent),
		nullableString(it.RTFPath),
		nullableString(it.ImagePath),
		nullableString(it.ThumbPath),
		filePaths,
		it.Preview,
		// pinyin 由**存储层自己算**，不由调用方传。
		//
		// 与 use_count 写死为 1 同一个理由：这是表的不变量，不是调用方的
		// 责任。放在这里之后，捕获路径、导入路径、测试辅助都自动带上，
		// 不存在"某条新路径忘了写、于是那条内容用首字母搜不到"的可能 ——
		// 而漏掉一个调用点是不会让任何测试变红的。
		itemPinyin(it),
		it.Fingerprint,
		it.ByteSize,
		nullableString(strPtr(it.SourceAppID)),
		nullableString(strPtr(it.SourceAppName)),
		nullableString(it.SourceURL),
		nullableInt64(it.CategoryID),
		boolToInt(it.Pinned),
		it.FirstSeenAt,
		nullableInt64(it.ExpiresAt),
		nullableString(it.TTLSource),
		it.CreatedAt,
		nullableInt64(it.LastUsedAt),
	).Scan(&res.ID, &res.UseCount, &res.FirstSeenAt)
	if err != nil {
		return PutResult{}, fmt.Errorf("store: upsert item: %w", err)
	}
	return res, nil
}

func encodeFilePaths(paths []string) (any, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("store: encode file_paths: %w", err)
	}
	return string(b), nil
}

func decodeFilePaths(s sql.NullString) ([]string, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil, fmt.Errorf("store: decode file_paths: %w", err)
	}
	return out, nil
}

// ── 查询辅助（M1 只需要计数与按指纹取回，检索留给 M2）─────────────

// CountAlive 返回存活（未软删）条目数。
func (d *DB) CountAlive(ctx context.Context) (int64, error) {
	return d.count(ctx, "SELECT count(*) FROM items WHERE deleted_at IS NULL")
}

// CountAll 返回全部条目数，含回收站。
func (d *DB) CountAll(ctx context.Context) (int64, error) {
	return d.count(ctx, "SELECT count(*) FROM items")
}

// CountTrashed 返回回收站条目数。
func (d *DB) CountTrashed(ctx context.Context) (int64, error) {
	return d.count(ctx, "SELECT count(*) FROM items WHERE deleted_at IS NOT NULL")
}

// TotalAliveBytes 返回存活条目登记的 byte_size 之和。
func (d *DB) TotalAliveBytes(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := d.r.QueryRowContext(ctx,
		"SELECT sum(byte_size) FROM items WHERE deleted_at IS NULL").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: sum byte_size: %w", err)
	}
	return n.Int64, nil
}

func (d *DB) count(ctx context.Context, query string) (int64, error) {
	var n int64
	if err := d.r.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: %s: %w", query, err)
	}
	return n, nil
}

// GetByFingerprint 按指纹取存活条目。
func (d *DB) GetByFingerprint(ctx context.Context, fp string) (*Item, error) {
	row := d.r.QueryRowContext(ctx,
		"SELECT "+itemColumns+" FROM items WHERE fingerprint = ? AND deleted_at IS NULL", fp)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// GetByID 按主键取条目（含回收站）。
func (d *DB) GetByID(ctx context.Context, id int64) (*Item, error) {
	row := d.r.QueryRowContext(ctx,
		"SELECT "+itemColumns+" FROM items WHERE id = ?", id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// rowScanner 覆盖 *sql.Row 与 *sql.Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (*Item, error) {
	var (
		it                                      Item
		text, html, rtfPath, imgPath, thumbPath sql.NullString
		filePaths, preview                      sql.NullString
		fp, kind                                string
		srcID, srcName, srcURL                  sql.NullString
		categoryID, expiresAt, lastUsedAt       sql.NullInt64
		deletedAt, importID                     sql.NullInt64
		ttlSource                               sql.NullString
		pinned                                  int64
	)
	err := row.Scan(
		&it.ID, &kind, &text, &html, &rtfPath, &imgPath, &thumbPath,
		&filePaths, &preview, &fp, &it.ByteSize, &srcID, &srcName, &srcURL,
		&categoryID, &pinned, &it.FirstSeenAt, &expiresAt, &ttlSource, &it.CreatedAt,
		&lastUsedAt, &it.UseCount, &deletedAt, &importID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("store: scan item: %w", err)
	}
	it.Kind = kind
	it.Fingerprint = fp
	it.Preview = preview.String
	it.TextContent = nullStringPtr(text)
	it.HTMLContent = nullStringPtr(html)
	it.RTFPath = nullStringPtr(rtfPath)
	it.ImagePath = nullStringPtr(imgPath)
	it.ThumbPath = nullStringPtr(thumbPath)
	it.SourceAppID = srcID.String
	it.SourceAppName = srcName.String
	it.SourceURL = nullStringPtr(srcURL)
	it.CategoryID = nullInt64Ptr(categoryID)
	it.ExpiresAt = nullInt64Ptr(expiresAt)
	it.LastUsedAt = nullInt64Ptr(lastUsedAt)
	it.DeletedAt = nullInt64Ptr(deletedAt)
	it.ImportID = nullInt64Ptr(importID)
	it.TTLSource = nullStringPtr(ttlSource)
	it.Pinned = pinned != 0
	if it.FilePaths, err = decodeFilePaths(filePaths); err != nil {
		return nil, err
	}
	return &it, nil
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func strPtr(s string) *string { return &s }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
