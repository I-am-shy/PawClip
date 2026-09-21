package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是草稿本的数据访问层（docs/DESIGN.md §4.4 / §5.4）。
//
// 四条纪律：
//
//  1. **一律走单写句柄 d.w**，与捕获写入共用 SQLite 的串行化纪律。
//  2. **md 是唯一真源。** 正文只以 Markdown 存一份，不另存 HTML；
//     图片引用写在正文里，因而没有第二张引用表（见 BlobRefsInDraftMD）。
//  3. **不进 TTL、不进条数与容量淘汰。** 草稿是用户手写的内容，
//     唯一的回收路径是 `archived_at` 到期（软删除 → 归档 → 硬删）。
//  4. **实时保存只写 md 一列**（SaveDraftMD），标题走 RenameDraft。
//     两条路径分开是为了让"打字期间每 500ms 一次"的那条更新尽可能小，
//     也避免"自动保存顺手把用户正在改的标题覆盖回去"。

// DraftBlobPrefix 是草稿正文里图片引用的 URL 前缀。
//
// 为什么用相对 URL `blob/<分片路径>` 而不是自定义 scheme `blob://`：
//
//   - docs/DESIGN.md §14 A 第 1 条要求"图片绝不走 IPC"，由 WebView 自己
//     按 URL 取字节。`blob/…` 在 Wails 的自定义 scheme 下会解析成
//     `wails://wails/blob/…`，正好落进 AssetServer 的用户 handler
//     （blobserver.go），**零新增机制**；
//   - 换成 `blob://` 就得给两个平台各注册一个 scheme handler，
//     那是纯粹为了好看而增加的一条平台分支。
//
// 前缀与 blobserver.go 的 BlobURLPrefix 是同一个字符串，值由这里定义，
// 那边引用它——两处各写一份的话，改一处就会出现"正文里写的 URL
// 取不到字节"这种只在运行时才看得见的错位。
const DraftBlobPrefix = "blob/"

// Draft 是一条草稿。
type Draft struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	MD    string `json:"md"`
	// Seq 是默认名里的编号（NULL → 0 表示没有编号，例如导入进来的）。
	Seq int64 `json:"seq"`
	// SortOrder 只在目录排序里用；默认按创建顺序，用户拖过之后就是拖的结果。
	SortOrder  int64  `json:"sortOrder"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	ArchivedAt *int64 `json:"archivedAt"`
}

// DraftRow 是目录列表用的投影：**不含正文全文**。
//
// 与 items 的列表查询同一条理由（§14 第 7 条：列表绝不 SELECT 全文）：
// 200 条草稿的正文全量进内存可以到几十 MB，而目录只需要标题、摘要、
// 字数与时间。
type DraftRow struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Seq   int64  `json:"seq"`
	// Snippet 是正文前若干个字符（去掉 Markdown 标记之后的一行摘要）。
	Snippet string `json:"snippet"`
	// Chars 是正文字符数（目录用来提示"这条有多长"）。
	Chars      int64  `json:"chars"`
	SortOrder  int64  `json:"sortOrder"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	ArchivedAt *int64 `json:"archivedAt"`
}

// snippetChars 是目录摘要的截断长度。
//
// 取 120 而不是 items.preview 的 200：草稿标题已经占了一行，
// 摘要在 180px 宽的目录里也只能显示两行左右，多取的部分是白读。
const snippetChars = 120

// ── 读 ──────────────────────────────────────────────────────────

// ListDrafts 列出存活的草稿，按 (sort_order, id) 升序。
//
// 排序键刻意**不是** updated_at：实时保存会不停改它，正在编辑的那条会
// 一直往上跳，用户点下一条时会点错。默认顺序是创建顺序（sort_order 在
// 新建时取"当前最大值 +1"），拖拽之后就是用户拖出来的顺序。
func (d *DB) ListDrafts(ctx context.Context) ([]DraftRow, error) {
	return d.listDrafts(ctx, `archived_at IS NULL`, `sort_order ASC, id ASC`)
}

// ListArchivedDrafts 列出归档（软删除）的草稿，最近删的在最前。
//
// 单独的入口而不是 ListDrafts 的一个参数：归档区在界面上是一个可折叠的
// 独立分区，排序规则也不同（按删除时间倒序），合成一个函数会让两边
// 的排序都变得需要解释。
func (d *DB) ListArchivedDrafts(ctx context.Context) ([]DraftRow, error) {
	return d.listDrafts(ctx, `archived_at IS NOT NULL`, `archived_at DESC, id DESC`)
}

