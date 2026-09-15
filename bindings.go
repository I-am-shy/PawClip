package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zego/pawclip/backup"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// 本文件是**前端能调用的一切**（DESIGN §10 里 app.go 的职责）。
//
// 三条自律：
//
//  1. **列表返回的是 ListRow，不是 Item。** Item 里有 text_content 全文，
//     一页 50 条、每条几 MB 的文本会把 IPC 撑爆。预览用 preview，全文按需
//     用 Get(id) 取单条。
//  2. **图片永远不在这层出现。** 返回的是 blobURL()（相对 URL），
//     由 AssetServer 直接服务（§14 第 1 条）。
//  3. **写操作只走 store 的方法，不拼 SQL。** 唯一例外是设置（SetSetting）。

// ListOptions 是前端传进来的列表/检索参数。
//
// 与 store.Query 一一对应但不直接暴露后者：绑定层的入参是**跨进程契约**，
// 改它会破坏前端；store.Query 是内部类型，可以随重构调整。中间隔一层
// 是为了让"前端契约"与"内部类型"能各自演进。
type ListOptions struct {
	Text          string   `json:"text"`
	Kinds         []string `json:"kinds"`
	CategoryID    *int64   `json:"categoryId"`
	Uncategorized bool     `json:"uncategorized"`
	TagID         *int64   `json:"tagId"`
	SourceAppID   string   `json:"sourceAppId"`
	PinnedOnly    bool     `json:"pinnedOnly"`
	Trashed       bool     `json:"trashed"`
	Since         *int64   `json:"since"`
	Until         *int64   `json:"until"`
	Cursor        *Cursor  `json:"cursor"`
	Limit         int      `json:"limit"`
	IncludeTotal  bool     `json:"includeTotal"`
}

// Cursor 是 keyset 分页游标（§14 第 3 条：不用 OFFSET）。
type Cursor struct {
	CreatedAt int64 `json:"createdAt"`
	ID        int64 `json:"id"`
}

func (o ListOptions) toQuery() store.Query {
	q := store.Query{
		Text:          o.Text,
		Kinds:         o.Kinds,
		CategoryID:    o.CategoryID,
		Uncategorized: o.Uncategorized,
		TagID:         o.TagID,
		SourceAppID:   o.SourceAppID,
		PinnedOnly:    o.PinnedOnly,
		Trashed:       o.Trashed,
		Since:         o.Since,
		Until:         o.Until,
		Limit:         o.Limit,
		IncludeTotal:  o.IncludeTotal,
	}
	if o.Cursor != nil {
		q.Cursor = &store.Cursor{CreatedAt: o.Cursor.CreatedAt, ID: o.Cursor.ID}
	}
	return q
}

// ListRow 是列表里的一条（元数据 + 可直接用的 URL，不含全文）。
type ListRow struct {
	ID            int64   `json:"id"`
	Kind          string  `json:"kind"`
	Preview       string  `json:"preview"`
	Fingerprint   string  `json:"fingerprint"`
	ByteSize      int64   `json:"byteSize"`
	SourceAppID   string  `json:"sourceAppId"`
	SourceAppName string  `json:"sourceAppName"`
	SourceURL     string  `json:"sourceUrl"`
	CategoryID    *int64  `json:"categoryId"`
	Pinned        bool    `json:"pinned"`
	FirstSeenAt   int64   `json:"firstSeenAt"`
	ExpiresAt     *int64  `json:"expiresAt"`
	TTLSource     string  `json:"ttlSource"`
	CreatedAt     int64   `json:"createdAt"`
	LastUsedAt    *int64  `json:"lastUsedAt"`
	UseCount      int64   `json:"useCount"`
	DeletedAt     *int64  `json:"deletedAt"`
	TextLen       int64   `json:"textLen"`
	FileCount     int     `json:"fileCount"`
	TagIDs        []int64 `json:"tagIds"`

	// ThumbURL / ImageURL 是**相对 URL**（`blob/9f/2a/xxx.thumb.png`），
	// 直接塞进 <img src>。图片字节不走 IPC。
	ThumbURL string `json:"thumbUrl"`
	ImageURL string `json:"imageUrl"`
	// TextURL 是外置的 HTML 正文（>64 KB 的那种）在包里的位置；
	// 库里没有 html_path 列，所以 M1/M2 一律内联进 html_content，
	// 这个字段留空，前端用 Get(id) 拿全文。
	HasHTML bool `json:"hasHtml"`
}

