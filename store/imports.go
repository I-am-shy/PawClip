package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 本文件是 .clipbak 导入所需的存储支撑（docs/BACKUP-FORMAT.md §8）。
//
// 与 items.go 的分工：items.go 是**捕获路径**的写入（去重式 upsert，
// use_count 由存储层自己算）；这里是**导入路径**的写入（原样插入，
// use_count 从包里带过来，并记 import_id）。两者刻意不共用一条 SQL：
// 导入要保留源机器的 use_count、first_seen_at，还要能按批次回滚，
// 而捕获路径恰恰**不能**让调用方决定这些值。

// ImportStatus 是 imports.status 的取值域（§4.1 的注释）。
const (
	ImportRunning    = "running"
	ImportOK         = "ok"
	ImportPartial    = "partial"
	ImportFailed     = "failed"
	ImportRolledBack = "rolled_back"
)

// ImportRow 是 imports 表的一行。
type ImportRow struct {
	ID           int64  `json:"id"`
	SourceName   string `json:"sourceName"`
	ManifestHash string `json:"manifestHash"`
	StartedAt    int64  `json:"startedAt"`
	FinishedAt   *int64 `json:"finishedAt"`
	Imported     int64  `json:"imported"`
	Skipped      int64  `json:"skipped"`
	Failed       int64  `json:"failed"`
	Status       string `json:"status"`
}

// CreateImport 开一条导入批次，status = running。
//
// 先建行再写数据（§8.2 第 1 步）：中途崩溃留下的也是一条 running 记录，
// 用户能看到"上次导入没跑完"，而不是无声无息。
func (d *DB) CreateImport(ctx context.Context, sourceName, manifestHash string) (int64, error) {
	res, err := d.w.ExecContext(ctx,
		`INSERT INTO imports (source_name, manifest_hash, started_at, imported, skipped, failed, status)
		 VALUES (?, ?, ?, 0, 0, 0, ?)`,
		sourceName, manifestHash, time.Now().Unix(), ImportRunning)
	if err != nil {
		return 0, fmt.Errorf("store: create import: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create import id: %w", err)
	}
	return id, nil
}

// FinishImport 收尾一条导入批次（§8.2 第 5 步）。
func (d *DB) FinishImport(ctx context.Context, id, imported, skipped, failed int64, status string) error {
	if _, err := d.w.ExecContext(ctx,
		`UPDATE imports SET finished_at = ?, imported = ?, skipped = ?, failed = ?, status = ?
		  WHERE id = ?`,
		time.Now().Unix(), imported, skipped, failed, status, id); err != nil {
		return fmt.Errorf("store: finish import: %w", err)
	}
	return nil
}

// GetImport 读一条批次。
func (d *DB) GetImport(ctx context.Context, id int64) (*ImportRow, error) {
	row := d.r.QueryRowContext(ctx,
		`SELECT id, COALESCE(source_name,''), COALESCE(manifest_hash,''), started_at,
		        finished_at, imported, skipped, failed, status
		   FROM imports WHERE id = ?`, id)
	return scanImport(row)
}