func (d *DB) listDrafts(ctx context.Context, where, order string) ([]DraftRow, error) {
	q := `SELECT id, title, COALESCE(seq, 0), substr(md, 1, ?), length(md),
	             sort_order, created_at, updated_at, archived_at
	        FROM drafts WHERE ` + where + ` ORDER BY ` + order
	rows, err := d.r.QueryContext(ctx, q, snippetChars)
	if err != nil {
		return nil, fmt.Errorf("store: list drafts: %w", err)
	}
	defer rows.Close()

	out := make([]DraftRow, 0, 16)
	for rows.Next() {
		var r DraftRow
		var raw string
		var archived sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Title, &r.Seq, &raw, &r.Chars,
			&r.SortOrder, &r.CreatedAt, &r.UpdatedAt, &archived); err != nil {
			return nil, fmt.Errorf("store: scan draft row: %w", err)
		}
		r.Snippet = DraftSnippet(raw)
		if archived.Valid {
			v := archived.Int64
			r.ArchivedAt = &v
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate drafts: %w", err)
	}
	return out, nil
}

// GetDraft 取一条草稿（含完整正文）。不存在时返回 nil, nil。
func (d *DB) GetDraft(ctx context.Context, id int64) (*Draft, error) {
	var (
		dr       Draft
		seq      sql.NullInt64
		archived sql.NullInt64
	)
	err := d.r.QueryRowContext(ctx,
		`SELECT id, title, md, seq, sort_order, created_at, updated_at, archived_at
		   FROM drafts WHERE id = ?`, id).
		Scan(&dr.ID, &dr.Title, &dr.MD, &seq, &dr.SortOrder, &dr.CreatedAt, &dr.UpdatedAt, &archived)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: get draft %d: %w", id, err)
	}
	dr.Seq = seq.Int64
	if archived.Valid {
		v := archived.Int64
		dr.ArchivedAt = &v
	}
	return &dr, nil
}

// NextDraftSeq 返回"最小的未占用正整数"，供默认名使用。
//
// 为什么不是"数量 + 1"：删掉草稿 2 之后再新建会撞名（两条都叫"草稿 2"），
// 而目录是按名字认人的。已归档的草稿**仍然占号**——它还能恢复，
// 让新草稿去占它的编号，恢复之后就会出现两条同名。
func (d *DB) NextDraftSeq(ctx context.Context) (int64, error) {
	var next int64
	// 思路：把已占用的 seq 拉出来逐个比对。用 SQL 的 MIN 递归写法虽然能
	// 一条语句解决，但可读性差很多，而草稿数是几百这个量级。
	rows, err := d.r.QueryContext(ctx, `SELECT seq FROM drafts WHERE seq IS NOT NULL ORDER BY seq ASC`)
	if err != nil {
		return 0, fmt.Errorf("store: read draft seqs: %w", err)
	}
	defer rows.Close()
	next = 1
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			return 0, fmt.Errorf("store: scan draft seq: %w", err)
		}
		if s < next {
			continue // 重号（老数据）不阻塞；下一个可用值不变
		}
		if s == next {
			next++
			continue
		}
		break // s > next：中间空出来了
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate draft seqs: %w", err)
	}
	return next, nil
}

// CountDrafts 返回存活 / 归档的草稿条数。
func (d *DB) CountDrafts(ctx context.Context) (alive, archived int64, err error) {
	err = d.r.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN archived_at IS NULL     THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN archived_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		 FROM drafts`).Scan(&alive, &archived)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count drafts: %w", err)
	}
	return alive, archived, nil
}

// DraftTextBytes 返回全部草稿正文的字符数（统计页用）。
//
// 之所以是"字符数"而不是字节：length() 对 TEXT 按字符计，
// 而正文里中文占多数，按字节算会让"占了多少"这个数字看起来虚高一倍。
func (d *DB) DraftTextBytes(ctx context.Context) (int64, error) {
	var n int64
	if err := d.r.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(length(md)), 0) FROM drafts`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: sum draft chars: %w", err)
	}
	return n, nil
}

// ── 写 ──────────────────────────────────────────────────────────