func toListRow(r store.ListRow) ListRow {
	return ListRow{
		ID:            r.ID,
		Kind:          r.Kind,
		Preview:       r.Preview,
		Fingerprint:   r.Fingerprint,
		ByteSize:      r.ByteSize,
		SourceAppID:   r.SourceAppID,
		SourceAppName: r.SourceAppName,
		SourceURL:     r.SourceURL,
		CategoryID:    r.CategoryID,
		Pinned:        r.Pinned,
		FirstSeenAt:   r.FirstSeenAt,
		ExpiresAt:     r.ExpiresAt,
		TTLSource:     r.TTLSource,
		CreatedAt:     r.CreatedAt,
		LastUsedAt:    r.LastUsedAt,
		UseCount:      r.UseCount,
		DeletedAt:     r.DeletedAt,
		TextLen:       r.TextLen,
		FileCount:     r.FileCount,
		TagIDs:        r.TagIDs,
		ThumbURL:      blobURL(r.ThumbPath),
		ImageURL:      blobURL(r.ImagePath),
	}
}

// PageResult 是一页结果（含检索诊断，便于设置界面里的"为什么搜不到"排查）。
type PageResult struct {
	Rows         []ListRow `json:"rows"`
	NextCursor   *Cursor   `json:"nextCursor"`
	HasMore      bool      `json:"hasMore"`
	Mode         string    `json:"mode"`
	TookMs       int64     `json:"tookMs"`
	Total        int64     `json:"total"`
	TotalValid   bool      `json:"totalValid"`
	FTSAvailable bool      `json:"ftsAvailable"`
}

// List 走列表或检索（text 非空即为检索）。
func (a *App) List(opts ListOptions) (*PageResult, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	pg, err := db.List(a.baseCtx(), opts.toQuery())
	if err != nil {
		return nil, err
	}
	out := &PageResult{
		Rows:         make([]ListRow, 0, len(pg.Rows)),
		HasMore:      pg.HasMore,
		Mode:         string(pg.Mode),
		TookMs:       pg.TookMs,
		Total:        pg.Total,
		TotalValid:   pg.TotalValid,
		FTSAvailable: pg.FTSAvailable,
	}
	for _, r := range pg.Rows {
		out.Rows = append(out.Rows, toListRow(r))
	}
	if pg.NextCursor != nil {
		out.NextCursor = &Cursor{CreatedAt: pg.NextCursor.CreatedAt, ID: pg.NextCursor.ID}
	}
	return out, nil
}

// ItemDetail 是单条条目的完整内容（用户点开预览时才调）。
type ItemDetail struct {
	ListRow
	Text string `json:"text"`
	HTML string `json:"html"`
	RTF  string `json:"rtf"`
	// FilePaths 是**原始路径**（可能已不存在，前端只展示）。
	FilePaths []string `json:"filePaths"`
	// ImageWidth / ImageHeight 只有图片类非零。
	ImageWidth  int `json:"imageWidth"`
	ImageHeight int `json:"imageHeight"`
}

// Get 取单条详情。
func (a *App) Get(id int64) (*ItemDetail, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := a.baseCtx()
	it, err := db.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	d := &ItemDetail{
		ListRow: ListRow{
			ID:            it.ID,
			Kind:          it.Kind,
			Preview:       it.Preview,
			Fingerprint:   it.Fingerprint,
			ByteSize:      it.ByteSize,
			SourceAppID:   it.SourceAppID,
			SourceAppName: it.SourceAppName,
			CategoryID:    it.CategoryID,
			Pinned:        it.Pinned,
			FirstSeenAt:   it.FirstSeenAt,
			ExpiresAt:     it.ExpiresAt,
			CreatedAt:     it.CreatedAt,
			LastUsedAt:    it.LastUsedAt,
			UseCount:      it.UseCount,
			DeletedAt:     it.DeletedAt,
			FileCount:     len(it.FilePaths),
		},
		FilePaths: it.FilePaths,
	}
	if it.TextContent != nil {
		d.Text = *it.TextContent
		d.TextLen = int64(len([]rune(*it.TextContent)))
	}
	if it.HTMLContent != nil {
		d.HTML = *it.HTMLContent
		d.HasHTML = true
	}
	if it.SourceURL != nil {
		d.SourceURL = *it.SourceURL
	}
	if it.ImagePath != nil {
		d.ImageURL = blobURL(*it.ImagePath)
		// 尺寸只对图片有意义；取不到就算了（前端按加载后的实际尺寸布局）。
		if a.blobsReady() {
			if w, h, err := store.DecodePNGSizeFromFile(a.blobAbs(*it.ImagePath)); err == nil {
				d.ImageWidth, d.ImageHeight = w, h
			}
		}
	}
	if it.ThumbPath != nil {
		d.ThumbURL = blobURL(*it.ThumbPath)
	}
	if it.RTFPath != nil {
		d.RTF = *it.RTFPath
	}
	if ids, err := db.ItemTags(ctx, id); err == nil {
		d.TagIDs = ids
	}
	return d, nil
}

