package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 本文件是条目层面的写操作（pin / 删除 / 恢复 / 硬删 / 改分类 / 改过期）。
//
// 三条纪律：
//
//  1. **一律走单写句柄 d.w**，与捕获写入共用 SQLite 的串行化纪律，
//     不绕过写入器去并发写（§2 的硬约束）。
//  2. **软删除优先**。剪贴板历史属于"误删代价极高"的数据（§5.2），
//     用户点删除只是进回收站；只有 GC 的硬删阶段会真的删行。
//  3. **FTS 靠触发器同步**，这里不需要手动维护 items_fts
//     （items_ai / items_ad / items_au 三个触发器已在 schema.go 建好）。

// maxIDList 是单次批量操作的 id 上限。
//
// SQLite 的默认参数上限是 999（SQLITE_MAX_VARIABLE_NUMBER 在旧版本），
// 而"全选删除"很容易超过。这里截断成 500 一批，由 batchIDs 自动分片。
const maxIDList = 500

// batchIDs 把 id 切片切成安全大小的批次。
func batchIDs(ids []int64) [][]int64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([][]int64, 0, (len(ids)+maxIDList-1)/maxIDList)
	for len(ids) > 0 {
		n := len(ids)
		if n > maxIDList {
			n = maxIDList
		}
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

// placeholders 生成 "?,?,?" 形式的占位符。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// int64Args 把 id 切片转成 []any，供 database/sql 展开。
func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// SoftDelete 把条目移入回收站（写 deleted_at）。
//
// 注意这会**腾出指纹唯一索引**：uq_items_fp_alive 只约束 deleted_at IS NULL
// 的行，所以删掉一份内容之后，同样的内容可以再次被捕获进来（这是设计意图，
// 见 §4.1 的注释）。
func (d *DB) SoftDelete(ctx context.Context, ids []int64) (int64, error) {
	return d.markDeleted(ctx, ids, time.Now().Unix())
}

func (d *DB) markDeleted(ctx context.Context, ids []int64, now int64) (int64, error) {
	var n int64
	for _, batch := range batchIDs(ids) {
		q := "UPDATE items SET deleted_at = ? WHERE deleted_at IS NULL AND id IN (" +
			placeholders(len(batch)) + ")"
		args := append([]any{now}, int64Args(batch)...)
		res, err := d.w.ExecContext(ctx, q, args...)
		if err != nil {
			return n, fmt.Errorf("store: soft delete: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("store: soft delete rows affected: %w", err)
		}
		n += affected
	}
	return n, nil
}

// Restore 把条目从回收站恢复。
//
// 恢复可能撞指纹唯一索引：回收站里那条的指纹，可能已经被"删掉之后又重新
// 复制进来的"另一条占住了。这种情况**不报错**，而是把它留在回收站并计入
// 返回值里的 conflict，让 UI 能解释"这几条没能恢复，因为已经有同样的内容了"。
func (d *DB) Restore(ctx context.Context, ids []int64) (restored int64, conflict int, err error) {
	for _, batch := range batchIDs(ids) {
		q := "UPDATE OR IGNORE items SET deleted_at = NULL WHERE deleted_at IS NOT NULL AND id IN (" +
			placeholders(len(batch)) + ")"
		res, err2 := d.w.ExecContext(ctx, q, int64Args(batch)...)
		if err2 != nil {
			return restored, conflict, fmt.Errorf("store: restore: %w", err2)
		}
		affected, err2 := res.RowsAffected()
		if err2 != nil {
			return restored, conflict, fmt.Errorf("store: restore rows affected: %w", err2)
		}
		restored += affected
		// OR IGNORE 下被唯一索引挡掉的行不会出现在 RowsAffected 里，
		// 所以"想恢复的条数 - 实际恢复的"就是冲突数。
		conflict += len(batch) - int(affected)
	}
	return restored, conflict, nil
}

// Purge 物理删除条目。
//
// ⚠️ 这里**故意不删 blob 文件**。blobs/ 是内容寻址的：同一份图片字节
// 可能被两条不同的条目引用（例如"图"与"图+文字"两种复制形态）。按条删文件
// 会让另一条变成悬空引用。文件回收统一交给 GC 的孤儿扫描（§5.2 第 6 步）：
// 它按"没有任何存活条目引用且 mtime > 1 天"判定，天然不会误删。
func (d *DB) Purge(ctx context.Context, ids []int64) (int64, error) {
	var n int64
	for _, batch := range batchIDs(ids) {
		q := "DELETE FROM items WHERE id IN (" + placeholders(len(batch)) + ")"
		res, err := d.w.ExecContext(ctx, q, int64Args(batch)...)
		if err != nil {
			return n, fmt.Errorf("store: purge: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("store: purge rows affected: %w", err)
		}
		n += affected
	}
	return n, nil
}

// EmptyTrash 清空回收站。
func (d *DB) EmptyTrash(ctx context.Context) (int64, error) {
	res, err := d.w.ExecContext(ctx, "DELETE FROM items WHERE deleted_at IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("store: empty trash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: empty trash rows affected: %w", err)
	}
	return n, nil
}

// SetPinned 置顶 / 取消置顶。
//
// 置顶会顺手把 expires_at 清掉并把来源记为 never——§5.1 规定
// "pinned = 1 强制凌驾全部规则"，与其等 GC 的保护步骤每轮纠一次，
// 不如在置顶的同一句话里就写对。
func (d *DB) SetPinned(ctx context.Context, ids []int64, pinned bool) error {
	for _, batch := range batchIDs(ids) {
		var q string
		if pinned {
			q = "UPDATE items SET pinned = 1, expires_at = NULL, ttl_source = 'never'" +
				" WHERE id IN (" + placeholders(len(batch)) + ")"
		} else {
			q = "UPDATE items SET pinned = 0 WHERE id IN (" + placeholders(len(batch)) + ")"
		}
		if _, err := d.w.ExecContext(ctx, q, int64Args(batch)...); err != nil {
			return fmt.Errorf("store: set pinned=%v: %w", pinned, err)
		}
	}
	return nil
}

// SetCategory 改分类。categoryID 为 nil 表示移出分类。
func (d *DB) SetCategory(ctx context.Context, ids []int64, categoryID *int64) error {
	for _, batch := range batchIDs(ids) {
		q := "UPDATE items SET category_id = ? WHERE id IN (" + placeholders(len(batch)) + ")"
		args := append([]any{nullableInt64(categoryID)}, int64Args(batch)...)
		if _, err := d.w.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("store: set category: %w", err)
		}
	}
	return nil
}

// TTL 来源（写进 items.ttl_source，UI 靠它解释"它为什么 6 天后会消失"）。
const (
	TTLSourceGlobal   = "global"
	TTLSourceCategory = "category"
	TTLSourceItem     = "item"
	TTLSourceNever    = "never"
)

// SetExpiry 设定单条条目的显式过期时间（ttl_source='item'）。
//
// expiresAt 为 nil 表示"永不过期"（ttl_source='never'）。
// 这是 §5.1 的①级：优先级最高，除了 pinned 谁都不能覆盖它。
func (d *DB) SetExpiry(ctx context.Context, ids []int64, expiresAt *int64) error {
	source := TTLSourceItem
	if expiresAt == nil {
		source = TTLSourceNever
	}
	for _, batch := range batchIDs(ids) {
		q := "UPDATE items SET expires_at = ?, ttl_source = ? WHERE id IN (" +
			placeholders(len(batch)) + ")"
		args := append([]any{nullableInt64(expiresAt), source}, int64Args(batch)...)
		if _, err := d.w.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("store: set expiry: %w", err)
		}
	}
	return nil
}

// MarkUsed 记录一次"从历史里取用"。
//
// 只动 last_used_at / use_count，**不动 created_at**：列表是按 created_at
// 排的，取用一次就把条目踢到最前会让用户正在看的那一屏突然重排。
// 而 last_used_at 是 GC 淘汰的依据（§5.2 第 4 步按 last_used_at ASC 淘汰），
// 所以它必须被更新，否则"经常用的会被淘汰"。
func (d *DB) MarkUsed(ctx context.Context, ids []int64) error {
	now := time.Now().Unix()
	for _, batch := range batchIDs(ids) {
		q := "UPDATE items SET last_used_at = ?, use_count = use_count + 1 WHERE id IN (" +
			placeholders(len(batch)) + ")"
		args := append([]any{now}, int64Args(batch)...)
		if _, err := d.w.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("store: mark used: %w", err)
		}
	}
	return nil
}