// CreateDraft 新建一条草稿，返回新 id。
//
// title 由调用方给（它必须是**已经按当前界面语言渲染好**的默认名，
// 见 docs/DESIGN.md §4.4 关于 seq 的说明）；seq 传 0 表示不编号。
// 正文初始为空串——不要写占位文案，否则用户要先删掉它才能开始打字。
func (d *DB) CreateDraft(ctx context.Context, title string, seq int64) (int64, error) {
	now := time.Now().Unix()
	var maxOrder sql.NullInt64
	if err := d.w.QueryRowContext(ctx,
		`SELECT MAX(sort_order) FROM drafts`).Scan(&maxOrder); err != nil {
		return 0, fmt.Errorf("store: read max draft sort_order: %w", err)
	}
	res, err := d.w.ExecContext(ctx,
		`INSERT INTO drafts (title, md, seq, sort_order, created_at, updated_at)
		 VALUES (?, '', ?, ?, ?, ?)`,
		title, nullableSeq(seq), maxOrder.Int64+1, now, now)
	if err != nil {
		return 0, fmt.Errorf("store: create draft: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: draft id: %w", err)
	}
	return id, nil
}

// SaveDraftMD 写正文，返回新的 updated_at（Unix 秒）。
//
// 这是实时保存的唯一入口，所以它是全仓最热的写路径之一。三条自我约束：
//
//   - **单条 UPDATE、只动 md 与 updated_at**，不重写整行、不碰 title/seq/sort_order；
//   - 允许写空串（用户真的把内容删光了，这是合法状态）；
//   - 不判断"内容是否变化"——那由调用方比对后决定要不要调（见前端
//     useDraftAutosave），因为"没变"的知识只有调用方有。
func (d *DB) SaveDraftMD(ctx context.Context, id int64, md string) (int64, error) {
	now := time.Now().Unix()
	res, err := d.w.ExecContext(ctx,
		`UPDATE drafts SET md = ?, updated_at = ? WHERE id = ?`, md, now, id)
	if err != nil {
		return 0, fmt.Errorf("store: save draft %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// 草稿在编辑期间被删掉了（另一处入口，或另一台机器导入）。
		// 明确报出来，而不是"保存成功"——前端要据此提示用户。
		return 0, fmt.Errorf("%w: draft %d", ErrDraftGone, id)
	}
	return now, nil
}

// RenameDraft 改标题。空标题是合法的（用户清空了输入框），
// 界面上会显示成"未命名"，而不是替用户编一个名字。
func (d *DB) RenameDraft(ctx context.Context, id int64, title string) error {
	res, err := d.w.ExecContext(ctx,
		`UPDATE drafts SET title = ?, updated_at = ? WHERE id = ?`,
		title, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: rename draft %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: draft %d", ErrDraftGone, id)
	}
	return nil
}

// ArchiveDraft 软删除（进归档区）。
//
// 只写 archived_at，**不动 sort_order**：恢复时它回到原来的位置，
// 而不是因为"删了一次"就跳到末尾。
func (d *DB) ArchiveDraft(ctx context.Context, id int64) error {
	res, err := d.w.ExecContext(ctx,
		`UPDATE drafts SET archived_at = ? WHERE id = ? AND archived_at IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: archive draft %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: draft %d", ErrDraftGone, id)
	}
	return nil
}

// RestoreDraft 从归档区恢复。
func (d *DB) RestoreDraft(ctx context.Context, id int64) error {
	res, err := d.w.ExecContext(ctx,
		`UPDATE drafts SET archived_at = NULL WHERE id = ? AND archived_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("store: restore draft %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: draft %d", ErrDraftGone, id)
	}
	return nil
}

// ReorderDrafts 按给定的 id 顺序重写 sort_order（第一个变成 1）。
//
// 整表重编号而不是分数索引（见 draftsDDL 的注释）。**只在事务里写**：
// 中途失败会留下一半旧序、一半新序的状态，那比"完全没生效"更难解释。
//
// ids 里不存在的 id 会被静默跳过（目录可能与库之间隔着一次删除）。
// 没出现在 ids 里的草稿（例如并发新建的）不动——它的 sort_order 仍然
// 与旧序可比，不会跑到最前面。
func (d *DB) ReorderDrafts(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: reorder drafts begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE drafts SET sort_order = ? WHERE id = ?`, i+1, id); err != nil {
			return fmt.Errorf("store: reorder draft %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: reorder drafts commit: %w", err)
	}
	return nil
}

// PurgeDraft 物理删除一条草稿（归档区里的"彻底删除"）。
//
// 与 items.Purge 同样的纪律：**不在这里删 blob 文件**。blobs/ 是内容寻址的，
// 同一份图片可能同时被剪贴板条目和另一条草稿引用；文件回收统一交给 GC 的
// 孤儿扫描，它的判据是"没有任何引用 + mtime 超过一天"。
func (d *DB) PurgeDraft(ctx context.Context, id int64) (int64, error) {
	res, err := d.w.ExecContext(ctx, `DELETE FROM drafts WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("store: purge draft %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge draft rows affected: %w", err)
	}
	return n, nil
}

