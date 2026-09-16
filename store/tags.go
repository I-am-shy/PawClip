package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 标签（docs/DESIGN.md §4.1 的 tags / item_tags 两张表 / §11 的 P1 功能）。
//
// 多对多，删除标签会级联清掉 item_tags（两张表上都是 ON DELETE CASCADE）。

// ErrTagNameTaken 表示同名标签已存在。
var ErrTagNameTaken = errors.New("store: tag name already exists")

// Tag 是 tags 表的一行。
type Tag struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	ItemCount int64  `json:"itemCount"`
}

// ListTags 返回全部标签，附带各自的存活条目数。
func (d *DB) ListTags(ctx context.Context) ([]Tag, error) {
	q := `SELECT t.id, t.name, COALESCE(t.color,''),
	             (SELECT count(*) FROM item_tags it
	                JOIN items i ON i.id = it.item_id
	               WHERE it.tag_id = t.id AND i.deleted_at IS NULL)
	        FROM tags t ORDER BY t.name ASC`
	rows, err := d.r.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.ItemCount); err != nil {
			return nil, fmt.Errorf("store: scan tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTag 新建标签。
func (d *DB) CreateTag(ctx context.Context, name, color string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("store: tag needs a name")
	}
	var id int64
	err := d.w.QueryRowContext(ctx,
		`INSERT INTO tags (name, color) VALUES (?, ?) RETURNING id`,
		name, nullableString(strPtrOrNil(color))).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrTagNameTaken
		}
		return 0, fmt.Errorf("store: create tag: %w", err)
	}
	return id, nil
}

// EnsureTag 按名字取标签，不存在则建。返回 id 与"是否新建"。
//
// 导入时按名字匹配标签要用它：两台机器的 id 体系完全无关（docs/BACKUP-FORMAT.md §8.2）。
func (d *DB) EnsureTag(ctx context.Context, name, color string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, errors.New("store: tag needs a name")
	}
	var id int64
	err := d.r.QueryRowContext(ctx, "SELECT id FROM tags WHERE name = ?", name).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("store: find tag: %w", err)
	}
	id, err = d.CreateTag(ctx, name, color)
	if errors.Is(err, ErrTagNameTaken) {
		// 并发插入：重新读一次。
		if e2 := d.r.QueryRowContext(ctx, "SELECT id FROM tags WHERE name = ?", name).Scan(&id); e2 != nil {
			return 0, false, fmt.Errorf("store: find tag after race: %w", e2)
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// RenameTag 改名 / 换色。
func (d *DB) RenameTag(ctx context.Context, id int64, name, color string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("store: tag needs a name")
	}
	_, err := d.w.ExecContext(ctx,
		"UPDATE tags SET name = ?, color = ? WHERE id = ?",
		name, nullableString(strPtrOrNil(color)), id)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrTagNameTaken
		}
		return fmt.Errorf("store: rename tag: %w", err)
	}
	return nil
}

// DeleteTag 删除标签（item_tags 靠外键级联清理）。
func (d *DB) DeleteTag(ctx context.Context, id int64) error {
	if _, err := d.w.ExecContext(ctx, "DELETE FROM tags WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete tag: %w", err)
	}
	return nil
}

// SetItemTags 覆盖式设定某条目的标签集合。
func (d *DB) SetItemTags(ctx context.Context, itemID int64, tagIDs []int64) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set item tags: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.setItemTagsOn(ctx, tx, itemID, tagIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// SetItemTagsTx 是 SetItemTags 的**事务内**版本：所有写都走到调用方给的 ex 上。
//
// 导入路径必须用它。条目本身是在一个"每 500 条提交一次"的事务里插入的，
// 标签要是另起一个事务（或走另一条连接）写，就会出现"条目已经可见、
// 标签却还没提交"的中间态；导入中途崩溃时那批数据会留下不一致的标签。
//
// 第一版这里写成了"自己开事务"，那等于没解决问题——所以单独把
// setItemTagsOn 提出来，两个入口共用同一段写逻辑，差别只在 ex 是谁。
func (d *DB) SetItemTagsTx(ctx context.Context, ex Execer, itemID int64, tagIDs []int64) error {
	return d.setItemTagsOn(ctx, ex, itemID, tagIDs)
}

// setItemTagsOn 是标签覆盖写的唯一实现。
func (d *DB) setItemTagsOn(ctx context.Context, ex Execer, itemID int64, tagIDs []int64) error {
	if _, err := ex.ExecContext(ctx, "DELETE FROM item_tags WHERE item_id = ?", itemID); err != nil {
		return fmt.Errorf("store: clear item tags: %w", err)
	}
	for _, tagID := range tagIDs {
		if _, err := ex.ExecContext(ctx,
			"INSERT OR IGNORE INTO item_tags (item_id, tag_id) VALUES (?, ?)",
			itemID, tagID); err != nil {
			return fmt.Errorf("store: add item tag: %w", err)
		}
	}
	return nil
}

// AddTagToItems 给一批条目打上同一个标签。
//
// 用 INSERT OR IGNORE：已经打过的条目不会因为主键冲突让整批失败。
func (d *DB) AddTagToItems(ctx context.Context, tagID int64, itemIDs []int64) (int64, error) {
	var n int64
	for _, batch := range batchIDs(itemIDs) {
		values := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*2)
		for _, id := range batch {
			values = append(values, "(?,?)")
			args = append(args, id, tagID)
		}
		res, err := d.w.ExecContext(ctx,
			"INSERT OR IGNORE INTO item_tags (item_id, tag_id) VALUES "+strings.Join(values, ","),
			args...)
		if err != nil {
			return n, fmt.Errorf("store: add tag to items: %w", err)
		}
		affected, _ := res.RowsAffected()
		n += affected
	}
	return n, nil
}

// RemoveTagFromItems 从一批条目上摘掉同一个标签。
func (d *DB) RemoveTagFromItems(ctx context.Context, tagID int64, itemIDs []int64) (int64, error) {
	var n int64
	for _, batch := range batchIDs(itemIDs) {
		q := "DELETE FROM item_tags WHERE tag_id = ? AND item_id IN (" + placeholders(len(batch)) + ")"
		args := append([]any{tagID}, int64Args(batch)...)
		res, err := d.w.ExecContext(ctx, q, args...)
		if err != nil {
			return n, fmt.Errorf("store: remove tag from items: %w", err)
		}
		affected, _ := res.RowsAffected()
		n += affected
	}
	return n, nil
}

// ItemTags 返回某条目当前的标签 id 列表。
func (d *DB) ItemTags(ctx context.Context, itemID int64) ([]int64, error) {
	rows, err := d.r.QueryContext(ctx,
		"SELECT tag_id FROM item_tags WHERE item_id = ? ORDER BY tag_id", itemID)
	if err != nil {
		return nil, fmt.Errorf("store: read item tags: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan item tag: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// 注意：**不要**在这里写"清理没有标签的条目"。
// 标签不是条目的生存依据，一个没有任何标签的条目是完全正常的。
