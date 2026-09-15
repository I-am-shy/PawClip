package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// 本文件是**回写与自动粘贴**（DESIGN §10 目录结构里的 `writer.go`）。
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

	// seq 标记"连续粘贴"模式的队列（DESIGN §11 P2）。
	queue *pasteQueue
}

// NewWriteback 构造回写器。
//
// guard 必须是 capture.Capture.Guard() 返回的那个实例，不能新建。
func NewWriteback(
	backend clipboard.Backend,
	guard *clipboard.SelfWriteGuard,
	blobs *store.BlobStore,
	db *store.DB,
	ctrl panel.Controller,
	ui func() store.UISettings,
	log *slog.Logger,
) (*Writeback, error) {
	if backend == nil {
		return nil, errors.New("writer: 需要剪贴板后端")
	}
	if guard == nil {
		// 不 panic 也不静默放行：没有守卫就等于每条回写都会重复入库。
		return nil, errors.New("writer: 需要共享的 SelfWriteGuard（用 Capture.Guard() 取）")
	}
	if blobs == nil || db == nil {
		return nil, errors.New("writer: 需要 BlobStore 与 DB")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Writeback{
		backend: backend,
		guard:   guard,
		blobs:   blobs,
		db:      db,
		panel:   ctrl,
		ui:      ui,
		log:     log,
		queue:   newPasteQueue(),
	}, nil
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
		return nil, nil, "", fmt.Errorf("writer: 条目 %d 在回收站里，先恢复再使用", id)
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
			note = "RTF 内容已丢失，本次只写文本"
			w.log.Warn("回写时读不到 RTF blob", "id", id, "path", *it.RTFPath, "err", err)
		}
	}
	if it.ImagePath != nil && *it.ImagePath != "" {
		if b, err := w.blobs.Get(*it.ImagePath); err == nil {
			p.PNG = b
		} else {
			note = "图片文件已丢失，本次不带图片"
			w.log.Warn("回写时读不到图片 blob", "id", id, "path", *it.ImagePath, "err", err)
		}
	}
	if len(it.FilePaths) > 0 {
		p.Files = it.FilePaths
	}

	if p.Empty() {
		return nil, it, "", fmt.Errorf("writer: 条目 %d 没有任何可写回的内容", id)
	}
	return p, it, note, nil
}

// Paste 把一条历史记录送回剪贴板；autoPaste 为真时再模拟一次粘贴键。
//
// 步骤顺序**不能改**（§7 第 4 条给了同样的顺序）：
//
//	① 读旧剪贴板（要恢复它）
//	② Arm 守卫（在写之前！）
//	③ 写回
//	④ 收起面板、模拟 ⌘V
//	⑤ 等 restoreDelayMs，把旧内容写回去（写之前再 Arm 一次）
//	⑥ 记一次使用（use_count / last_used_at）
func (w *Writeback) Paste(ctx context.Context, id int64, autoPaste bool) (*PasteResult, error) {
	ui := w.currentUI()

	// PayloadFor 第二个返回值是 *store.Item，Paste 只用"写什么"，所以丢掉。
	payload, _, note, err := w.PayloadFor(ctx, id)
	if err != nil {
		return nil, err
	}

	res := &PasteResult{Mode: PasteClipboard, Note: note}

	// 希望自动粘贴，但没授权 / 平台不支持 → 如实降级（§13 风险表）。
	wantAuto := autoPaste || ui.PasteMode == string(PasteAuto)
	autoOK := w.panel != nil && w.panel.CanAutoPaste()
	if wantAuto && !autoOK {
		res.Note = joinNote(res.Note, "没有「辅助功能」授权，已降级为只复制到剪贴板")
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
			res.Note = joinNote(res.Note, "原剪贴板未能读取，粘贴后不恢复")
		}
	}

	// ② Arm 之后再写。顺序反了的话，捕获流水线可能在我们 Arm 之前
	//    就已经读到新内容了（macOS 是 0.2s 轮询，窗口很小但真实存在）。
	w.guard.Arm(payload.Fingerprint())

	// ③ 写回。
	if err := w.backend.Write(payload); err != nil {
		return nil, fmt.Errorf("writer: 写剪贴板失败：%w", err)
	}

	if !wantAuto {
		if err := w.markUsed(ctx, id); err != nil {
			w.log.Warn("记录使用次数失败", "id", id, "err", err)
		}
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
		res.Note = joinNote(res.Note, "模拟粘贴失败，内容已复制到剪贴板，请手动粘贴")
		res.Mode = PasteClipboard
		if err := w.markUsed(ctx, id); err != nil {
			w.log.Warn("记录使用次数失败", "id", id, "err", err)
		}
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
			res.Note = joinNote(res.Note, "原剪贴板恢复失败")
		} else {
			res.Restored = true
		}
	}

	// ⑥ 记一次使用。
	if err := w.markUsed(ctx, id); err != nil {
		w.log.Warn("记录使用次数失败", "id", id, "err", err)
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

// PasteText 把一段任意文本写进剪贴板（内容转换器 / 片段库用）。
//
// 同样要 Arm：转换器产出的内容也可能被用户再复制一次，
// 但"我们刚写进去的"不该立刻入库。
func (w *Writeback) PasteText(s string) error {
	p := &clipboard.Payload{Text: &s}
	w.guard.Arm(p.Fingerprint())
	return w.backend.Write(p)
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

// ── 连续粘贴模式（DESIGN §11 的 P2）──────────────────────────────
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

// StartSequence 开始连续粘贴。
func (w *Writeback) StartSequence(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("writer: 连续粘贴需要至少一条内容")
	}
	w.queue.Set(ids)
	n, err := w.NextInSequence(ctx)
	if err != nil {
		return 0, err
	}
	_ = n
	return w.queue.Remaining(), nil
}

// NextInSequence 把队列里的下一条送进剪贴板并自动粘贴。
func (w *Writeback) NextInSequence(ctx context.Context) (*PasteResult, error) {
	id, ok := w.queue.Next()
	if !ok {
		return nil, errors.New("writer: 连续粘贴队列已空")
	}
	return w.Paste(ctx, id, true)
}

// SequenceRemaining 返回队列剩余条数。
func (w *Writeback) SequenceRemaining() int { return w.queue.Remaining() }

// SequenceTotal 返回队列总条数（UI 显示 "3 / 12" 用）。
func (w *Writeback) SequenceTotal() int { return len(w.queue.ids) }

// ClearSequence 清空队列。
func (w *Writeback) ClearSequence() { w.queue.Clear() }

var _ = context.Background
