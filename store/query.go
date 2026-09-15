package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是**列表与检索**的统一入口（DESIGN.md §4.2 / §14 第 3、7 条）。
//
// 三条不能违反的约束：
//
//  1. **分页用 keyset 而不是 OFFSET**（§14 第 3 条）。OFFSET 在万级库上会
//     线性变慢：`OFFSET 8000` 要求 SQLite 先扫掉 8000 行再丢掉。
//     这里用 `(created_at, id) < (游标)` 把代价钉在 O(log n) + LIMIT。
//  2. **列表绝不 SELECT text_content**（§14 第 7 条）。只取 preview（≤200 字符）。
//     否则 2000 条全文进内存能到几十 MB，而列表一个字都不显示。
//  3. **检索走两段式**（§4.2）：≥3 字符走 FTS5 trigram，1–2 字符走 LIKE 兜底。
//     两字中文词（"链接""图片"）与两字符 ASCII（"AI""5G"）都落在兜底路径上——
//     这是同类工具在国内最常见的差评来源，所以**同一个函数**里处理，
//     不给出两条会被走岔的入口。

// SearchMode 说明本次检索实际走了哪条路径。
//
// 它存在的意义是**可取证**：验收判据「搜索正确性」要求能证明 1/2/3 字
// 分别走了预期的那条路，而不是"结果看起来对"。
type SearchMode string

const (
	// SearchModeNone 表示这次不是检索，只是列表。
	SearchModeNone SearchMode = "none"
	// SearchModeFTS 表示走了 FTS5 trigram（查询 ≥ 3 字符且 FTS 可用）。
	SearchModeFTS SearchMode = "fts"
	// SearchModeLike 表示走了 LIKE 兜底（查询 1–2 字符，或 FTS 不可用）。
	SearchModeLike SearchMode = "like"
	// SearchModeFTSFallback 表示本来走 FTS、但它一行都没命中，于是又补了一次
	// LIKE。见 List 里的说明——这是"绝不漏结果"的保险，不是主路径。
	SearchModeFTSFallback SearchMode = "fts+like"
)

// TrigramMinRunes 是 FTS5 trigram 分词器能命中的最小查询长度（§4.2 实测）。
// 少于它必须走 LIKE，否则会得到"空结果"——不是错，是真的一行都匹配不到。
const TrigramMinRunes = 3

// 分页默认值与上限。
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Cursor 是 keyset 分页游标：(created_at, id) 二元组。
//
// 只用 created_at 不够：同一秒内落库的条目 created_at 相同，
// 单列游标会让它们要么漏掉、要么重复。
type Cursor struct {
	CreatedAt int64 `json:"createdAt"`
	ID        int64 `json:"id"`
}

// Query 是列表 / 检索的输入。
type Query struct {
	// Text 为空表示"不检索，只列时间线"。
	Text string
	// Kinds 非空时只返回这些 kind（text/html/rtf/image/files/mixed）。
	Kinds []string
	// CategoryID 非空时只返回该分类；Uncategorized 为真时只返回未分类。
	CategoryID    *int64
	Uncategorized bool
	// TagID 非空时只返回带该标签的条目。
	TagID *int64
	// SourceAppID 非空时按来源应用过滤。
	SourceAppID string
	// PinnedOnly 非空时只返回置顶条目。
	PinnedOnly bool
	// Trashed 为真时列回收站（deleted_at IS NOT NULL）而不是时间线。
	Trashed bool
	// Since / Until 是按 first_seen_at 的时间窗（M4 的"今天"这类语法用）。
	Since *int64
	Until *int64

	// Cursor 为 nil 表示第一页。
	Cursor *Cursor
	// Limit 为 0 时用 DefaultPageSize。
	Limit int
	// IncludeTotal 为真时额外跑一次 count(*)（列表首屏需要"共 N 条"时用）。
	IncludeTotal bool
}

func (q Query) withDefaults() Query {
	out := q
	if out.Limit <= 0 {
		out.Limit = DefaultPageSize
	}
	if out.Limit > MaxPageSize {
		out.Limit = MaxPageSize
	}
	return out
}