// PurgeArchivedDraftsBefore 硬删归档超过 cutoff 的草稿（§5.4 第 3 步）。
//
// 返回被删掉的**正文**，调用方据此决定要不要回收它们的 blob——不返回 ID
// 是因为行已经没了，而 GC 的孤儿扫描本来就按磁盘上的文件反查引用，
// 不依赖这批 ID。返回正文只是为了让调用方能算"这一轮释放了多少字符"。
func (d *DB) PurgeArchivedDraftsBefore(ctx context.Context, cutoff int64) (purged int64, chars int64, err error) {
	// 先取待删的正文长度，再删。分两步而不是用 RETURNING：
	// RETURNING 要 SQLite 3.35+，而这里的构建标签是别人控制的
	// （sqlite_fts5 与版本无关），不值得为一个统计数字去押版本。
	if err := d.w.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(length(md)), 0) FROM drafts
		  WHERE archived_at IS NOT NULL AND archived_at <= ?`, cutoff).Scan(&chars); err != nil {
		return 0, 0, fmt.Errorf("store: sum purged draft chars: %w", err)
	}
	res, err := d.w.ExecContext(ctx,
		`DELETE FROM drafts WHERE archived_at IS NOT NULL AND archived_at <= ?`, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("store: purge archived drafts: %w", err)
	}
	purged, err = res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("store: purge archived drafts rows affected: %w", err)
	}
	if purged == 0 {
		chars = 0
	}
	return purged, chars, nil
}

// ── 备份：流式导出所需的读 ───────────────────────────────────────

// CountDraftsTx 在给定连接上数草稿（导出预统计用）。
//
// 为什么不能用 CountDrafts：导出走的是**只读事务**，stats 里的数字必须与
// 后面两遍扫描看到的是同一份快照。走 d.r（另一个连接、另一个快照）时，
// 导出期间用户新写一条草稿就会让 stats.drafts 与实际写出的条数对不上——
// 而那个数字正是导入侧的炸弹防护基准（BACKUP-FORMAT §9）。
func (d *DB) CountDraftsTx(ctx context.Context, q RowQuerier) (alive, archived int64, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN archived_at IS NULL     THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN archived_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		 FROM drafts`).Scan(&alive, &archived)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count drafts (tx): %w", err)
	}
	return alive, archived, nil
}

// StreamAliveDrafts 流式遍历**存活**草稿（含完整正文），返回条数。
//
// 三条设计决定：
//
//   - **归档的不导出**。与 items 不导出回收站同一条理由
//     （docs/BACKUP-FORMAT.md §5）：那已经是待删数据，用户预期备份里是
//     "我的东西"，不是"外加那些本来就要消失的"。
//   - **一条一条回调，不 collect 成切片**。与 items 导出同一条内存纪律：
//     任一时刻只持有当前这一条正文。草稿的正文是用户手写的，长度没有上限，
//     攒成切片就真的会随草稿总量线性涨内存。
//   - **按 (sort_order, id) 升序**，与目录顺序一致。导入方按这个顺序
//     追加，就不必理解源机器的 sort_order 数值。
func (d *DB) StreamAliveDrafts(ctx context.Context, q RowsQuerier, fn func(*Draft) error) (int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, title, md, COALESCE(seq, 0), sort_order, created_at, updated_at
		   FROM drafts WHERE archived_at IS NULL
		  ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return 0, fmt.Errorf("store: scan drafts: %w", err)
	}
	defer rows.Close()

	var n int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		var dr Draft
		if err := rows.Scan(&dr.ID, &dr.Title, &dr.MD, &dr.Seq,
			&dr.SortOrder, &dr.CreatedAt, &dr.UpdatedAt); err != nil {
			return n, fmt.Errorf("store: scan draft: %w", err)
		}
		if err := fn(&dr); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("store: iterate drafts: %w", err)
	}
	return n, nil
}