// ── 写操作 ──────────────────────────────────────────────────────

// Delete 把条目移入回收站（软删除，可在回收站恢复）。
func (a *App) Delete(ids []int64) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	return db.SoftDelete(a.baseCtx(), ids)
}

// Restore 从回收站恢复。返回恢复成功的条数与"因指纹冲突没能恢复"的条数。
//
// conflict 必须返回给用户：恢复失败不是错误，但用户需要知道为什么
// 那几条没回来（回收站里那条的指纹已经被"删掉后又重新复制进来的"占住了）。
func (a *App) Restore(ids []int64) (restored int64, conflict int, err error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, 0, err
	}
	return db.Restore(a.baseCtx(), ids)
}

// Purge 物理删除（不可恢复）。blob 文件交给 GC 的孤儿扫描回收。
func (a *App) Purge(ids []int64) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	return db.Purge(a.baseCtx(), ids)
}

// EmptyTrash 清空回收站。
func (a *App) EmptyTrash() (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	return db.EmptyTrash(a.baseCtx())
}

// Pin 置顶 / 取消置顶。置顶会同时清掉过期时间（§5.1：置顶强制凌驾全部规则）。
func (a *App) Pin(ids []int64, pinned bool) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.SetPinned(a.baseCtx(), ids, pinned)
}

// SetCategory 改分类；categoryID 为 nil 表示移到"未分类"。
func (a *App) SetCategory(ids []int64, categoryID *int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.SetCategory(a.baseCtx(), ids, categoryID)
}

// SetExpiry 改过期时间。
//
// expiresAt 为 nil 表示"永不过期"。这两种情况 store.SetExpiry 会分别写
// ttl_source = 'item' / 'never'——**这个区分不能省**：§5.1 的三级 TTL 优先级
// 靠它判断，只写 expires_at = NULL 而不钉住 ttl_source 的话，下一轮 GC 的
// ApplyDefaultTTL 会重新算出一个到期时间（②级全局默认 TTL 会盖上来），
// 用户设的"永不"就不生效了。
func (a *App) SetExpiry(ids []int64, expiresAt *int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.SetExpiry(a.baseCtx(), ids, expiresAt)
}

// ── 分类与标签 ─────────────────────────────────────────────────

// Categories 列出全部分类（含条目数）。
func (a *App) Categories() ([]store.Category, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	return db.ListCategories(a.baseCtx())
}

// SaveCategory 新建或更新一个分类。
//
// c.ID 为 0 表示新建。分类名有唯一约束，重名会返回
// store.ErrCategoryNameTaken —— 前端要把它翻译成"这个名字已经有了"，
// 而不是弹一个技术错误。
func (a *App) SaveCategory(c store.Category) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	ctx := a.baseCtx()
	if c.ID == 0 {
		return db.CreateCategory(ctx, &c)
	}
	return c.ID, db.UpdateCategory(ctx, &c)
}

// DeleteCategory 删除分类。条目不会被删，它们的 category_id 归 NULL
// （外键是 ON DELETE SET NULL）；这一点取决于 foreign_keys=ON，
// 由 store 的启动自检保证。
func (a *App) DeleteCategory(id int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.DeleteCategory(a.baseCtx(), id)
}