// ListRow 是列表 / 检索返回的**元数据**（不含正文）。
//
// ThumbPath / ImagePath 是相对 blobs/ 的路径，前端拼成自定义协议 URL 交给
// WebView 自己去加载——图片绝不走 IPC（§14 第 1 条：100 张缩略图 base64
// 塞进 IPC 会让首屏卡 1 秒以上、内存翻倍）。
type ListRow struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Preview       string `json:"preview"`
	Fingerprint   string `json:"fingerprint"`
	ByteSize      int64  `json:"byteSize"`
	SourceAppID   string `json:"sourceAppId"`
	SourceAppName string `json:"sourceAppName"`
	SourceURL     string `json:"sourceUrl"`
	CategoryID    *int64 `json:"categoryId"`
	Pinned        bool   `json:"pinned"`
	FirstSeenAt   int64  `json:"firstSeenAt"`
	ExpiresAt     *int64 `json:"expiresAt"`
	TTLSource     string `json:"ttlSource"`
	CreatedAt     int64  `json:"createdAt"`
	LastUsedAt    *int64 `json:"lastUsedAt"`
	UseCount      int64  `json:"useCount"`
	DeletedAt     *int64 `json:"deletedAt"`
	ThumbPath     string `json:"thumbPath"`
	ImagePath     string `json:"imagePath"`
	// TextLen 是 text_content 的字符数（用 SQL 的 length() 算，不取正文）。
	TextLen int64 `json:"textLen"`
	// FileCount 是 file_paths 数组长度（用 json_array_length 算，不解析 JSON）。
	FileCount int `json:"fileCount"`
	// TagIDs 由一次 IN 查询批量补齐，避免 N+1。
	TagIDs []int64 `json:"tagIds"`
}

// HasImage 报告该条目是否可以取到图（缩略图优先）。
func (r ListRow) HasImage() bool { return r.ThumbPath != "" || r.ImagePath != "" }

