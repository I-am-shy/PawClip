package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// 本文件是**回写与自动粘贴**（docs/DESIGN.md §10 目录结构里的 `writer.go`）。
//
// 它把"用户选中一条历史记录"变成"剪贴板里有内容 / 目标 App 里出现内容"。
//
// 单写 goroutine 的纪律在这里不适用（那管的是数据库），但有一条同样不能违反
// 的纪律：**任何一次我们自己写剪贴板的动作，都必须先 Arm 共享的
// SelfWriteGuard**。漏掉一次，那条内容就会被捕获流水线当成用户复制的新内容
// 重新入库——表现为"用一次历史就多一条一模一样的记录"，而且验收判据 1
// （连续 100 次回贴，历史条目数增加为 0）会直接失败。
//
// 这就是 capture.Capture 把 Guard() 暴露出来的唯一原因：**必须是同一个实例**。
// 两个实例各记各的期望指纹，谁也拦不住谁。
//
// 第二条纪律：**这里的每一句给用户看的话都必须走 w.t(...)**。
// 见 docs/DESIGN.md §14 第 24 条把"错误提示"列为 i18n 易漏位置的那一段。

// PasteMode 是回写方式（§9 的 ui.pasteMode）。
type PasteMode string

const (
	// PasteClipboard 只把内容写进剪贴板，用户自己去按 ⌘V。
	PasteClipboard PasteMode = "clipboard"
	// PasteAuto 写剪贴板 + 模拟一次 ⌘/Ctrl+V（"直贴"）。
	PasteAuto PasteMode = "autoPaste"
)

// Writeback 是回写器。
type Writeback struct {
	backend clipboard.Backend
	guard   *clipboard.SelfWriteGuard
	blobs   *store.BlobStore
	db      *store.DB
	panel   panel.Controller
	log     *slog.Logger

	// ui 返回当前的 ui.* 设置。用函数而不是快照，是为了让设置界面改完
	// 立刻生效——回写是低频动作，每次读一次锁可以忽略。
	ui func() store.UISettings

	// tr 把 msgKey 翻成用户当前的语言。
	//
	// 为什么回写器需要它：这里产出的 Note 是**直接显示给用户的**一句话
	// （"没有辅助功能授权，已降级为只复制"），而 §14 第 24 条把"错误提示"
	// 明确列为 i18n 的易漏位置。原来这些句子是中文字面量写在这里的，
	// 结果是"界面选英文 → 降级提示仍然是中文"。
	//
	// 传函数而不是把 *App 塞进来：回写器不需要知道 App 长什么样，
	// 也就不会被顺手用来读别的状态。
	tr func(msgKey) string

	// seqMu 保护队列。
	//
	// 队列会被两个来源碰：前端绑定调用（Wails 在它自己的线程里执行）
	// 与后端的事件处理器。加锁前它们的交错是数据竞态（`go test -race`
	// 会报，而真机上表现为"偶尔跳过一个"）。
	seqMu sync.Mutex

	// queue 标记"连续粘贴"模式的队列（docs/DESIGN.md §11 P2）。
	queue *pasteQueue
}

// NewWriteback 构造回写器。
//
// guard 必须是 capture.Capture.Guard() 返回的那个实例，不能新建。
// tr 为 nil 时回退到英文目录（测试与"还没初始化完"的场景）。
func NewWriteback(
	backend clipboard.Backend,
	guard *clipboard.SelfWriteGuard,
	blobs *store.BlobStore,
	db *store.DB,
	ctrl panel.Controller,
	ui func() store.UISettings,
	tr func(msgKey) string,
	log *slog.Logger,
) (*Writeback, error) {
	if backend == nil {
		// diag: 构造函数参数校验（编程错误，运行时不可能触发）。
		return nil, errors.New("writer: 需要剪贴板后端")
	}
	if guard == nil {
		// 不 panic 也不静默放行：没有守卫就等于每条回写都会重复入库。
		// diag: 同上。这条其实是**架构纪律**的可执行文档：见本文件开头
		// 关于"必须是同一个 Guard 实例"的那段。
		return nil, errors.New("writer: 需要共享的 SelfWriteGuard（用 Capture.Guard() 取）")
	}
	if blobs == nil || db == nil {
		// diag: 构造函数参数校验，同上。
		return nil, errors.New("writer: 需要 BlobStore 与 DB")
	}
	if log == nil {
		log = slog.Default()
	}
	if tr == nil {
		tr = func(k msgKey) string {
			if s, ok := catalog[LangEn][k]; ok {
				return s
			}
			return string(k)
		}
	}
	return &Writeback{
		backend: backend,
		guard:   guard,
		blobs:   blobs,
		db:      db,
		panel:   ctrl,
		ui:      ui,
		tr:      tr,
		log:     log,
		queue:   newPasteQueue(),
	}, nil
}