// RecalcCategoryTTL 在分类的 ttl_seconds 被改动之后批量重算该分类下的条目。
//
// 只动 ttl_source = 'category' 的行：用户单条显式设定的（'item'）不动，
// 置顶保护的（'never'）也不动——§5.1 明确要求。
func (d *DB) RecalcCategoryTTL(ctx context.Context, categoryID int64, ttlSeconds *int64) (int64, error) {
	var (
		q    string
		args []any
	)
	if ttlSeconds == nil {
		// 分类改成"跟随全局"：交还给 global 一档，由 GC 下一轮按全局 TTL 重算。
		q = `UPDATE items SET expires_at = NULL, ttl_source = ?
		      WHERE category_id = ? AND deleted_at IS NULL AND ttl_source = ?`
		args = []any{TTLSourceGlobal, categoryID, TTLSourceCategory}
	} else {
		expires := time.Now().Unix() + *ttlSeconds
		q = `UPDATE items SET expires_at = ?, ttl_source = ?
		      WHERE category_id = ? AND deleted_at IS NULL AND ttl_source = ?`
		args = []any{expires, TTLSourceCategory, categoryID, TTLSourceCategory}
	}
	res, err := d.w.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: recalc category ttl: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: recalc category ttl rows affected: %w", err)
	}
	return n, nil
}