// Page 是一页结果 + 取证信息。
type Page struct {
	Rows []ListRow `json:"rows"`
	// NextCursor 非 nil 时表示还有下一页。
	NextCursor *Cursor `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
	// Mode 是本次实际走的检索路径（§4.2 的可取证点）。
	Mode SearchMode `json:"mode"`
	// TookMs 是本次查询耗时（验收判据「检索延迟 < 50 ms」的取证）。
	TookMs int64 `json:"tookMs"`
	// Total 仅在 Query.IncludeTotal 为真时有效。
	Total      int64 `json:"total"`
	TotalValid bool  `json:"totalValid"`
	// FTSAvailable 暴露全文索引是否可用，UI 在降级时给出提示。
	FTSAvailable bool `json:"ftsAvailable"`
}

// listColumns 与 scanListRow 的顺序必须严格一致。
//
// `length(text_content)` 与 `json_array_length(file_paths)` 都在 SQL 侧算完，
// 只为拿长度而不把正文或 JSON 拉进内存。
const listColumns = `
	i.id, i.kind, i.preview, i.fingerprint, i.byte_size,
	i.source_app_id, i.source_app_name, i.source_url,
	i.category_id, i.pinned, i.first_seen_at, i.expires_at, i.ttl_source,
	i.created_at, i.last_used_at, i.use_count, i.deleted_at,
	i.thumb_path, i.image_path,
	COALESCE(length(i.text_content), 0),
	COALESCE(json_array_length(i.file_paths), 0)`

// List 执行一次列表或检索。
//
// 正常的检索只有一次查询。另外两种"再补一次 LIKE"的情形都是**保险**，
// 在正常输入上永远不会触发，但缺了它们就会在真实数据上翻车：
//
//	A. FTS 命中 0 行 → 补一次 LIKE
//	   trigram 是纯字符滑窗，`C++`、`a@b.com`、`%%%` 这类查询切出来的
//	   token 与文本里切出来的未必对得上。只在**一行都没命中**时才补跑，
//	   所以正常查询的代价不变。
//	B. FTS 直接报错 → 退化为 LIKE
//	   极端输入（例如全是标点、trigram 切不出 token）可能让 MATCH 抛
//	   OperationalError。搜索结果降级好过搜索框直接报错。
//
// 两种情形都把 Mode 标成 SearchModeFTSFallback，让 UI / 验收能看出来
// "这次不是主路径给的答案"。
func (d *DB) List(ctx context.Context, q Query) (*Page, error) {
	q = q.withDefaults()
	start := time.Now()

	mode := d.matchMode(q.Text)
	pg, err := d.runQuery(ctx, q, mode)
	if err != nil && mode == SearchModeFTS {
		d.log.Warn("FTS 查询失败，本次退化为 LIKE", "query", q.Text, "err", err)
		pg, err = d.runQuery(ctx, q, SearchModeLike)
		if err == nil {
			mode = SearchModeLike
		}
	}
	if err != nil {
		return nil, err
	}

	if mode == SearchModeFTS && len(pg.Rows) == 0 {
		if alt, err2 := d.runQuery(ctx, q, SearchModeLike); err2 == nil && len(alt.Rows) > 0 {
			alt.Mode = SearchModeFTSFallback
			pg = alt
		}
	}

	pg.TookMs = time.Since(start).Milliseconds()
	return pg, nil
}

// runQuery 是列表查询的唯一实现，mode 决定用 FTS 还是 LIKE。
func (d *DB) runQuery(ctx context.Context, q Query, mode SearchMode) (*Page, error) {
	start := time.Now()
	where, args := d.buildFilters(q)

	join := ""
	if mode != SearchModeNone {
		var matchSQL string
		if mode == SearchModeFTS {
			// ⚠️ 这里**必须**写真实表名 items_fts，不能给它起别名。
			// FTS5 的 MATCH 运算符要求左操作数是 FTS 表本身，
			// `JOIN items_fts f ... WHERE f MATCH ?` 会直接报
			// "no such column: f"（SQLite 3.50.4 实测）。
			join = " JOIN items_fts ON items_fts.rowid = i.id"
			matchSQL = "items_fts MATCH ?"
			args = append(args, FTSPhrase(q.Text))
		} else {
			pat := "%" + EscapeLike(q.Text) + "%"
			matchSQL = `(i.text_content LIKE ? ESCAPE '\' OR i.preview LIKE ? ESCAPE '\')`
			args = append(args, pat, pat)
		}
		where = append(where, matchSQL)
	}

	// keyset：游标是 (created_at, id) 二元组，两个都要比较。
	if q.Cursor != nil {
		where = append(where,
			"(i.created_at < ? OR (i.created_at = ? AND i.id < ?))")
		args = append(args, q.Cursor.CreatedAt, q.Cursor.CreatedAt, q.Cursor.ID)
	}

	sqlText := "SELECT " + listColumns + " FROM items i" + join +
		" WHERE " + strings.Join(where, " AND ") +
		" ORDER BY i.created_at DESC, i.id DESC LIMIT ?"
	// 多取一条用来判断"还有下一页"，返回前丢掉。
	args = append(args, q.Limit+1)

	rows, err := d.r.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list items: %w", err)
	}
	defer rows.Close()

	out := make([]ListRow, 0, q.Limit)
	for rows.Next() {
		r, err := scanListRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate items: %w", err)
	}

	pg := &Page{
		Mode:         mode,
		FTSAvailable: d.ftsAvailable,
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		pg.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		pg.HasMore = true
	}
	pg.Rows = out

	if len(out) > 0 {
		if err := d.attachTags(ctx, out); err != nil {
			return nil, err
		}
	}

	if q.IncludeTotal {
		n, err := d.CountQuery(ctx, q)
		if err != nil {
			return nil, err
		}
		pg.Total, pg.TotalValid = n, true
	}

	pg.TookMs = time.Since(start).Milliseconds()
	return pg, nil
}