// Tags 列出全部标签（含条目数）。
func (a *App) Tags() ([]store.Tag, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	return db.ListTags(a.baseCtx())
}

// SaveTag 新建或改名标签（都按"同名即复用"处理）。
func (a *App) SaveTag(id int64, name, color string) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	ctx := a.baseCtx()
	if id == 0 {
		return db.CreateTag(ctx, name, color)
	}
	return id, db.RenameTag(ctx, id, name, color)
}

// DeleteTag 删标签。item_tags 会级联清理，条目本身不受影响。
func (a *App) DeleteTag(id int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.DeleteTag(a.baseCtx(), id)
}

// SetItemTags 覆盖式设定某条目的标签集合。
func (a *App) SetItemTags(itemID int64, tagIDs []int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.SetItemTags(a.baseCtx(), itemID, tagIDs)
}

// ── 回写与自动粘贴 ──────────────────────────────────────────────

// Paste 把某条历史写回剪贴板（可选自动粘贴）。
//
// 走的是与捕获共享同一个 SelfWriteGuard 的 Writeback——这是"用一次历史
// 不会多出一条记录"（验收判据 1）成立的前提。
func (a *App) Paste(id int64, autoPaste bool) (*PasteResult, error) {
	wb, err := a.writeback()
	if err != nil {
		return nil, err
	}
	return wb.Paste(a.baseCtx(), id, autoPaste)
}

// CopyOnly 只写剪贴板，不模拟粘贴。
func (a *App) CopyOnly(id int64) (*PasteResult, error) {
	wb, err := a.writeback()
	if err != nil {
		return nil, err
	}
	return wb.CopyOnly(a.baseCtx(), id)
}

// PasteText 直接把一段文本写进剪贴板（用于"内容转换器"的输出）。
func (a *App) PasteText(text string) error {
	wb, err := a.writeback()
	if err != nil {
		return err
	}
	return wb.PasteText(text)
}

// StartSequence 进入"连续粘贴"模式（§11 P2）。
func (a *App) StartSequence(ids []int64) (int, error) {
	wb, err := a.writeback()
	if err != nil {
		return 0, err
	}
	return wb.StartSequence(a.baseCtx(), ids)
}

// NextInSequence 连续粘贴的下一项。
func (a *App) NextInSequence() (*PasteResult, error) {
	wb, err := a.writeback()
	if err != nil {
		return nil, err
	}
	return wb.NextInSequence(a.baseCtx())
}

// SequenceState 报告连续粘贴的进度。
func (a *App) SequenceState() SequenceState {
	wb, err := a.writeback()
	if err != nil {
		return SequenceState{}
	}
	return SequenceState{
		Remaining: wb.SequenceRemaining(),
		Total:     wb.SequenceTotal(),
		Active:    wb.SequenceTotal() > 0,
	}
}

// SequenceState 是连续粘贴的进度快照。
type SequenceState struct {
	Active    bool `json:"active"`
	Remaining int  `json:"remaining"`
	Total     int  `json:"total"`
}

// ClearSequence 退出连续粘贴模式。
func (a *App) ClearSequence() { a.clearSequence() }

func (a *App) clearSequence() {
	if wb, err := a.writeback(); err == nil {
		wb.ClearSequence()
	}
}

// ── 面板 / 托盘 ─────────────────────────────────────────────────

// ShowPanel 呼出面板（免抢焦点）。
func (a *App) ShowPanel() error {
	ctrl := a.panelController()
	if ctrl == nil {
		return panel.ErrUnsupported
	}
	return ctrl.Show()
}

// HidePanel 收起面板。
func (a *App) HidePanel() {
	if ctrl := a.panelController(); ctrl != nil {
		ctrl.Hide()
	}
}

// PanelVisible 报告面板是否可见。
func (a *App) PanelVisible() bool {
	if ctrl := a.panelController(); ctrl != nil {
		return ctrl.Visible()
	}
	return false
}

// PanelDiag 返回一次面板诊断快照（平台无关时是 `{}`）。
//
// 存在的意义是"能让验收在真机上取证"：免抢焦点这件事没有单元测试可写，
// 只能靠把 NSPanel 的 class / styleMask / 激活策略读出来核对。
func (a *App) PanelDiag() string {
	if ctrl := a.panelController(); ctrl != nil {
		return ctrl.Diag()
	}
	return "{}"
}

