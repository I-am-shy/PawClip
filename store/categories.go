package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 分类（DESIGN.md §4.1 的 categories 表 / §5.1 的②级 TTL / §11 的 P1 功能）。
//
// rule 是 JSON 自动归类规则（§3.2 的 match/conditions 结构）。这里只负责
// 存取，规则的**求值**在 classify.go 里——把存储与判定分开，规则求值才能
// 被单测直接喂样本，不用碰数据库。

// ErrCategoryNameTaken 表示同名分类已存在（name 上有 UNIQUE）。
var ErrCategoryNameTaken = errors.New("store: category name already exists")

// Category 是 categories 表的一行。
type Category struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Color      string `json:"color"`
	Icon       string `json:"icon"`
	Rule       string `json:"rule"`
	TTLSeconds *int64 `json:"ttlSeconds"`
	SortOrder  int    `json:"sortOrder"`
	CreatedAt  int64  `json:"createdAt"`
	// ItemCount 仅在 ListCategories 里填充（一次 GROUP BY 拿到，不做 N+1）。
	ItemCount int64 `json:"itemCount"`
}

const categoryColumns = `id, name, COALESCE(color,''), COALESCE(icon,''), COALESCE(rule,''),
	ttl_seconds, sort_order, created_at`

func scanCategory(row rowScanner) (*Category, error) {
	var c Category
	var ttl sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.Color, &c.Icon, &c.Rule,
		&ttl, &c.SortOrder, &c.CreatedAt); err != nil {
		return nil, err
	}
	c.TTLSeconds = nullInt64Ptr(ttl)
	return &c, nil
}

// ListCategories 返回全部分类，按 sort_order、name 排序。
//
// 顺带把每个分类的存活条目数一次算出来：分类树要在每一项后面显示条数，
// 逐项查询会变成 N+1。
func (d *DB) ListCategories(ctx context.Context) ([]Category, error) {
	q := "SELECT " + categoryColumns +
		", (SELECT count(*) FROM items i WHERE i.category_id = categories.id AND i.deleted_at IS NULL)" +
		" FROM categories ORDER BY sort_order ASC, name ASC"

	rows, err := d.r.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		var ttl sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.Icon, &c.Rule,
			&ttl, &c.SortOrder, &c.CreatedAt, &c.ItemCount); err != nil {
			return nil, fmt.Errorf("store: scan category: %w", err)
		}
		c.TTLSeconds = nullInt64Ptr(ttl)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCategory 按 id 取分类。
func (d *DB) GetCategory(ctx context.Context, id int64) (*Category, error) {
	row := d.r.QueryRowContext(ctx, "SELECT "+categoryColumns+" FROM categories WHERE id = ?", id)
	c, err := scanCategory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get category: %w", err)
	}
	return c, nil
}

// CreateCategory 新建分类。
func (d *DB) CreateCategory(ctx context.Context, c *Category) (int64, error) {
	if c == nil || strings.TrimSpace(c.Name) == "" {
		return 0, errors.New("store: category needs a name")
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().Unix()
	}
	var id int64
	err := d.w.QueryRowContext(ctx,
		`INSERT INTO categories (name, color, icon, rule, ttl_seconds, sort_order, created_at)
		 VALUES (?,?,?,?,?,?,?) RETURNING id`,
		strings.TrimSpace(c.Name), nullableString(strPtrOrNil(c.Color)), nullableString(strPtrOrNil(c.Icon)),
		nullableString(strPtrOrNil(c.Rule)), nullableInt64(c.TTLSeconds), c.SortOrder, c.CreatedAt,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrCategoryNameTaken
		}
		return 0, fmt.Errorf("store: create category: %w", err)
	}
	c.ID = id
	return id, nil
}

// UpdateCategory 全量更新一个分类的元数据。
//
// 返回受影响的条目数：改了 ttl_seconds 时，该分类下 ttl_source='category'
// 的条目需要重算（§5.1 明确要求）。重算不在这里做，而是交给调用方显式调用
// RecalcCategoryTTL——因为"改元数据"和"改两百条条目的过期时间"是两件事，
// 混在一起会让这个函数在 UI 上表现为"改个颜色卡一下"。
func (d *DB) UpdateCategory(ctx context.Context, c *Category) error {
	if c == nil || c.ID == 0 {
		return errors.New("store: category needs an id")
	}
	_, err := d.w.ExecContext(ctx,
		`UPDATE categories SET name = ?, color = ?, icon = ?, rule = ?, ttl_seconds = ?,
		        sort_order = ? WHERE id = ?`,
		strings.TrimSpace(c.Name), nullableString(strPtrOrNil(c.Color)), nullableString(strPtrOrNil(c.Icon)),
		nullableString(strPtrOrNil(c.Rule)), nullableInt64(c.TTLSeconds), c.SortOrder, c.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrCategoryNameTaken
		}
		return fmt.Errorf("store: update category: %w", err)
	}
	return nil
}

// DeleteCategory 删除分类。
//
// items.category_id 上是 ON DELETE SET NULL（§4.1），所以删分类不会带走条目，
// 只把它们变成"未分类"。这也意味着 UI 不必警告"该分类下有 N 条内容会被删"——
// 那是假的。
func (d *DB) DeleteCategory(ctx context.Context, id int64) error {
	if _, err := d.w.ExecContext(ctx, "DELETE FROM categories WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete category: %w", err)
	}
	return nil
}

// ReorderCategories 按给定顺序重排分类（拖拽排序）。
func (d *DB) ReorderCategories(ctx context.Context, ids []int64) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: reorder categories: %w", err)
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE categories SET sort_order = ? WHERE id = ?", i, id); err != nil {
			return fmt.Errorf("store: reorder categories: %w", err)
		}
	}
	return tx.Commit()
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isUniqueViolation 判断错误是不是 UNIQUE 约束冲突。
//
// mattn/go-sqlite3 把扩展错误码暴露成 sqlite3.Error，但为了不把驱动类型
// 泄漏到 store 的其他文件里，这里只做字符串判定——稳定且够用
// （SQLite 的文案是 "UNIQUE constraint failed: ..."）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed: UNIQUE")
}