// ImportedDraft 是一条待写入的导入草稿（.clipbak §8.2 第 4 步的草稿版）。
type ImportedDraft struct {
	Title string
	MD    string
	// CreatedAt / UpdatedAt 保留源机器的时间（epoch 秒）：导入的语义是
	// "把这些数据搬过来"，不是"我刚写了一条"。
	CreatedAt int64
	UpdatedAt int64
}

// InsertImportedDraft 插入一条导入的草稿，返回新 id。
//
// 与 CreateDraft 的三点差别，每一点都有理由：
//
//  1. **保留源机器的 created_at / updated_at**（CreateDraft 一律取 now）。
//  2. **写 import_id**（CreateDraft 不写），否则"撤销导入"撤不掉草稿——
//     items 的回滚就是靠这一列（见 imports.go 的 DeleteImportItems）。
//  3. **seq 一律写 NULL**。源机器的编号对本机没有意义：留着会与本机编号
//     撞号，目录里就出现两条"草稿 3"；而默认名的生成（NextDraftSeq）
//     依赖"未占用的最小正整数"，被外来编号占掉几位是纯粹的自伤。
//     标题本身是原样带过来的，用户看到的文字一点没变。
//
// sort_order 由本函数自己取"当前最大 +1"（与 CreateDraft 同一条规则），
// 导入方只需按包里的顺序依次调用，相对顺序就保住了。
func (d *DB) InsertImportedDraft(ctx context.Context, ex Execer, importID int64, src *ImportedDraft) (int64, error) {
	if src == nil {
		return 0, errors.New("store: nil imported draft")
	}
	// 时间兜底：包里缺字段（老包）时给一个合法值，而不是 0（1970）。
	created, updated := src.CreatedAt, src.UpdatedAt
	if created <= 0 {
		created = time.Now().Unix()
	}
	if updated <= 0 {
		updated = created
	}
	// 用 Exec + LastInsertId 而不是 INSERT ... RETURNING：与同文件的
	// CreateDraft 保持一种写法。写句柄是单连接池，LastInsertId 落在同一
	// 条连接上；drafts 上没有触发器，不存在"取到的 id 不是自己的"的窗口。
	res, err := ex.ExecContext(ctx,
		`INSERT INTO drafts (title, md, seq, sort_order, created_at, updated_at, import_id)
		 VALUES (?, ?, NULL,
		         (SELECT COALESCE(MAX(sort_order), 0) + 1 FROM drafts),
		         ?, ?, ?)`,
		src.Title, src.MD, created, updated, importID)
	if err != nil {
		return 0, fmt.Errorf("store: insert imported draft: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: imported draft id: %w", err)
	}
	return id, nil
}

// ── blob 引用：从正文里抽，而不是维护第二张表 ────────────────────

// forEachDraftMD 逐行流式读草稿正文，回调处理一条就丢一条。
//
// 为什么不是"全查出来再遍历"：孤儿扫描每轮 GC 都跑一次（默认 60s），
// 把全部草稿正文一次性读进内存会让常驻内存随草稿总量线性增长，
// 而 §1.1 的内存预算是按"面板收起 30 MB"定的。
//
// 分批（LIMIT + 游标推进）而不是一条大查询：写句柄是单连接池，
// 长时间持有 rows 会挡住捕获写入。
func (d *DB) forEachDraftMD(ctx context.Context, fn func(md string) error) error {
	const batch = 64
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 先收集本批再处理：处理期间不能再持有 rows（见上面那条注释）。
		type row struct {
			id int64
			md string
		}
		buf := make([]row, 0, batch)
		rows, err := d.r.QueryContext(ctx,
			`SELECT id, md FROM drafts WHERE id > ? ORDER BY id ASC LIMIT ?`, lastID, batch)
		if err != nil {
			return fmt.Errorf("store: scan draft md: %w", err)
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.md); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan draft md row: %w", err)
			}
			buf = append(buf, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("store: iterate draft md: %w", err)
		}
		if len(buf) == 0 {
			return nil
		}
		for _, r := range buf {
			if err := fn(r.md); err != nil {
				return err
			}
			lastID = r.id
		}
	}
}