// AutoPasteAvailable 报告自动粘贴是否可用（macOS 需要辅助功能授权）。
//
// 返回 false 时前端要把"直贴"按钮降级成"只复制"并说明原因——
// §13 风险表明确要求"如实降级，不假装贴上了"。
func (a *App) AutoPasteAvailable() bool {
	if ctrl := a.panelController(); ctrl != nil {
		return ctrl.CanAutoPaste()
	}
	return false
}

// RequestAutoPaste 打开系统的辅助功能授权引导页。
func (a *App) RequestAutoPaste() {
	if ctrl := a.panelController(); ctrl != nil {
		ctrl.RequestAutoPaste()
	}
}

// ── 统计 ────────────────────────────────────────────────────────

// StatsSummary 是统计面板的数据。
//
// ⚠️ 名字不叫 Stats 是因为 app.go 里已经有一个 Stats 了（捕获流水线的计数
// 投影，是 health 的一部分）。两个都是"统计"，但一个是"捕获了多少次"、
// 一个是"库里有什么"——同名会让读代码的人搞错，也让包级声明冲突。
type StatsSummary struct {
	Alive      int64                 `json:"alive"`
	Trashed    int64                 `json:"trashed"`
	All        int64                 `json:"all"`
	AliveBytes int64                 `json:"aliveBytes"`
	Events     int64                 `json:"events"`
	DiskBytes  int64                 `json:"diskBytes"`
	DiskFiles  int64                 `json:"diskFiles"`
	Kinds      []store.KindStat      `json:"kinds"`
	TopApps    []store.SourceAppStat `json:"topApps"`
	LastGC     *GCReport             `json:"lastGC"`
	GCRuns     int64                 `json:"gcRuns"`
	Paused     bool                  `json:"paused"`
}

// GCReport 是一轮 GC 的结果投影。
type GCReport struct {
	StartedAt      string   `json:"startedAt"`
	TookMs         int64    `json:"tookMs"`
	TTLApplied     int64    `json:"ttlApplied"`
	Protected      int64    `json:"protected"`
	Trashed        int64    `json:"trashed"`
	Deleted        int64    `json:"deleted"`
	Archived       int64    `json:"archived"`
	Purged         int64    `json:"purged"`
	Evicted        int64    `json:"evicted"`
	EvictedBytes   int64    `json:"evictedBytes"`
	OrphansRemoved int64    `json:"orphansRemoved"`
	OrphanBytes    int64    `json:"orphanBytes"`
	Vacuumed       bool     `json:"vacuumed"`
	Alive          int64    `json:"alive"`
	TotalBytes     int64    `json:"totalBytes"`
	Summary        string   `json:"summary"`
	Errors         []string `json:"errors,omitempty"`
}

// StatsOverview 返回统计面板的数据。
func (a *App) StatsOverview() (*StatsSummary, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := a.baseCtx()
	s := &StatsSummary{}

	if s.Alive, err = db.CountAlive(ctx); err != nil {
		return nil, err
	}
	if s.Trashed, err = db.CountTrashed(ctx); err != nil {
		return nil, err
	}
	if s.All, err = db.CountAll(ctx); err != nil {
		return nil, err
	}
	if s.AliveBytes, err = db.TotalAliveBytes(ctx); err != nil {
		return nil, err
	}
	// Events 用事件总数（ticks）而不是条目数——用户问"记录了多少次"时，
	// 想知道的是"我复制了多少回"，去重之后的条目数会小得多。
	if s.Kinds, err = db.KindStats(ctx); err != nil {
		return nil, err
	}
	if s.TopApps, err = db.TopSourceApps(ctx, 8); err != nil {
		return nil, err
	}

	// blobs/ 的实际占用：与 byte_size 合计口径不同（前者是文件系统实际值，
	// 含缩略图与 RTF；后者是条目声明的大小）。两个数字都给，
	// 差异本身是有用信息（差得多说明孤儿文件或缩略图占比高）。
	if bs := a.blobStore(); bs != nil {
		_ = bs.Walk(func(bi store.BlobInfo) error {
			s.DiskBytes += bi.Size
			s.DiskFiles++
			return nil
		})
	}

	if gc := a.gcEngine(); gc != nil {
		s.GCRuns = gc.Runs()
		s.Paused = gc.Paused()
		if r := gc.Last(); r != nil {
			s.Events = r.Alive
			s.LastGC = &GCReport{
				StartedAt:      r.StartedAt.Format(time.RFC3339),
				TookMs:         r.TookMs,
				TTLApplied:     r.TTLApplied,
				Protected:      r.Protected,
				Trashed:        r.Trashed,
				Deleted:        r.Deleted,
				Archived:       r.Archived,
				Purged:         r.Purged,
				Evicted:        r.Evicted,
				EvictedBytes:   r.EvictedBytes,
				OrphansRemoved: r.OrphansRemoved,
				OrphanBytes:    r.OrphanBytes,
				Vacuumed:       r.Vacuumed,
				Alive:          r.Alive,
				TotalBytes:     r.TotalBytes,
				Summary:        r.Summary(),
				Errors:         r.Errors,
			}
		}
	}
	if cap := a.capturePipeline(); cap != nil {
		s.Events = cap.Stats().Ticks
	}
	return s, nil
}