// t 是回写器内部的翻译。
func (w *Writeback) t(k msgKey) string { return w.tr(k) }

// tf 是带格式化参数的翻译（文案里的 %d 由它填）。
func (w *Writeback) tf(k msgKey, args ...any) string {
	return fmt.Sprintf(w.tr(k), args...)
}

// PasteResult 是一次回写的结果（前端要拿它决定提示文案）。
type PasteResult struct {
	// Mode 是实际采用的回写方式。请求 autoPaste 但没有辅助功能授权时
	// 会降级为 clipboard——**降级要如实告诉用户**，不能假装贴上了。
	Mode PasteMode `json:"mode"`
	// Pasted 表示"内容确实进了目标 App"（只有 autoPaste 成功才为真）。
	Pasted bool `json:"pasted"`
	// Note 是给用户看的降级原因，空串表示一切正常。
	Note string `json:"note"`
	// Restored 表示原剪贴板已被恢复（§9 的 ui.restoreClipboard）。
	Restored bool `json:"restored"`
}

// PayloadFor 从一条历史记录构造要写回剪贴板的内容。
//
// 图片与 RTF 的字节在 blobs/ 里，这里读出来；找不到文件时**不报错整个失败**，
// 而是跳过那一种表示并把原因记进 note——用户至少还能拿到文本部分。
func (w *Writeback) PayloadFor(ctx context.Context, id int64) (*clipboard.Payload, *store.Item, string, error) {
	it, err := w.db.GetByID(ctx, id)
	if err != nil {
		return nil, nil, "", err
	}
	if it.DeletedAt != nil {
		return nil, nil, "", errors.New(w.tf(msgErrItemTrashed, id))
	}

	p := &clipboard.Payload{}
	var note string

	if it.TextContent != nil && *it.TextContent != "" {
		p.Text = it.TextContent
	}
	if it.HTMLContent != nil && *it.HTMLContent != "" {
		p.HTML = it.HTMLContent
	}
	if it.RTFPath != nil && *it.RTFPath != "" {
		if b, err := w.blobs.Get(*it.RTFPath); err == nil {
			p.RTF = b
		} else {
			note = w.t(msgNoteRTFMissing)
			w.log.Warn("回写时读不到 RTF blob", "id", id, "path", *it.RTFPath, "err", err)
		}
	}
	if it.ImagePath != nil && *it.ImagePath != "" {
		if b, err := w.blobs.Get(*it.ImagePath); err == nil {
			p.PNG = b
		} else {
			note = w.t(msgNoteImageMissing)
			w.log.Warn("回写时读不到图片 blob", "id", id, "path", *it.ImagePath, "err", err)
		}
	}
	if len(it.FilePaths) > 0 {
		p.Files = it.FilePaths
	}

	if p.Empty() {
		return nil, it, "", errors.New(w.tf(msgErrNoContent, id))
	}
	return p, it, note, nil
}

