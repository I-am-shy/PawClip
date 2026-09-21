package main

// 草稿本的绑定层（docs/DESIGN.md §4.4）。
//
// 单独一个文件而不是塞进 bindings.go：那边是"剪贴板历史"的绑定面
// （列表 / 回写 / 批量操作），草稿本完全是另一件事——它有自己的
// 数据表、自己的保存节奏、自己的失败模式。混在一起之后，
// "实时保存每 500ms 调一次的那条路径上有什么"会变得很难一眼看出来。
//
// ⚠️ 所有导出方法都会变成前端绑定（Wails 反射 App 的导出方法），
// 所以这里的每一个方法都是**跨进程契约的一部分**：改签名要同步改
// frontend/src/api.ts 的 Bindings 表（那里是手写的，见该文件头部注释）。

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zego/pawclip/store"
)

// draftTitleMaxRunes 是标题的长度上限。
//
// 不是安全限制（标题进的是 TEXT 列，多长都存得下），而是**界面限制**：
// 目录宽 180 逻辑点，标题会按 CSS 截断显示；允许一个 5000 字的标题
// 只会让人误以为"标题栏坏了"。超出部分截断而不是拒绝——用户的输入
// 不该因为一个长度上限而整体丢掉。
const draftTitleMaxRunes = 200

// DraftList 是草稿目录的一次性快照。
//
// 几个设置值（归档保留期、图片上限、防抖窗口）搭这个结构一起带出去，
// 而不是让前端为它们各发一次 IPC：它们**只在草稿本页面用**，
// 而且都是"打开页面时读一次就够"的静态值。
type DraftList struct {
	Items    []store.DraftRow `json:"items"`
	Archived []store.DraftRow `json:"archived"`
	// ArchiveTtlSec 是归档保留期（秒），界面用它说明"再过 X 天彻底删除"。
	ArchiveTtlSec int64 `json:"archiveTtlSec"`
	// LastDraftID 是"上次打开的草稿"（ui.lastDraftId）：重新进入草稿本时
	// 回到它，而不是固定回第一条。指向的草稿可能已删，前端必须校验存在。
	LastDraftID int64 `json:"lastDraftId"`
	// TocCollapsed 是目录的收起状态（ui.draftTocCollapsed）。
	TocCollapsed bool `json:"tocCollapsed"`
	// MaxImageBytes 是单张贴图上限，ImageMaxMB 是它的人读形式。
	//
	// 两个都给：前端要拿数字做预检，要拿文本拼错误提示。
	// 让前端自己换算就会与后端算的口径不一致（§14 第 24 条那类
	// "两处各算一遍"的老问题）。
	MaxImageBytes int64  `json:"maxImageBytes"`
	MaxImageLabel string `json:"maxImageLabel"`
	// AutoSaveDebounceMs 是实时保存的防抖窗口。
	AutoSaveDebounceMs int `json:"autoSaveDebounceMs"`
}

// DraftSaveResult 是一次实时保存的回执。
type DraftSaveResult struct {
	// UpdatedAt 是落库时间（Unix 秒），界面用它显示"已保存 · HH:MM"。
	UpdatedAt int64 `json:"updatedAt"`
	// Chars 是这次写进去的字符数（字数统计用，避免前端再数一遍）。
	Chars int64 `json:"chars"`
}