// RunGC 立刻跑一轮回收（统计面板的"现在清理"按钮）。
func (a *App) RunGC() (*GCReport, error) {
	gc := a.gcEngine()
	if gc == nil {
		return nil, errors.New("pawclip: GC 尚未启动")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := gc.RunOnce(ctx)
	if r == nil {
		return nil, nil
	}
	return &GCReport{
		StartedAt:      r.StartedAt.Format(time.RFC3339),
		TookMs:         r.TookMs,
		TTLApplied:     r.TTLApplied,
		Protected:      r.Protected,
		Trashed:        r.Trashed,
		Deleted:        r.Deleted,
		Archived:       r.Archived,
		Purged:         r.Purged,
		Evicted:        r.Evicted,
		EvictedBytes:   r.EvictedBytes,
		OrphansRemoved: r.OrphansRemoved,
		OrphanBytes:    r.OrphanBytes,
		Vacuumed:       r.Vacuumed,
		Alive:          r.Alive,
		TotalBytes:     r.TotalBytes,
		Summary:        r.Summary(),
		Errors:         r.Errors,
	}, nil
}

// ── 导出 / 导入 ─────────────────────────────────────────────────

// ExportOptions 是导出参数（§7 的表格）。
type ExportOptions struct {
	Scope          string   `json:"scope"`
	CategoryID     *int64   `json:"categoryId"`
	Since          *int64   `json:"since"`
	Until          *int64   `json:"until"`
	ExcludeKinds   []string `json:"excludeKinds"`
	IncludeExpired bool     `json:"includeExpired"`
	ManifestFormat string   `json:"manifestFormat"`
	EmbedFiles     bool     `json:"embedFiles"`
	// OutputDir 为空时用数据目录下的 exports/（**不**默认落到用户家里）。
	OutputDir string `json:"outputDir"`
}

// ExportResult 是导出结果。
type ExportResult struct {
	Path         string   `json:"path"`
	Bytes        int64    `json:"bytes"`
	Items        int64    `json:"items"`
	Categories   int64    `json:"categories"`
	Tags         int64    `json:"tags"`
	BlobBytes    int64    `json:"blobBytes"`
	Blobs        int64    `json:"blobs"`
	MissingBlobs int64    `json:"missingBlobs"`
	Warnings     []string `json:"warnings,omitempty"`
	TookMs       int64    `json:"tookMs"`
}

// Export 导出一个 .clipbak。
func (a *App) Export(opts ExportOptions) (*ExportResult, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	bs := a.blobStore()
	if bs == nil {
		return nil, errors.New("pawclip: blob 存储未就绪")
	}

	outDir := strings.TrimSpace(opts.OutputDir)
	if outDir == "" {
		outDir = filepath.Join(db.Dir(), "exports")
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			return nil, fmt.Errorf("pawclip: 创建导出目录：%w", err)
		}
	}

	format := opts.ManifestFormat
	if format == "" {
		format = "json"
	}
	res, err := backup.Export(a.baseCtx(), db, bs, backup.ExportOptions{
		Scope:          backup.ExportScope(opts.Scope),
		CategoryID:     opts.CategoryID,
		Since:          opts.Since,
		Until:          opts.Until,
		ExcludeKinds:   opts.ExcludeKinds,
		IncludeExpired: opts.IncludeExpired,
		ManifestFormat: format,
		EmbedFiles:     opts.EmbedFiles,
		OutputDir:      outDir,
		AppVersion:     Version,
		Platform:       runtimeGOOS(),
	}, a.gcEngine(), a.log)
	if err != nil {
		return nil, err
	}
	return &ExportResult{
		Path:         res.Path,
		Bytes:        res.Bytes,
		Items:        res.Items,
		Categories:   res.Categories,
		Tags:         res.Tags,
		BlobBytes:    res.BlobBytes,
		Blobs:        res.Blobs,
		MissingBlobs: res.MissingBlobs,
		Warnings:     res.Warnings,
		TookMs:       res.TookMs,
	}, nil
}