// Paste 把一条历史记录送回剪贴板；autoPaste 为真时再模拟一次粘贴键。
//
// 它由两半组成：**取载荷**（PayloadFor）与**送出去**（deliver）。
// 拆开是为了让"内容转换器的结果"和"去格式贴纯文本"复用同一个投递流程——
// 那两处的差别只在"写什么"，而 Arm 守卫 / 收面板 / 模拟按键 / 恢复剪贴板
// 的顺序纪律是共用的（漏掉任何一步都是可感知的 bug）。
func (w *Writeback) Paste(ctx context.Context, id int64, autoPaste bool) (*PasteResult, error) {
	// PayloadFor 第二个返回值是 *store.Item，Paste 只用"写什么"，所以丢掉。
	payload, _, note, err := w.PayloadFor(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := w.deliver(ctx, payload, autoPaste, note)
	if err != nil {
		return nil, err
	}
	// ⑥ 记一次使用（use_count / last_used_at）。
	//    放在投递之后统一做：任何一个非错误返回都算"用户用了一次"，
	//    分散在各分支里写很容易漏掉其中一条。
	if err := w.markUsed(ctx, id); err != nil {
		w.log.Warn("记录使用次数失败", "id", id, "err", err)
	}
	return res, nil
}

// deliver 是投递的后半段，步骤顺序**不能改**（§7 第 4 条给了同样的顺序）：
//
//	① 读旧剪贴板（要恢复它）
//	② Arm 守卫（在写之前！）
//	③ 写回
//	④ 收起面板、模拟 ⌘V
//	⑤ 等 restoreDelayMs，把旧内容写回去（写之前再 Arm 一次）
//
// note 是调用方已经知道要附加的说明（例如"RTF 丢失，本次只写文本"）。
func (w *Writeback) deliver(ctx context.Context, payload *clipboard.Payload, autoPaste bool, note string) (*PasteResult, error) {
	ui := w.currentUI()
	res := &PasteResult{Mode: PasteClipboard, Note: note}

	// 希望自动粘贴，但没授权 / 平台不支持 → 如实降级（§13 风险表）。
	wantAuto := autoPaste || ui.PasteMode == string(PasteAuto)
	autoOK := w.panel != nil && w.panel.CanAutoPaste()
	if wantAuto && !autoOK {
		res.Note = joinNote(res.Note, w.t(msgPasteDegraded))
		wantAuto = false
	}

	// ① 先留一份旧剪贴板。只在需要恢复、且真的要自动粘贴时才读——
	//    读一次剪贴板在 Windows 上可能要重试，不该为"只复制"付这个代价。
	var (
		old    *clipboard.Payload
		oldErr error
	)
	if wantAuto && ui.RestoreClipboard {
		old, oldErr = w.snapshotClipboard()
		if oldErr != nil {
			// 恢复失败不影响主流程，只是不恢复而已。
			w.log.Warn("读取原剪贴板失败，本次不恢复", "err", oldErr)
			res.Note = joinNote(res.Note, w.t(msgNoteSnapshotFailed))
		}
	}

	// ② Arm 之后再写。顺序反了的话，捕获流水线可能在我们 Arm 之前
	//    就已经读到新内容了（macOS 是 0.2s 轮询，窗口很小但真实存在）。
	w.guard.Arm(payload.Fingerprint())

	// ③ 写回。
	if err := w.backend.Write(payload); err != nil {
		return nil, msgf(msgErrClipboardWr, err, err)
	}

	if !wantAuto {
		res.Mode = PasteClipboard
		return res, nil
	}

	// ④ 收起面板再模拟按键：面板若还占着键盘，⌘V 会打到我们自己的搜索框里。
	if w.panel != nil {
		w.panel.Hide()
	}
	// 给 AppKit / Win32 一点时间把前台切回去。太短会让按键落到错误的窗口。
	time.Sleep(60 * time.Millisecond)

	if err := w.panel.AutoPaste(); err != nil {
		// 已经把内容放进剪贴板了，所以这不是失败，是降级。
		w.log.Warn("模拟粘贴失败，内容已在剪贴板里", "err", err)
		res.Note = joinNote(res.Note, w.t(msgNoteAutoPasteFail))
		res.Mode = PasteClipboard
		return res, nil
	}
	res.Pasted = true
	res.Mode = PasteAuto

	// ⑤ 恢复原剪贴板。
	//
	// 为什么要延迟：部分 App 是异步读剪贴板的（§7 第 5 条），
	// 恢复太快会让它读到我们的旧内容，表现为"粘贴出来的是上一次的东西"。
	if old != nil && ui.RestoreDelayMs > 0 {
		time.Sleep(time.Duration(ui.RestoreDelayMs) * time.Millisecond)
		// 恢复同样是我们自己的写入，同样要 Arm。
		w.guard.Arm(old.Fingerprint())
		if err := w.backend.Write(old); err != nil {
			w.log.Warn("恢复原剪贴板失败", "err", err)
			res.Note = joinNote(res.Note, w.t(msgNoteRestoreFailed))
		} else {
			res.Restored = true
		}
	}

	return res, nil
}

// snapshotClipboard 把当前剪贴板读成一个可写回的 Payload。
func (w *Writeback) snapshotClipboard() (*clipboard.Payload, error) {
	raw, err := w.backend.Read()
	if err != nil {
		if errors.Is(err, clipboard.ErrEmpty) {
			return nil, nil // 空剪贴板没什么可恢复的，不算错误
		}
		return nil, err
	}
	// Content 已经把所有表示拍平成 PNG 字节（Raw.Content 内部读的是
	// raw.Image.PNG），所以这里不需要再解一层 Image。
	// 用 Content 而不是手写字段还有一个硬性理由：Arm 的指纹必须与
	// 采集侧算出来的**逐字节一致**，两边都从 Content 派生才不会漂移。
	c := raw.Content()
	p := &clipboard.Payload{
		Text:  c.Text,
		HTML:  c.HTML,
		RTF:   c.RTF,
		PNG:   c.PNG,
		Files: c.Files,
	}
	if p.Empty() {
		return nil, nil
	}
	return p, nil
}

// CopyOnly 只把内容写进剪贴板（不模拟粘贴）。给"复制"按钮用。
func (w *Writeback) CopyOnly(ctx context.Context, id int64) (*PasteResult, error) {
	return w.Paste(ctx, id, false)
}

// PasteText 把一段任意文本写进剪贴板（只复制，不模拟粘贴）。
//
// 同样要 Arm：转换器产出的内容也可能被用户再复制一次，
// 但"我们刚写进去的"不该立刻入库。
func (w *Writeback) PasteText(s string) error {
	p := &clipboard.Payload{Text: &s}
	w.guard.Arm(p.Fingerprint())
	return w.backend.Write(p)
}

// PasteCustomText 把一段任意文本按**完整回写流程**送出去（内容转换器的结果）。
//
// 与 PasteText 的区别：这里会走 §7 那套顺序（收面板 → 模拟 ⌘V → 恢复原
// 剪贴板），所以"点一下转换结果就直接贴出来"是可行的。
//
// **不记使用次数**：转换结果是同一条内容的另一种形态，用户取用的仍然是
// 那一条；把 use_count 记在它头上会让"最常用"的排序被转换动作污染。
func (w *Writeback) PasteCustomText(ctx context.Context, text string, autoPaste bool) (*PasteResult, error) {
	if text == "" {
		return nil, errors.New(w.t(msgErrEmptyText))
	}
	p := &clipboard.Payload{Text: &text}
	return w.deliver(ctx, p, autoPaste, "")
}

// PastePlain 只把条目的**纯文本**写回（docs/DESIGN.md §11 P2 的"去格式贴纯文本"）。
//
// 为什么不复用 Paste：Paste 的载荷里带着 HTML / RTF，落到支持富文本的
// App 里会把字体、颜色、超链接一起带过去——这正是用户点"去格式"要避免的。
//
// 图片 / 文件类条目没有纯文本兜底，直接如实报错，而不是"贴一张图给你"。
func (w *Writeback) PastePlain(ctx context.Context, id int64, autoPaste bool) (*PasteResult, error) {
	it, err := w.db.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if it.DeletedAt != nil {
		return nil, errors.New(w.tf(msgErrItemTrashed, id))
	}
	if it.TextContent == nil || strings.TrimSpace(*it.TextContent) == "" {
		return nil, errors.New(w.t(msgErrNoPlainText))
	}
	p := &clipboard.Payload{Text: it.TextContent}
	res, err := w.deliver(ctx, p, autoPaste, "")
	if err != nil {
		return nil, err
	}
	if err := w.markUsed(ctx, id); err != nil {
		w.log.Warn("记录使用次数失败", "id", id, "err", err)
	}
	return res, nil
}

// markUsed 记一次取用（use_count + last_used_at）。
func (w *Writeback) markUsed(ctx context.Context, id int64) error {
	return w.db.MarkUsed(ctx, []int64{id})
}

func (w *Writeback) currentUI() store.UISettings {
	if w.ui == nil {
		return store.DefaultSettings().UI
	}
	return w.ui()
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}

// ── 连续粘贴模式（docs/DESIGN.md §11 的 P2）──────────────────────────────
//
// 语义：用户多选若干条，点"连续粘贴"，然后回到目标 App 里逐个按
// ⌘V（或按一次热键）消费下一条。队列放在这里而不是前端，
// 是因为"下一个是什么"必须和剪贴板的真实内容一致——
// 前端持队列会在面板销毁重建后丢失。

type pasteQueue struct {
	ids   []int64
	pos   int
	armed bool
}

func newPasteQueue() *pasteQueue { return &pasteQueue{} }

// Set 用一批 id 重排队列。
func (q *pasteQueue) Set(ids []int64) {
	q.ids = append(q.ids[:0], ids...)
	q.pos = 0
	q.armed = len(ids) > 0
}

// Next 取出下一个 id；用完了返回 false。
func (q *pasteQueue) Next() (int64, bool) {
	if !q.armed || q.pos >= len(q.ids) {
		q.armed = false
		return 0, false
	}
	id := q.ids[q.pos]
	q.pos++
	return id, true
}

// Remaining 返回还剩几条。
func (q *pasteQueue) Remaining() int {
	if !q.armed {
		return 0
	}
	return len(q.ids) - q.pos
}

// Clear 清空队列。
func (q *pasteQueue) Clear() {
	q.ids = q.ids[:0]
	q.pos = 0
	q.armed = false
}

// StartSequence 开始连续粘贴：排队并立刻投出第一条。
//
// 返回值是"投出第一条之后还剩几条"，前端拿它显示提示。
func (w *Writeback) StartSequence(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New(w.t(msgErrSeqEmpty))
	}
	w.seqMu.Lock()
	w.queue.Set(ids)
	w.seqMu.Unlock()

	if _, err := w.nextInSequence(ctx); err != nil {
		return 0, err
	}

	w.seqMu.Lock()
	defer w.seqMu.Unlock()
	return w.queue.Remaining(), nil
}