// DraftImage 是一次贴图导入的结果。
type DraftImage struct {
	// URL 是要插进正文的图片地址（形如 blob/9f/2a/<sha>.png）。
	//
	// 它是**相对 URL**，与列表缩略图用的是同一个前缀（详见
	// store.DraftBlobPrefix 的注释）：由 Wails 的 AssetServer 直接把
	// 字节递给 WebView，不走 IPC。
	URL string `json:"url"`
	// Bytes 落盘后的字节数。
	Bytes int64 `json:"bytes"`
	// Width/Height 是像素尺寸，供前端预留占位高度；解不出时为 0。
	//
	// 只认 PNG（store.DecodePNGSize）——草稿贴图绝大多数是截图，
	// 而截图在两端都是 PNG。别的格式返回 0，前端按"未知高度"处理，
	// 不为此再引一个图像解析器。
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Drafts 返回目录（含归档区）与草稿本相关的几个设置值。
//
// 一次调用给全，而不是拆成三个方法：目录页打开时要同时用到这三样，
// 拆开就是三次 IPC 往返，而它们之间没有先后依赖。
func (a *App) Drafts() (*DraftList, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := a.baseCtx()

	items, err := db.ListDrafts(ctx)
	if err != nil {
		return nil, err
	}
	archived, err := db.ListArchivedDrafts(ctx)
	if err != nil {
		return nil, err
	}

	out := &DraftList{Items: items, Archived: archived}
	if s := a.Settings(); s != nil {
		out.ArchiveTtlSec = s.Draft.ArchiveTTLSec
		out.LastDraftID = s.UI.LastDraftID
		out.TocCollapsed = s.UI.DraftTOCCollapsed
		out.MaxImageBytes = s.Draft.ImageMaxBytes
		out.MaxImageLabel = humanBytes(s.Draft.ImageMaxBytes)
		out.AutoSaveDebounceMs = s.Draft.AutoSaveDebounceMs
	}
	if out.ArchiveTtlSec <= 0 {
		// 读不到时给默认值：前端要拿它算"再过 X 天删除"的提示，
		// 0 会让那句话显示成"0 天后清理"，像是一个坏掉的天数。
		out.ArchiveTtlSec = store.DefaultSettings().Draft.ArchiveTTLSec
	}
	if out.AutoSaveDebounceMs <= 0 {
		// 设置读不到时给一个能用的值：0 会让前端每次都立即落盘，
		// 逐字输入就变成逐字写库（写句柄是单连接，捕获线程在排同一条队）。
		out.AutoSaveDebounceMs = store.DefaultSettings().Draft.AutoSaveDebounceMs
	}
	return out, nil
}

// Draft 取一条草稿（含完整正文）。
//
// 不存在时返回**本地化过的** ErrNotFound，而不是 (nil, nil)：
// 前端只可能因为"这条被删了"或"id 记错了"才拿到这个结果，
// 两种情况都该看到一句人话，而不是自己判空。
func (a *App) Draft(id int64) (*store.Draft, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	d, err := db.GetDraft(a.baseCtx(), id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, a.localizeErr(store.ErrNotFound)
	}
	return d, nil
}

// CreateDraft 新建一条草稿，返回它的完整内容（正文是空串）。
//
// 默认名在这里渲染，**并且只在这一刻渲染**（见 i18n.go 里
// msgDraftDefaultName 的注释）：名字写进库之后就是内容，
// 用户切语言不会让它跟着变——那样看起来像是草稿被改过。
func (a *App) CreateDraft() (*store.Draft, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := a.baseCtx()

	seq, err := db.NextDraftSeq(ctx)
	if err != nil {
		return nil, err
	}
	id, err := db.CreateDraft(ctx, a.render(msgDraftDefaultName, seq), seq)
	if err != nil {
		return nil, err
	}
	return a.Draft(id)
}

// SaveDraft 写正文（实时保存的唯一入口），返回落库时间。
//
// 它**不碰标题**（标题走 RenameDraft）：两条写路径分开，才不会出现
// "自动保存顺手把用户正在改的标题覆盖回去"。这也是为什么签名里
// 没有 title 参数，而不是"传空串表示不改"——那种约定迟早会被违反。
func (a *App) SaveDraft(id int64, md string) (*DraftSaveResult, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return nil, err
	}
	ts, err := db.SaveDraftMD(a.baseCtx(), id, md)
	if err != nil {
		return nil, a.localizeErr(err)
	}
	return &DraftSaveResult{UpdatedAt: ts, Chars: int64(utf8.RuneCountInString(md))}, nil
}

// RenameDraft 改标题。
//
// 两处收口：**去掉首尾空白**（几乎总是误输入），以及**截断到 200 字**
// （界面限制，见 draftTitleMaxRunes）。空标题是合法的——用户清空了输入框，
// 界面上显示成"未命名"，而不是替用户编一个名字再让他删。
func (a *App) RenameDraft(id int64, title string) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return a.localizeErr(db.RenameDraft(a.baseCtx(), id, clampTitle(title)))
}

// ArchiveDraft 软删除（进归档区，保留期内可恢复）。
func (a *App) ArchiveDraft(id int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return a.localizeErr(db.ArchiveDraft(a.baseCtx(), id))
}