// ImportOptions 是导入参数（§8.3）。
type ImportOptions struct {
	ConflictPolicy string `json:"conflictPolicy"`
	ExpiryPolicy   string `json:"expiryPolicy"`
	ImportExpired  bool   `json:"importExpired"`
	CategoryPolicy string `json:"categoryPolicy"`
	ImportSettings bool   `json:"importSettings"`
}

func (o ImportOptions) toBackup() backup.ImportOptions {
	return backup.ImportOptions{
		ConflictPolicy: o.ConflictPolicy,
		ExpiryPolicy:   o.ExpiryPolicy,
		ImportExpired:  o.ImportExpired,
		CategoryPolicy: o.CategoryPolicy,
		ImportSettings: o.ImportSettings,
	}
}

// PrecheckResult 是导入前的预检结果（确认页要展示的一切）。
type PrecheckResult struct {
	Path            string   `json:"path"`
	ManifestName    string   `json:"manifestName"`
	Format          string   `json:"format"`
	FormatVersion   int      `json:"formatVersion"`
	AppVersion      string   `json:"appVersion"`
	ExportedAt      string   `json:"exportedAt"`
	Platform        string   `json:"platform"`
	Scope           string   `json:"scope"`
	Total           int      `json:"total"`
	WillImport      int      `json:"willImport"`
	SkipDuplicate   int      `json:"skipDuplicate"`
	SkipExpired     int      `json:"skipExpired"`
	Invalid         int      `json:"invalid"`
	CategoriesNew   []string `json:"categoriesNew"`
	CategoriesReuse []string `json:"categoriesReuse"`
	TagsNew         int      `json:"tagsNew"`
	TagsReuse       int      `json:"tagsReuse"`
	Uncompressed    int64    `json:"uncompressedBytes"`
	NeedBytes       int64    `json:"needBytes"`
	AvailableBytes  int64    `json:"availableBytes"`
	BlobCount       int      `json:"blobCount"`
	Warnings        []string `json:"warnings,omitempty"`
	AlreadyImported bool     `json:"alreadyImported"`
}

// PrecheckBackup 预检一个备份包（**不写任何数据**）。
func (a *App) PrecheckBackup(pkgPath string, opts ImportOptions) (*PrecheckResult, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	if err := a.checkImportPath(pkgPath); err != nil {
		return nil, err
	}
	pc, err := backup.Precheck(a.baseCtx(), db, pkgPath, opts.toBackup())
	if err != nil {
		return nil, err
	}
	out := &PrecheckResult{
		Path:            pc.Path,
		ManifestName:    pc.ManifestName,
		Format:          pc.Format,
		FormatVersion:   pc.FormatVersion,
		AppVersion:      pc.AppVersion,
		ExportedAt:      pc.ExportedAt,
		Platform:        pc.Platform,
		Scope:           pc.Scope,
		Total:           pc.Total,
		WillImport:      pc.WillImport,
		SkipDuplicate:   pc.SkipDuplicate,
		SkipExpired:     pc.SkipExpired,
		Invalid:         pc.Invalid,
		CategoriesNew:   pc.CategoriesNew,
		CategoriesReuse: pc.CategoriesReuse,
		TagsNew:         pc.TagsNew,
		TagsReuse:       pc.TagsReuse,
		Uncompressed:    pc.UncompressedBytes,
		NeedBytes:       pc.NeedBytes,
		AvailableBytes:  pc.AvailableBytes,
		BlobCount:       pc.BlobCount,
		Warnings:        pc.Warnings,
	}
	// 同一个包导入过没有？确认页据此提示"这个包你已经导入过了"。
	// 判据是 manifest 的 sha256 —— 同一份清单就是同一个包，
	// 与文件名、路径无关。
	if rows, err := db.ListImports(a.baseCtx(), 50); err == nil {
		for _, r := range rows {
			if r.ManifestHash != "" && r.ManifestHash == pc.ManifestHash {
				out.AlreadyImported = true
				break
			}
		}
	}
	return out, nil
}