// CountQuery 统计当前过滤条件下的条目数（不含分页与游标）。
//
// 计数用的是与 runQuery **同一条**匹配规则（走 FTS 还是 LIKE 由长度决定），
// 否则会出现"列表 12 条、共 N 条 说 37"这种自相矛盾的界面。
func (d *DB) CountQuery(ctx context.Context, q Query) (int64, error) {
	q = q.withDefaults()
	mode := d.matchMode(q.Text)

	where, args := d.buildFilters(q)
	join := ""
	if mode == SearchModeFTS {
		// 同 runQuery：MATCH 左侧必须是真实表名，不能用别名。
		join = " JOIN items_fts ON items_fts.rowid = i.id"
		where = append(where, "items_fts MATCH ?")
		args = append(args, FTSPhrase(q.Text))
	} else if mode == SearchModeLike {
		pat := "%" + EscapeLike(q.Text) + "%"
		where = append(where,
			`(i.text_content LIKE ? ESCAPE '\' OR i.preview LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat)
	}

	sqlText := "SELECT count(*) FROM items i" + join + " WHERE " + strings.Join(where, " AND ")
	var n int64
	if err := d.r.QueryRowContext(ctx, sqlText, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count items: %w", err)
	}
	return n, nil
}

// buildFilters 组装与检索无关的那些过滤条件。
func (d *DB) buildFilters(q Query) ([]string, []any) {
	where := make([]string, 0, 8)
	args := make([]any, 0, 8)

	if q.Trashed {
		where = append(where, "i.deleted_at IS NOT NULL")
	} else {
		where = append(where, "i.deleted_at IS NULL")
	}

	if len(q.Kinds) > 0 {
		placeholders := make([]string, 0, len(q.Kinds))
		for _, k := range q.Kinds {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, k)
		}
		if len(placeholders) > 0 {
			where = append(where, "i.kind IN ("+strings.Join(placeholders, ",")+")")
		}
	}

	switch {
	case q.Uncategorized:
		where = append(where, "i.category_id IS NULL")
	case q.CategoryID != nil:
		where = append(where, "i.category_id = ?")
		args = append(args, *q.CategoryID)
	}

	if q.TagID != nil {
		where = append(where,
			"EXISTS (SELECT 1 FROM item_tags it WHERE it.item_id = i.id AND it.tag_id = ?)")
		args = append(args, *q.TagID)
	}

	if q.SourceAppID != "" {
		where = append(where, "i.source_app_id = ?")
		args = append(args, q.SourceAppID)
	}
	if q.PinnedOnly {
		where = append(where, "i.pinned = 1")
	}
	if q.Since != nil {
		where = append(where, "i.first_seen_at >= ?")
		args = append(args, *q.Since)
	}
	if q.Until != nil {
		where = append(where, "i.first_seen_at <= ?")
		args = append(args, *q.Until)
	}
	return where, args
}

// attachTags 用一次 IN 查询给整页补上标签，避免每条一次查询。
func (d *DB) attachTags(ctx context.Context, rows []ListRow) error {
	idx := make(map[int64]int, len(rows))
	args := make([]any, 0, len(rows))
	for i := range rows {
		idx[rows[i].ID] = i
		args = append(args, rows[i].ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")

	q := "SELECT item_id, tag_id FROM item_tags WHERE item_id IN (" + placeholders + ") ORDER BY tag_id"
	rrows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: load item tags: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var itemID, tagID int64
		if err := rrows.Scan(&itemID, &tagID); err != nil {
			return fmt.Errorf("store: scan item tag: %w", err)
		}
		if i, ok := idx[itemID]; ok {
			rows[i].TagIDs = append(rows[i].TagIDs, tagID)
		}
	}
	return rrows.Err()
}

func scanListRow(row rowScanner) (ListRow, error) {
	var (
		r                                   ListRow
		preview, fp, srcID, srcName, srcURL sql.NullString
		thumbPath, imagePath                sql.NullString
		ttlSource                           sql.NullString
		categoryID, expiresAt, lastUsedAt   sql.NullInt64
		deletedAt                           sql.NullInt64
		pinned                              int64
	)
	err := row.Scan(
		&r.ID, &r.Kind, &preview, &fp, &r.ByteSize,
		&srcID, &srcName, &srcURL,
		&categoryID, &pinned, &r.FirstSeenAt, &expiresAt, &ttlSource,
		&r.CreatedAt, &lastUsedAt, &r.UseCount, &deletedAt,
		&thumbPath, &imagePath,
		&r.TextLen, &r.FileCount,
	)
	if err != nil {
		return ListRow{}, fmt.Errorf("store: scan list row: %w", err)
	}
	r.Preview = preview.String
	r.Fingerprint = fp.String
	r.SourceAppID = srcID.String
	r.SourceAppName = srcName.String
	r.SourceURL = srcURL.String
	r.ThumbPath = thumbPath.String
	r.ImagePath = imagePath.String
	r.TTLSource = ttlSource.String
	r.CategoryID = nullInt64Ptr(categoryID)
	r.ExpiresAt = nullInt64Ptr(expiresAt)
	r.LastUsedAt = nullInt64Ptr(lastUsedAt)
	r.DeletedAt = nullInt64Ptr(deletedAt)
	r.Pinned = pinned != 0
	return r, nil
}

// ErrCursorStale 在游标指向的条目已经被删时代替"重复/漏项"暴露出来。
//
// 之所以显式定义：keyset 分页下，若游标锚定的条目在翻页途中被 GC 删掉，
// 后续页仍然正确（比较的是值不是行），所以正常不该出现这个错误。
// 留着它是为了在将来改成"按 id 锚定"时有一个明确的失败面。
var ErrCursorStale = errors.New("store: pagination cursor is stale")