// DraftBlobRefsInMD 抽出正文里引用的全部 blob 相对路径（按出现顺序，可重复）。
//
// 认的形式是 `](blob/9f/2a/<sha>.png)`。前缀 `blob/` 与 `blobs/`（包内路径）
// 不会混淆：`HasPrefix("blobs/x", "blob/")` 为假。
func DraftBlobRefsInMD(md string) []string {
	var out []string
	rewriteMDRefs(md, func(url string) string {
		if rel, ok := draftRelFromURL(url); ok {
			out = append(out, rel)
		}
		return url
	})
	return out
}

// DraftBlobRefsToPackage 把正文里的 `blob/<rel>` 改写成 `blobs/<rel>`（导出用）。
//
// 为什么包内路径多一个 s：包里的目录就叫 `blobs/`（docs/BACKUP-FORMAT.md
// §2 的结构），而库里存的 URL 前缀是 `blob/`。这是**格式决定的两套写法**，
// 导出/导入各转一次，库里的正文永远只有一种写法。
func DraftBlobRefsToPackage(md string) string {
	return rewriteMDRefs(md, func(url string) string {
		if rel, ok := draftRelFromURL(url); ok {
			return "blobs/" + rel
		}
		return url
	})
}

// DraftBlobRefsFromPackage 把 `blobs/<rel>` 改回 `blob/<rel>`（导入用）。
//
// keep 返回 false 的引用会被替换成 `]()`（空 URL）：目标 blob 不在包里，
// 留着就是一个必然破图的引用，而"图裂了"和"图没了"对用户是两件事——
// 后者至少说明白了这个包不完整。返回去掉的总数，供导入报告使用。
func DraftBlobRefsFromPackage(md string, keep func(rel string) bool) (string, int) {
	dropped := 0
	out := rewriteMDRefs(md, func(url string) string {
		const pkgPrefix = "blobs/"
		if !strings.HasPrefix(url, pkgPrefix) {
			return url
		}
		rel := url[len(pkgPrefix):]
		if keep != nil && !keep(rel) {
			dropped++
			return ""
		}
		return DraftBlobPrefix + rel
	})
	return out, dropped
}

// DraftBlobRefsInPackageMD 抽出正文里的**包内** blob 引用（`blobs/<rel>`）。
//
// 与 DraftBlobRefsInMD 配对：那个认库内前缀 `blob/`，这个认包内前缀 `blobs/`。
// 分成两个函数而不是给一个加参数：两个前缀各自对应一侧的**唯一正确写法**，
// 合成一个带参数的函数就等于允许"用错前缀去扫"这件事发生。
//
// 导入侧先用它清点"这条草稿要落盘哪些文件"，核对 sha256 之后再据结果
// 决定正文里保留哪些（DraftBlobRefsFromPackage）。
func DraftBlobRefsInPackageMD(md string) []string {
	const pkgPrefix = "blobs/"
	var out []string
	rewriteMDRefs(md, func(url string) string {
		if strings.HasPrefix(url, pkgPrefix) && len(url) > len(pkgPrefix) {
			out = append(out, url[len(pkgPrefix):])
		}
		return url
	})
	return out
}

// draftRelFromURL 判断一个 `](…)` 里的 URL 是不是草稿 blob 引用，
// 是则返回去掉前缀的相对路径。
func draftRelFromURL(url string) (string, bool) {
	if !strings.HasPrefix(url, DraftBlobPrefix) {
		return "", false
	}
	rel := url[len(DraftBlobPrefix):]
	if rel == "" {
		return "", false
	}
	return rel, true
}

// rewriteMDRefs 把 md 里每一处 `](URL)` 的 URL 交给 fn，用返回值替换。
//
// 用文本扫描而不是上 Markdown 解析器：这里要的不是"理解 Markdown"，
// 而是"找出所有链接目标的字面量"。这个取舍带来两条必须自己兑现的约定，
// 都是**为了不误伤用户正文**：
//
//  1. **转义的 `]` 不构成链接。** 生成 md 的那一侧必须把正文里的 `]`
//     转义成 `\]`（md.ts 的 escapeText），这里则必须识别它——否则用户在
//     正文里打一个 `](` 就能骗过扫描器，让一段普通文字被当成图片引用。
//     两边的约定是一对，缺任何一边都会失效，所以 test/check-md.mjs 里
//     有一条对应用例盯着前端那一半。
//  2. **URL 在空白与括号处截断。** 手工改过的 md 里可能出现未闭合的
//     `](blob/x.png 后面还有字`。此时按"目标到第一个空白/括号为止"截断，
//     宁可多算一个引用（多算 = 文件留着，是保守方向），也不要把后面
//     整段文字吞进 URL。
func rewriteMDRefs(md string, fn func(url string) string) string {
	if md == "" || !strings.Contains(md, "](") {
		return md
	}
	var b strings.Builder
	b.Grow(len(md))
	rest := md
	for {
		i := indexUnescapedLinkClose(rest)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		start := i + 2
		n := mdURLLen(rest[start:])
		b.WriteString(rest[:start])
		if n == 0 {
			// `](` 后面直接是空白或括号：这不是链接目标。原样写出去、
			// 从这里继续扫（至少前进 2 个字节，不会死循环）。
			rest = rest[start:]
			continue
		}
		b.WriteString(fn(rest[start : start+n]))
		rest = rest[start+n:]
	}
}