// NextInSequence 把队列里的下一条送进剪贴板并自动粘贴。
func (w *Writeback) NextInSequence(ctx context.Context) (*PasteResult, error) {
	return w.nextInSequence(ctx)
}

// nextInSequence 是加锁版本的内部实现。
//
// 为什么 StartSequence 不能直接调 NextInSequence：那会在持有 seqMu 的
// 情况下再锁一次（seqMu 是普通 Mutex，不是可重入的）→ 死锁。
// 所以"取一个 id"与"投出去"必须分成两步，锁只护住取 id 那一下。
func (w *Writeback) nextInSequence(ctx context.Context) (*PasteResult, error) {
	w.seqMu.Lock()
	id, ok := w.queue.Next()
	w.seqMu.Unlock()
	if !ok {
		return nil, errors.New(w.t(msgErrSeqDone))
	}
	return w.Paste(ctx, id, true)
}

// SequenceRemaining 返回队列剩余条数。
func (w *Writeback) SequenceRemaining() int {
	w.seqMu.Lock()
	defer w.seqMu.Unlock()
	return w.queue.Remaining()
}

// SequenceTotal 返回队列总条数（UI 显示 "3 / 12" 用）。
func (w *Writeback) SequenceTotal() int {
	w.seqMu.Lock()
	defer w.seqMu.Unlock()
	return len(w.queue.ids)
}

// ClearSequence 清空队列。
func (w *Writeback) ClearSequence() {
	w.seqMu.Lock()
	defer w.seqMu.Unlock()
	w.queue.Clear()
}

var _ = context.Background