// LastImport 读最近一条**可回滚**的批次（用于"撤销上次导入"）。
//
// 只认 status = ok / partial：rolled_back 的再回滚一次没有意义，
// running 的说明进程中途死了，让它留着给用户看。
func (d *DB) LastImport(ctx context.Context) (*ImportRow, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT id, COALESCE(source_name,''), COALESCE(manifest_hash,''), started_at,
		        finished_at, imported, skipped, failed, status
		   FROM imports
		  WHERE status IN (?, ?)
		  ORDER BY started_at DESC, id DESC LIMIT 1`, ImportOK, ImportPartial)
	if err != nil {
		return nil, fmt.Errorf("store: last import: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return scanImport(rows)
}

// ListImports 列最近的批次（统计面板用）。
func (d *DB) ListImports(ctx context.Context, limit int) ([]ImportRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.r.QueryContext(ctx,
		`SELECT id, COALESCE(source_name,''), COALESCE(manifest_hash,''), started_at,
		        finished_at, imported, skipped, failed, status
		   FROM imports ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list imports: %w", err)
	}
	defer rows.Close()
	var out []ImportRow
	for rows.Next() {
		r, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

type importScanner interface {
	Scan(dest ...any) error
}

func scanImport(row importScanner) (*ImportRow, error) {
	var (
		r        ImportRow
		finished sql.NullInt64
	)
	if err := row.Scan(&r.ID, &r.SourceName, &r.ManifestHash, &r.StartedAt,
		&finished, &r.Imported, &r.Skipped, &r.Failed, &r.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: scan import: %w", err)
	}
	r.FinishedAt = nullInt64Ptr(finished)
	return &r, nil
}

// DeleteImportItems 删掉某批次导入的**全部条目**（§8.5 一键回滚）。
//
// ⚠️ 与 Purge 一样**不删 blob 文件**：blobs/ 是内容寻址的，同一份字节
// 可能被别的条目引用。文件回收交给 GC 的孤儿扫描。
// 返回删掉的条数；同时把批次标记成 rolled_back。
func (d *DB) DeleteImportItems(ctx context.Context, importID int64) (int64, error) {
	res, err := d.w.ExecContext(ctx, "DELETE FROM items WHERE import_id = ?", importID)
	if err != nil {
		return 0, fmt.Errorf("store: delete import items: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete import items rows affected: %w", err)
	}
	if _, err := d.w.ExecContext(ctx,
		`UPDATE imports SET status = ?, finished_at = ? WHERE id = ?`,
		ImportRolledBack, time.Now().Unix(), importID); err != nil {
		return n, fmt.Errorf("store: mark import rolled back: %w", err)
	}
	return n, nil
}

// PruneImports 清理超过保留期的批次记录（§8.5：30 天后回滚入口消失）。
//
// 只删**记录**，不动 items：超过 30 天还能回滚才叫危险。
func (d *DB) PruneImports(ctx context.Context, before int64) (int64, error) {
	// 先解除 items 对批次的引用，再把记录删掉——import_id 是
	// ON DELETE SET NULL，但显式做一遍更清楚，也避免触发外键级联的开销。
	if _, err := d.w.ExecContext(ctx,
		`UPDATE items SET import_id = NULL
		  WHERE import_id IN (SELECT id FROM imports WHERE started_at < ?)`, before); err != nil {
		return 0, fmt.Errorf("store: detach pruned imports: %w", err)
	}
	res, err := d.w.ExecContext(ctx, "DELETE FROM imports WHERE started_at < ?", before)
	if err != nil {
		return 0, fmt.Errorf("store: prune imports: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// InsertImportedItem 原样插入一条导入的条目（不走去重 upsert）。
//
// 与 PutItem 的三点差别，每一点都有理由：
//
//  1. **保留源机器的 use_count / first_seen_at / last_used_at**。
//     导入的语义是"把这些数据搬过来"，不是"我刚复制了一堆东西"。
//  2. **写 import_id**，否则一键回滚无从下手。
//  3. **刷新 FTS** 靠触发器（items_ai 已在 schema.go 建好），这里不用管。
//
// 冲突（指纹已被存活条目占用）由调用方按 conflictPolicy 处理：
// 直接调用本函数会撞 uq_items_fp_alive 并返回错误，这是**有意的**——
// 让"策略没想清楚"立刻暴露，而不是静默产生重复行。
func (d *DB) InsertImportedItem(ctx context.Context, ex Execer, it *Item) (int64, error) {
	if it == nil {
		return 0, errors.New("store: nil imported item")
	}
	filePaths, err := encodeFilePaths(it.FilePaths)
	if err != nil {
		return 0, err
	}
	var id int64
	err = ex.QueryRowContext(ctx, `
INSERT INTO items (
  kind, text_content, html_content, rtf_path, image_path, thumb_path, file_paths,
  preview, fingerprint, byte_size, source_app_id, source_app_name, source_url,
  category_id, pinned, first_seen_at, expires_at, ttl_source, created_at,
  last_used_at, use_count, deleted_at, import_id
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?)
RETURNING id`,
		it.Kind,
		nullableString(it.TextContent),
		nullableString(it.HTMLContent),
		nullableString(it.RTFPath),
		nullableString(it.ImagePath),
		nullableString(it.ThumbPath),
		filePaths,
		it.Preview,
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
		it.UseCount,
		nullableInt64(it.ImportID),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert imported item: %w", err)
	}
	return id, nil
}

// MergeIntoExisting 把导入条目的使用统计并进本机已有的同指纹条目
// （§8.2 第 4c 步的 merge 策略，默认策略）。
//
// 规则照原文："累加 use_count，取较新的 lastUsedAt，保留本机分类"。
// **保留本机分类**这一点容易被写漏：那条条目已经在用户的分类体系里了，
// 拿包里的 categoryId 去覆盖会把用户的整理结果冲掉。
func (d *DB) MergeIntoExisting(ctx context.Context, id, addUseCount, lastUsedAt int64) error {
	return mergeIntoExistingOn(ctx, d.w, id, addUseCount, lastUsedAt)
}

// MergeIntoExistingTx 是 MergeIntoExisting 的**事务内**版本。
//
// ⚠️ 导入路径必须用它。原因与 SoftDeleteTx 完全一样：写句柄是
// SetMaxOpenConns(1) 的单连接池，导入循环已经用 db.Writer().BeginTx
// 占住了那唯一一条连接，再走 d.w 就是自己去要第二条连接 —— **永久死锁**。
// 实测就是这里挂住了 TestAcceptance2（第二次导入全部走 merge 路径）。
func (d *DB) MergeIntoExistingTx(ctx context.Context, ex Execer, id, addUseCount, lastUsedAt int64) error {
	return mergeIntoExistingOn(ctx, ex, id, addUseCount, lastUsedAt)
}

func mergeIntoExistingOn(ctx context.Context, ex Execer, id, addUseCount, lastUsedAt int64) error {
	if addUseCount < 0 {
		addUseCount = 0
	}
	// lastUsedAt 取较大值：SQLite 的 max() 在双参数标量形式下可用，
	// 但 NULL 会传染，所以用 COALESCE 把它当 0（= 很久以前）参与比较。
	q := `UPDATE items
	         SET use_count    = use_count + ?,
	             last_used_at = CASE
	                              WHEN ? > COALESCE(last_used_at, 0) THEN ?
	                              ELSE last_used_at
	                            END
	       WHERE id = ?`
	if _, err := ex.ExecContext(ctx, q, addUseCount, lastUsedAt, lastUsedAt, id); err != nil {
		return fmt.Errorf("store: merge imported item: %w", err)
	}
	return nil
}

// GetByFingerprintTx 是 GetByFingerprint 的**事务内**版本。
//
// 两个理由：
//  1. 事务内读走 d.r（另一条连接、另一个连接池）会看到**本事务尚未提交**
//     的旧状态。同一批里出现两条同指纹的条目时，第二次查重本应命中
//     第一次刚插进来的那条，却会因为看不见而继续往下走、最终撞唯一索引。
//  2. 一致性：读与写落在同一个快照上，"查到了就更新、没查到就插入"这个
//     判断才有意义。
//
// 取不到时同样返回 ErrNotFound（与 GetByFingerprint 保持一致的契约）。
func GetByFingerprintTx(ctx context.Context, ex RowQuerier, fp string) (*Item, error) {
	row := ex.QueryRowContext(ctx,
		"SELECT "+itemColumns+" FROM items WHERE fingerprint = ? AND deleted_at IS NULL", fp)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// CategoryByName 按名字精确匹配分类（§8.2 第 2 步的映射依据）。
//
// 按 name 而不是 id 匹配，是因为两个机器上的 ID 体系完全无关。
func (d *DB) CategoryByName(ctx context.Context, name string) (*Category, error) {
	row := d.r.QueryRowContext(ctx,
		"SELECT "+categoryColumns+" FROM categories WHERE name = ?", name)
	c, err := scanCategory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

// TotalItemBytes 返回全部条目（含回收站）的 byte_size 合计，用于统计面板。
func (d *DB) TotalItemBytes(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	if err := d.r.QueryRowContext(ctx, "SELECT COALESCE(sum(byte_size),0) FROM items").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: total item bytes: %w", err)
	}
	return n.Int64, nil
}

// TopSourceApps 返回条目数最多的来源应用（统计面板的"Top 来源应用"）。
func (d *DB) TopSourceApps(ctx context.Context, limit int) ([]SourceAppStat, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.r.QueryContext(ctx,
		`SELECT COALESCE(source_app_id,''), COALESCE(source_app_name,''),
		        count(*), COALESCE(sum(byte_size),0)
		   FROM items
		  WHERE deleted_at IS NULL AND COALESCE(source_app_id,'') <> ''
		  GROUP BY source_app_id, source_app_name
		  ORDER BY count(*) DESC, source_app_id ASC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: top source apps: %w", err)
	}
	defer rows.Close()
	var out []SourceAppStat
	for rows.Next() {
		var s SourceAppStat
		if err := rows.Scan(&s.AppID, &s.AppName, &s.Items, &s.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan source app: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SourceAppStat 是一个来源应用的统计。
type SourceAppStat struct {
	AppID   string `json:"appId"`
	AppName string `json:"appName"`
	Items   int64  `json:"items"`
	Bytes   int64  `json:"bytes"`
}

// KindStat 是一种内容类型的条数与占用。
type KindStat struct {
	Kind  string `json:"kind"`
	Items int64  `json:"items"`
	Bytes int64  `json:"bytes"`
}

// KindStats 按 kind 分组统计（统计面板用）。
func (d *DB) KindStats(ctx context.Context) ([]KindStat, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT kind, count(*), COALESCE(sum(byte_size),0)
		   FROM items WHERE deleted_at IS NULL
		  GROUP BY kind ORDER BY count(*) DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: kind stats: %w", err)
	}
	defer rows.Close()
	var out []KindStat
	for rows.Next() {
		var s KindStat
		if err := rows.Scan(&s.Kind, &s.Items, &s.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan kind stat: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