// RestoreDraft 从归档区恢复。
func (a *App) RestoreDraft(id int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return a.localizeErr(db.RestoreDraft(a.baseCtx(), id))
}

// PurgeDraft 彻底删除一条草稿（归档区里的手动清理）。
//
// 不删 blob 文件：blobs/ 是内容寻址的，同一张图可能同时被剪贴板条目
// 引用，按条删文件会让另一条变成悬空引用。文件回收交给 GC 的孤儿扫描。
func (a *App) PurgeDraft(id int64) (int64, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return 0, err
	}
	return db.PurgeDraft(a.baseCtx(), id)
}

// ReorderDrafts 按给定顺序重写目录顺序（拖拽排序的落库入口）。
func (a *App) ReorderDrafts(ids []int64) error {
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	return db.ReorderDrafts(a.baseCtx(), ids)
}

// ImportDraftImage 把一张图片落成 blob，返回可以插进正文的 URL。
//
// 三条校验，每一条都对应一种真实的坏输入：
//
//  1. **大小上限**（draft.imageMaxBytes）。10 MB 的图 base64 之后是 13 MB，
//     走 IPC 会把 WebView 卡住几秒；超限要在**解码之前**就拒绝，
//     否则我们已经为它分配了内存。
//  2. **必须是图片 mime。** 只接受能确定扩展名的几种：blobserver 按
//     扩展名回 Content-Type，认不出的会以 application/octet-stream 发出去，
//     浏览器不会把它当图渲染——那就是"贴上去是一张破图"。
//  3. **扩展名必须是字母数字**（store.ShardPath 会再校验一次）。
//
// base64 允许带 `data:image/png;base64,` 前缀：前端用 FileReader
// 读剪贴板/文件得到的就是那个形式，收口在这里剥比让每个调用点都记得剥更可靠。
func (a *App) ImportDraftImage(mime, dataBase64 string) (*DraftImage, error) {
	// 只借 a.ready() 完成初始化（库句柄与 blob 存储都建起来），
	// 这条路径不需要 db：图片落的是 blobs/ 目录，不写数据库行。
	if _, _, _, err := a.ready(); err != nil {
		return nil, err
	}
	a.initMu.Lock()
	blobs := a.blobs
	a.initMu.Unlock()
	if blobs == nil {
		return nil, a.localizeErr(store.ErrNotFound)
	}

	maxBytes := int64(0)
	if s := a.Settings(); s != nil {
		maxBytes = s.Draft.ImageMaxBytes
	}
	if maxBytes <= 0 {
		maxBytes = store.DefaultSettings().Draft.ImageMaxBytes
	}

	// 预估解码后的大小：base64 每 4 个字符表示 3 字节。先估后解，
	// 免得为一个必然被拒的输入去分配几十 MB。
	payload := stripDataURLPrefix(dataBase64)
	if est := int64(len(payload)) / 4 * 3; est > maxBytes {
		return nil, a.localizeErr(msgf(msgErrDraftImageTooBig, nil, humanBytes(maxBytes)))
	}

	data, decErr := base64.StdEncoding.DecodeString(payload)
	if decErr != nil {
		// 退一步试试 URL 安全字母表：有些来源（URL 参数、某些截屏工具）
		// 用它编码 `+` 与 `/`。两种都试过才报错，是"少一个失败面"的做法。
		data, decErr = base64.RawURLEncoding.DecodeString(payload)
		if decErr != nil {
			// 走 msgErrDraftImageBroken 而不是把它标成 diag：这条路用户
			// 真的会走到——贴了一段文本或 HTML 时（比如从浏览器复制后
			// 触发了"贴图"），解码就一定失败，而那时他需要看到的是
			// "你贴的不是图片"，不是一句 base64 诊断。
			// decErr 留在 cause 里，不丢诊断信息。
			return nil, a.localizeErr(msgf(msgErrDraftImageBroken, decErr))
		}
	}
	if int64(len(data)) > maxBytes {
		return nil, a.localizeErr(msgf(msgErrDraftImageTooBig, nil, humanBytes(maxBytes)))
	}

	ext, ok := draftImageExt(mime, data)
	if !ok {
		return nil, a.localizeErr(msgf(msgErrDraftImageType, nil))
	}
	rel, _, err := blobs.Put(data, ext)
	if err != nil {
		return nil, err
	}

	img := &DraftImage{URL: store.DraftBlobPrefix + rel, Bytes: int64(len(data))}
	if w, h, err := store.DecodePNGSize(data); err == nil {
		img.Width, img.Height = w, h
	}
	return img, nil
}