// ClearExpired 把 expires_at <= now 的存活条目按 mode 处理。
//
// 三种行为的语义（§5.2 第 2 步）：
//
//	trash   → 写 deleted_at，进回收站（默认）
//	delete  → 直接物理删除
//	archive → 保留内容但清掉 expires_at（转入"永不主动过期"），
//	          并（当 archiveCategoryID 非 nil 时）移入该分类
//
// archiveCategoryID 只在 mode = archive 时生效。搬分类与清 expires_at
// 写在**同一条 SQL** 里：分两条的话，中间崩溃会留下一批"已经永不回收、
// 但没进归档分类"的条目——用户既不会看到它过期，也找不到它。
//
// 返回各行为的条数，供 GC 报告与统计面板使用。
func (d *DB) ClearExpired(ctx context.Context, now int64, mode string, archiveCategoryID *int64) (trashed, deleted, archived int64, err error) {
	// pinned 与显式 never 的条目必须被排除在外。理论上 SetPinned 已经清掉了
	// expires_at，但用户在置顶之前可能设过显式过期时间，而且手改库也可能造出
	// 这种组合——所以这里再挡一次，这是"收藏永不回收"的兜底。
	const guard = "deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ? AND pinned = 0"

	switch mode {
	case OnExpireDelete:
		res, err2 := d.w.ExecContext(ctx, "DELETE FROM items WHERE "+guard, now)
		if err2 != nil {
			return 0, 0, 0, fmt.Errorf("store: expire(delete): %w", err2)
		}
		deleted, _ = res.RowsAffected()
	case OnExpireArchive:
		var (
			q    string
			args []any
		)
		if archiveCategoryID != nil {
			q = "UPDATE items SET expires_at = NULL, ttl_source = ?, category_id = ? WHERE " + guard
			args = []any{TTLSourceNever, *archiveCategoryID, now}
		} else {
			q = "UPDATE items SET expires_at = NULL, ttl_source = ? WHERE " + guard
			args = []any{TTLSourceNever, now}
		}
		res, err2 := d.w.ExecContext(ctx, q, args...)
		if err2 != nil {
			return 0, 0, 0, fmt.Errorf("store: expire(archive): %w", err2)
		}
		archived, _ = res.RowsAffected()
	default: // OnExpireTrash
		res, err2 := d.w.ExecContext(ctx,
			"UPDATE items SET deleted_at = ? WHERE "+guard, now, now)
		if err2 != nil {
			return 0, 0, 0, fmt.Errorf("store: expire(trash): %w", err2)
		}
		trashed, _ = res.RowsAffected()
	}
	return trashed, deleted, archived, nil
}