// indexUnescapedLinkClose 找 `](` 中那个**未被转义**的 `]` 的位置；
// 找不到返回 -1。
func indexUnescapedLinkClose(s string) int {
	from := 0
	for {
		i := strings.Index(s[from:], "](")
		if i < 0 {
			return -1
		}
		i += from
		if !isEscapedAt(s, i) {
			return i
		}
		from = i + 1
	}
}

// isEscapedAt 报告 s[i] 前面是否有奇数个连续反斜杠（即它被转义了）。
func isEscapedAt(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// mdURLLen 返回从 s 开头的 URL 的长度（到第一个空白或圆括号为止）。
func mdURLLen(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(', ')':
			return i
		}
	}
	return len(s)
}

// DraftSnippet 把正文前若干字符整理成一行摘要（目录用）。
//
// 只做最基本的清理：剥掉图片与链接的 Markdown 外壳、去掉行首标记、
// 把换行压成空格。**不做富文本渲染**——摘要是给人扫一眼的，
// 多做一步就多一处与正文不一致的可能。
func DraftSnippet(raw string) string {
	s := raw
	// 换行与连续空白压成单个空格（中文之间不留空格的习惯不在这里照顾，
	// 摘要本来就允许粗糙）。
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\t", " ").Replace(s)
	// 图片整个丢掉（`![alt](url)`）：摘要是正文的文字摘要，
	// 一个必然破图的 URL 留在里面只会显得像乱码。
	s = stripMarkdownImages(s)
	s = stripMarkdownLinkTargets(s)
	for _, mark := range []string{"**", "__", "*", "_", "`", "\\"} {
		s = strings.ReplaceAll(s, mark, "")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > snippetChars {
		return string(r[:snippetChars]) + "…"
	}
	return s
}

// stripMarkdownImages 去掉 `![alt](url)` 整体。
func stripMarkdownImages(s string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "![")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		j := strings.IndexByte(rest[i:], ')')
		if j < 0 {
			return b.String() // 没闭合：后面整段丢掉，免得留下半个标记
		}
		rest = rest[i+j+1:]
	}
}

// stripMarkdownLinkTargets 把 `[文字](url)` 压成 `文字`。
func stripMarkdownLinkTargets(s string) string {
	var b strings.Builder
	rest := s
	for {
		open := strings.IndexByte(rest, '[')
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		// 找 `](` 作为"标题与目标的分界"，找不到就保留原文继续。
		mid := strings.Index(rest[open:], "](")
		if mid < 0 {
			b.WriteString(rest[open:])
			return b.String()
		}
		mid += open
		end := strings.IndexByte(rest[mid:], ')')
		if end < 0 {
			b.WriteString(rest[open:])
			return b.String()
		}
		b.WriteString(rest[open+1 : mid]) // 只保留方括号里的文字
		rest = rest[mid+end+1:]
	}
}

// ErrDraftGone 表示目标草稿已经不存在。
//
// 单独一个错误值而不是笼统的 "not found"：实时保存路径要把这一种
// 翻译成"这条草稿已被删除"，而不是"保存失败，请重试"——后者会让用户
// 一直重试一件永远不会成功的事。
var ErrDraftGone = errors.New("store: draft not found")

// nullableSeq 把"没有编号"（0）写成 NULL。
//
// 用 NULL 而不是 0 表示未编号：0 会参与 NextDraftSeq 的比对逻辑
// （它比任何合法编号都小），把"未编号"误当成"编号 0 已被占用"。
func nullableSeq(seq int64) any {
	if seq <= 0 {
		return nil
	}
	return seq
}