// ImportResult 是导入结果。
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
	ThumbsMade     int      `json:"thumbsMade"`
	Status         string   `json:"status"`
	Errors         []string `json:"errors,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	TookMs         int64    `json:"tookMs"`
	Inserted       int      `json:"inserted"`
}

// ImportBackup 执行导入。必须先 PrecheckBackup，把结果原样传进来。
//
// 为什么要传 pc：§8.1 第 6 步的语义是"用户看到的确认页"与"实际执行的那次"
// 必须是同一份数据。重新预检一次的话，两次之间库可能变了（比如捕获刚写进来
// 一条），用户确认的数字就对不上了。
func (a *App) ImportBackup(pkgPath string, opts ImportOptions, pc *PrecheckResult) (*ImportResult, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, errors.New("pawclip: 请先调用 PrecheckBackup")
	}
	if err := a.checkImportPath(pkgPath); err != nil {
		return nil, err
	}
	bopt := opts.toBackup()

	// 用预检时那份清单再算一次，得到 backup 包需要的 *PrecheckResult。
	// （不能直接把绑定层的结构体塞回去：它丢了未导出的 manifest 字段。）
	fresh, err := backup.Precheck(a.baseCtx(), db, pkgPath, bopt)
	if err != nil {
		return nil, err
	}
	if fresh.ManifestName != pc.ManifestName || fresh.Total != pc.Total {
		return nil, errors.New("pawclip: 包在预检之后发生了变化，请重新预检")
	}

	res, err := backup.Import(a.baseCtx(), db, a.blobStore(), pkgPath, bopt, fresh, a.log)
	if err != nil {
		return nil, err
	}
	return &ImportResult{
		ImportID:       res.ImportID,
		Imported:       res.Imported,
		Skipped:        res.Skipped,
		Failed:         res.Failed,
		Merged:         res.Merged,
		Overwritten:    res.Overwritten,
		CategoriesMade: res.CategoriesMade,
		TagsMade:       res.TagsMade,
		BlobsWritten:   res.BlobsWritten,
		ThumbsMade:     res.ThumbsMade,
		Status:         res.Status,
		Errors:         res.Errors,
		Warnings:       res.Warnings,
		TookMs:         res.TookMs,
		Inserted:       res.Imported - res.Merged - res.Overwritten,
	}, nil
}

// LastImport 返回最近一条可回滚的导入批次。
func (a *App) LastImport() (*store.ImportRow, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	return db.LastImport(a.baseCtx())
}

// RollbackImport 一键回滚某批导入（删掉那批导进来的条目）。
func (a *App) RollbackImport(importID int64) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	return backup.Rollback(a.baseCtx(), db, importID)
}

// checkImportPath 校验用户给的包路径。
//
// 绑定的入参是跨进程契约，可能来自任意地方（拖放、命令行、别的程序）。
// 这里只做"确实是 .clipbak 文件"这一层；真正的安全校验
// （ZIP Slip、解压炸弹、条目名白名单）在 backup.Precheck 里。
func (a *App) checkImportPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("pawclip: 备份包路径为空")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("pawclip: 打不开备份包：%w", err)
	}
	if fi.IsDir() {
		return errors.New("pawclip: 备份包路径是目录")
	}
	if !strings.EqualFold(filepath.Ext(p), ".clipbak") {
		return fmt.Errorf("pawclip: 不是 .clipbak 文件：%s", filepath.Base(p))
	}
	return nil
}

// ── 小工具 ──────────────────────────────────────────────────────

// runtimeGOOS 单独包一层是为了让 backup 的 Platform 字段在测试里可替换。
func runtimeGOOS() string { return platformName() }