// ProtectPinned 执行 §5.2 的第 1 步：把置顶条目的过期策略钉成"永不"。
//
// 每轮 GC 开头跑一次。它是幂等的（第二次不会有行受影响）。
func (d *DB) ProtectPinned(ctx context.Context) (int64, error) {
	res, err := d.w.ExecContext(ctx,
		`UPDATE items SET expires_at = NULL, ttl_source = ?
		  WHERE pinned = 1 AND expires_at IS NOT NULL`, TTLSourceNever)
	if err != nil {
		return 0, fmt.Errorf("store: protect pinned: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeTrashedBefore 硬删回收站里 deleted_at <= cutoff 的条目（§5.2 第 3 步）。
func (d *DB) PurgeTrashedBefore(ctx context.Context, cutoff int64) (int64, error) {
	res, err := d.w.ExecContext(ctx,
		"DELETE FROM items WHERE deleted_at IS NOT NULL AND deleted_at <= ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: purge trashed before: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge trashed rows affected: %w", err)
	}
	return n, nil
}

// EvictIDs 按 (pinned ASC, last_used_at ASC) 挑出该淘汰的条目 id（§5.2 第 4 步）。
//
// 只用一条 SQL 选出"超额的、最该走的"那些 id，避免把 2000 条全拉进内存
// 再排序。COALESCE 把 last_used_at 为 NULL 的算成 first_seen_at——
// 从没被取用过的条目应该先走，而不是因为 NULL 排到最后。
//
// kinds 非空时只在这些 kind 里挑（容量淘汰优先淘汰大体积图片用得上）。
func (d *DB) EvictIDs(ctx context.Context, over int, kinds []string) ([]int64, error) {
	if over <= 0 {
		return nil, nil
	}
	where := []string{"deleted_at IS NULL", "pinned = 0"}
	args := make([]any, 0, len(kinds)+1)

	if len(kinds) > 0 {
		ph := make([]string, 0, len(kinds))
		for _, k := range kinds {
			ph = append(ph, "?")
			args = append(args, k)
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	args = append(args, over)

	q := "SELECT id FROM items WHERE " + strings.Join(where, " AND ") +
		" ORDER BY COALESCE(last_used_at, first_seen_at) ASC, id ASC LIMIT ?"

	rows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: select eviction candidates: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, over)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan eviction candidate: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountAliveByKinds 统计存活条目里这些 kind 的数量（容量淘汰用）。
func (d *DB) CountAliveByKinds(ctx context.Context, kinds []string) (int64, error) {
	if len(kinds) == 0 {
		return d.CountAlive(ctx)
	}
	ph := make([]string, 0, len(kinds))
	args := make([]any, 0, len(kinds))
	for _, k := range kinds {
		ph = append(ph, "?")
		args = append(args, k)
	}
	var n int64
	q := "SELECT count(*) FROM items WHERE deleted_at IS NULL AND kind IN (" +
		strings.Join(ph, ",") + ")"
	if err := d.r.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count by kinds: %w", err)
	}
	return n, nil
}

// Vacuum 整理数据库文件（§5.2 第 6 步）。
//
// VACUUM 不能在事务里跑，也不能在 WAL 模式下"边写边跑"，所以它只在 GC
// 真正删过行之后才调用，避免每轮 60s 都做一次全库重写。
func (d *DB) Vacuum(ctx context.Context) error {
	if _, err := d.w.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	return nil
}

// AllBlobRefs 返回全部**存活条目**引用到的 blob 相对路径集合（含缩略图）。
//
// 这是孤儿扫描的判据：不在这个集合里、且 mtime 超过 1 天的文件就是孤儿。
// 用存活条目（含回收站）而不是"只有时间线上的"——回收站里的条目还能恢复，
// 它的 blob 不能被当成孤儿删掉。
func (d *DB) AllBlobRefs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT rtf_path, image_path, thumb_path FROM items`)
	if err != nil {
		return nil, fmt.Errorf("store: read blob refs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{}, 256)
	for rows.Next() {
		var rtf, img, thumb *string
		if err := rows.Scan(&rtf, &img, &thumb); err != nil {
			return nil, fmt.Errorf("store: scan blob ref: %w", err)
		}
		for _, p := range []*string{rtf, img, thumb} {
			if p != nil && *p != "" {
				out[*p] = struct{}{}
			}
		}
	}
	return out, rows.Err()
}

// TagItemIDs 返回某标签下的全部条目 id（导出/统计用）。
func (d *DB) TagItemIDs(ctx context.Context, tagID int64) ([]int64, error) {
	rows, err := d.r.QueryContext(ctx, "SELECT item_id FROM item_tags WHERE tag_id = ?", tagID)
	if err != nil {
		return nil, fmt.Errorf("store: read tag items: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan tag item: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// EvictLargestIDs 按 byte_size 从大到小挑出该淘汰的条目 id。
//
// 与 EvictIDs 的分工：条数淘汰按"最久没用过"挑（EvictIDs），
// 容量淘汰按"占地方最大"挑（本函数）——§5.2 第 5 步的原话是
// "优先淘汰大体积图片"，所以容量路径上体积优先于使用时间。
//
// exclude 里已有的 id 会被跳过：条数淘汰刚挑走的那批不该在容量淘汰里再挑一次。
func (d *DB) EvictLargestIDs(ctx context.Context, over int, kinds []string, exclude []int64) ([]int64, error) {
	if over <= 0 {
		return nil, nil
	}
	where := []string{"deleted_at IS NULL", "pinned = 0"}
	args := make([]any, 0, len(kinds)+len(exclude)+1)

	if len(kinds) > 0 {
		ph := make([]string, 0, len(kinds))
		for _, k := range kinds {
			ph = append(ph, "?")
			args = append(args, k)
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if len(exclude) > 0 {
		// 分批做 IN：exclude 可能很大，一次塞进去会撞 SQLite 的参数上限。
		// 这里用临时表更干净，但临时表要跨连接（读池/写池不同连接）——
		// 所以退回"分批拼接"。
		conds := make([]string, 0, len(exclude))
		for _, batch := range batchIDs(exclude) {
			ph := make([]string, 0, len(batch))
			for _, id := range batch {
				ph = append(ph, "?")
				args = append(args, id)
			}
			conds = append(conds, "id NOT IN ("+strings.Join(ph, ",")+")")
		}
		where = append(where, "("+strings.Join(conds, " AND ")+")")
	}
	args = append(args, over)

	q := "SELECT id FROM items WHERE " + strings.Join(where, " AND ") +
		" ORDER BY byte_size DESC, id ASC LIMIT ?"

	rows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: select largest items: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, over)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan largest item: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountAliveImages 统计存活的图片类条目数（容量淘汰的兜底：只剩图片时也还能挑）。
func (d *DB) CountAliveImages(ctx context.Context) (int64, error) {
	return d.CountAliveByKinds(ctx, []string{"image", "mixed"})
}

// ApplyDefaultTTL 把"没有过期时间、也没被标记为永不"的存活条目
// 按三级优先级的③级（全局默认 TTL）算出 expires_at 写回去。
//
// ⚠️ 基准是 first_seen_at 而不是 now，这一点必须如此：
// 若用 now + ttl 算，每轮 GC 都会把过期时间往后推，
// 条目**永远也不会到期**——那是个能静默吃掉整个保留策略的 bug。
// 锚在首次复制时间上，才算"这条内容的寿命是 30 天"。
//
// 跳过 ttl_source='never' 的行（§5.1：那代表"永不过期"这个显式决定），
// 也跳过 pinned（置顶强制凌驾全部规则）。
func (d *DB) ApplyDefaultTTL(ctx context.Context, ttlSec int64) (int64, error) {
	if ttlSec <= 0 {
		return 0, nil
	}
	res, err := d.w.ExecContext(ctx,
		`UPDATE items
		    SET expires_at = first_seen_at + ?, ttl_source = ?
		  WHERE deleted_at IS NULL
		    AND pinned = 0
		    AND expires_at IS NULL
		    AND (ttl_source IS NULL OR ttl_source = ?)`,
		ttlSec, TTLSourceGlobal, TTLSourceGlobal)
	if err != nil {
		return 0, fmt.Errorf("store: apply default ttl: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: apply default ttl rows affected: %w", err)
	}
	return n, nil
}