// SetLastView 记住"当前停在哪一页"。
//
// 值先过一次 store.NormalizeLastView：写进库的必须是一个已知的视图名，
// 否则下次启动会照着它去找一个不存在的页面。
//
// 与内存里的值相同时**直接返回**，不写库：视图切换是用户行为，
// 但"点回当前页"这种操作不该产生一次写事务（写句柄是单连接，
// 捕获线程在排同一条队）。
func (a *App) SetLastView(view string) error {
	v := store.NormalizeLastView(view)
	if s := a.Settings(); s != nil && s.UI.LastView == v {
		return nil
	}
	if err := a.SetSetting(store.KeyUILastView, jsonLiteral(v)); err != nil {
		return a.localizeErr(msgf(msgErrLastView, err, err))
	}
	return nil
}

// SetLastDraft 记住"上次打开的草稿"（ui.lastDraftId）。
//
// 重新进入草稿本时回到它，而不是固定回目录第一条——用户有多条草稿时，
// 固定回第一条看起来就像"我刚写的东西没了"。
//
// 与内存里的值相同时直接返回，不写库（理由同 SetLastView）。
// 不校验 id 是否存在：这条调用的语义是"用户正在看它"，存在性由
// 下一次读取时（前端在目录里找不到就回退第一条）兜底。
func (a *App) SetLastDraft(id int64) error {
	if s := a.Settings(); s != nil && s.UI.LastDraftID == id {
		return nil
	}
	return a.SetSetting(store.KeyUILastDraftId, strconv.FormatInt(id, 10))
}

// ── 小工具 ──────────────────────────────────────────────────────

// clampTitle 去掉首尾空白并把长度截到 draftTitleMaxRunes 个字符（不是字节）。
func clampTitle(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > draftTitleMaxRunes {
		return string(r[:draftTitleMaxRunes])
	}
	return s
}

// stripDataURLPrefix 剥掉 `data:image/png;base64,` 这样的前缀。
//
// 只认 `;base64,` 这一个分界：别的 data URL（text/plain 之类）不是图片，
// 让它们带着前缀进解码器会解不出来并得到一个 base64 错误，
// 而真实原因其实是"这不是图片"——那正是 msgErrDraftImageType 该负责的话。
func stripDataURLPrefix(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "data:") {
		return s
	}
	if i := strings.Index(s, ";base64,"); i >= 0 {
		return s[i+len(";base64,"):]
	}
	return s
}

// draftImageExt 由 mime 与内容定出 blob 的扩展名。
//
// 为什么要看**内容**：剪贴板里来的图片经常没有可靠的 mime
// （Windows 上是自造的、macOS 上可能是 public.tiff 而实际是 PNG）。
// PNG 的魔数是最可靠的一路——它也正是下游（缩略图、blobserver）认的那一种。
func draftImageExt(mime string, data []byte) (string, bool) {
	if len(data) >= 8 && string(data[1:4]) == "PNG" {
		return "png", true
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "jpg", true
	}
	if len(data) >= 6 && (string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a") {
		return "gif", true
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "webp", true
	}
	// 魔数认不出时退回 mime。这一步是给 bmp / tiff 这类没有统一魔数的
	// 格式留的出口，也是**唯一**靠声明而不是内容判断的分支。
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/bmp", "image/x-ms-bmp":
		return "bmp", true
	case "image/tiff":
		return "tiff", true
	}
	return "", false
}

// humanBytes 把字节数变成"10 MB"这样的短标签（给错误提示用）。
//
// 与前端 format.ts 的 formatBytes 同口径（1024 进制、MB 以上一位小数）：
// 两处口径不一致时，用户会在同一个上限上看到两个数字（"上限 10 MB"
// 报错却说"超过 9.5 MB"）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	digits := 0
	if i >= 1 {
		digits = 1
	}
	return fmt.Sprintf("%.*f %s", digits, v, units[i])
}
