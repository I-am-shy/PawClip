package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 本文件实现 docs/DESIGN.md §2 的"单写 goroutine + 合批提交"。
//
// 为什么要单写 goroutine：SQLite 的写操作必须串行化，并发写会撞 SQLITE_BUSY。
// 用单个 goroutine 消费 channel 顺便实现了 §14 第 4 条的合批提交——
// 攒 50 ms 或 20 条一起 commit，显著减少 fsync 次数。
//
// 除了本类型，只有 store.DB.SetRaw（设置写入，低频）会碰单写句柄；
// 单写句柄是 SetMaxOpenConns(1) 的，所以那点竞争会在连接池上排队，
// 加上 busy_timeout=5000 不会出现 SQLITE_BUSY。

// 错误。
var (
	// ErrQueueFull 表示写队列在超时内一直满，本次写入被拒。
	ErrQueueFull = errors.New("store: write queue is full")
	// ErrWriterClosed 表示写入器已关闭。
	ErrWriterClosed = errors.New("store: writer is closed")
)

// BlobRole 指明一个 blob 属于条目的哪个字段。
//
// 只有 image 与 rtf：docs/DESIGN.md §4.1 的 items 表给这两个字段留了路径列。
// HTML 在 M1 一律内联在 html_content（超长则截断），原因见收尾报告里的
// "文档缺口"一节（DDL 没有 html_path）。
type BlobRole string

const (
	BlobRoleImage BlobRole = "image"
	BlobRoleRTF   BlobRole = "rtf"
)

// BlobWrite 是一份待落盘的二进制内容。
type BlobWrite struct {
	Role BlobRole
	// Data 是内容本体。写入按 sha256 内容寻址，同一内容重复写是幂等的。
	Data []byte
	// Ext 是扩展名（不含点）：png / rtf。
	Ext string
	// Thumb 为真时额外生成长边 ≤ ThumbMaxEdge 的缩略图并回填 thumb_path。
	Thumb bool
}

// WriteRequest 是一次落库请求。
type WriteRequest struct {
	Item Item
	// Blobs 会先原子落盘，再把相对路径回填到 Item 的对应列，最后才插行——
	// 顺序反了会在崩溃后留下指向不存在文件的悬空行。
	//
	// Item.ByteSize 只需带非 blob 部分，blob 的字节数由写入器累加。
	Blobs []BlobWrite
	// Result 非空时，写入器在提交后回传结果（缓冲 1，不会阻塞写入器）。
	Result chan WriteResult
}

// WriteResult 是一次写入的结果。
type WriteResult struct {
	ID          int64
	UseCount    int64
	FirstSeenAt int64
	// BlobPaths 是实际落盘的各角色相对路径。
	BlobPaths map[BlobRole]string
	Err       error
}

// WriterConfig 是写入器的调参。
type WriterConfig struct {
	// FlushInterval 是合批窗口，默认 50ms。
	FlushInterval time.Duration
	// BatchMax 是单批最大条数，默认 20。
	BatchMax int
	// QueueSize 是队列容量，默认 512。
	QueueSize int
	// EnqueueTimeout 是队列满时的等待上限，默认 2s。
	EnqueueTimeout time.Duration
	// CheckpointEvery 是每多少次写入做一次 wal_checkpoint(TRUNCATE)，默认 1000，<=0 关闭。
	CheckpointEvery int
	// CheckpointInterval 是小时级兜底检查点间隔，默认 1h，<0 关闭。
	CheckpointInterval time.Duration
}

func (c WriterConfig) withDefaults() WriterConfig {
	out := c
	if out.FlushInterval <= 0 {
		out.FlushInterval = 50 * time.Millisecond
	}
	if out.BatchMax <= 0 {
		out.BatchMax = 20
	}
	if out.QueueSize <= 0 {
		out.QueueSize = 512
	}
	if out.EnqueueTimeout <= 0 {
		out.EnqueueTimeout = 2 * time.Second
	}
	if out.CheckpointInterval == 0 {
		out.CheckpointInterval = time.Hour
	}
	return out
}

// WriterStats 是写入器的累计计数（仅供诊断与验收取证）。
type WriterStats struct {
	Enqueued    int64
	Rejected    int64
	Items       int64
	Batches     int64
	Errors      int64
	Checkpoints int64
	LastFlushMs int64
}

// Writer 是唯一的写入口。
type Writer struct {
	db    *DB
	blobs *BlobStore
	cfg   WriterConfig
	log   *slog.Logger

	in   chan WriteRequest
	done chan struct{}

	ctxMu  sync.RWMutex
	closed bool

	ctr writerCounters
}

type writerCounters struct {
	enqueued    atomic.Int64
	rejected    atomic.Int64
	items       atomic.Int64
	batches     atomic.Int64
	writeErrors atomic.Int64
	checkpoints atomic.Int64
	lastFlushMs atomic.Int64
}

// NewWriter 构造写入器（尚未启动）。
func NewWriter(db *DB, blobs *BlobStore, cfg WriterConfig, log *slog.Logger) (*Writer, error) {
	if db == nil {
		return nil, errors.New("store: writer needs a database")
	}
	if blobs == nil {
		return nil, errors.New("store: writer needs a blob store")
	}
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()
	return &Writer{
		db:    db,
		blobs: blobs,
		cfg:   cfg,
		log:   log,
		in:    make(chan WriteRequest, cfg.QueueSize),
		done:  make(chan struct{}),
	}, nil
}

// Start 启动写入 goroutine。ctx 取消时会把剩余批次刷完再退出。
func (w *Writer) Start(ctx context.Context) {
	go w.loop(ctx)
}

// Enqueue 提交一次写入。队列满时最多等待 EnqueueTimeout，仍满则返回 ErrQueueFull。
//
// 故意不做无限阻塞：捕获 goroutine 卡住会让整条监听链路停摆，
// 而少记一条剪贴板远比停止记录要好。
func (w *Writer) Enqueue(req WriteRequest) error {
	w.ctxMu.RLock()
	defer w.ctxMu.RUnlock()
	if w.closed {
		return ErrWriterClosed
	}

	select {
	case w.in <- req:
		w.ctr.enqueued.Add(1)
		return nil
	default:
	}

	timer := time.NewTimer(w.cfg.EnqueueTimeout)
	defer timer.Stop()
	select {
	case w.in <- req:
		w.ctr.enqueued.Add(1)
		return nil
	case <-timer.C:
		w.ctr.rejected.Add(1)
		return ErrQueueFull
	case <-w.done:
		w.ctr.rejected.Add(1)
		return ErrWriterClosed
	}
}

// Close 停止接收新请求，等待队列排空与最后一批提交完成。
func (w *Writer) Close() error {
	w.ctxMu.Lock()
	if w.closed {
		w.ctxMu.Unlock()
		<-w.done
		return nil
	}
	w.closed = true
	close(w.in)
	w.ctxMu.Unlock()
	<-w.done
	return nil
}

// Stats 返回累计计数。
func (w *Writer) Stats() WriterStats {
	return WriterStats{
		Enqueued:    w.ctr.enqueued.Load(),
		Rejected:    w.ctr.rejected.Load(),
		Items:       w.ctr.items.Load(),
		Batches:     w.ctr.batches.Load(),
		Errors:      w.ctr.writeErrors.Load(),
		Checkpoints: w.ctr.checkpoints.Load(),
		LastFlushMs: w.ctr.lastFlushMs.Load(),
	}
}

// Flush 阻塞到当前队列被排空并提交。
//
// 实现方式是往队列尾部塞一个哨兵请求：批次是 FIFO 且逐批提交，
// 哨兵被处理时，它前面的写入必然都已经 commit 过了。
func (w *Writer) Flush(ctx context.Context) error {
	ack := make(chan WriteResult, 1)
	if err := w.Enqueue(WriteRequest{Result: ack}); err != nil {
		return err
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ── 写入 goroutine ──────────────────────────────────────────────

func (w *Writer) loop(ctx context.Context) {
	defer close(w.done)

	pending := make([]WriteRequest, 0, w.cfg.BatchMax)

	// 定时器只在有积压时启动：空载时一次唤醒都不产生（空闲 CPU ≈ 0）。
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		timerC = nil
	}
	armTimer := func() {
		stopTimer()
		timer = time.NewTimer(w.cfg.FlushInterval)
		timerC = timer.C
	}

	var ckptC <-chan time.Time
	if w.cfg.CheckpointInterval > 0 {
		t := time.NewTicker(w.cfg.CheckpointInterval)
		defer t.Stop()
		ckptC = t.C
	}

	writesSinceCheckpoint := 0
	checkpoint := func(force bool) {
		if w.cfg.CheckpointEvery <= 0 {
			return
		}
		if !force && writesSinceCheckpoint < w.cfg.CheckpointEvery {
			return
		}
		if err := w.db.Checkpoint(); err != nil {
			w.log.Warn("wal checkpoint failed", "err", err)
			return
		}
		writesSinceCheckpoint = 0
		w.ctr.checkpoints.Add(1)
	}

	flush := func(fctx context.Context) {
		stopTimer()
		if len(pending) == 0 {
			return
		}
		n := w.flushBatch(fctx, pending)
		writesSinceCheckpoint += n
		w.ctr.items.Add(int64(n))
		w.ctr.batches.Add(1)
		pending = pending[:0]
		checkpoint(false)
	}

	shutdownFlush := func() {
		// 关停时原 ctx 已取消，BeginTx 会直接报错，所以换一个带超时的干净 ctx。
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		flush(fctx)
		checkpoint(true)
	}

	for {
		select {
		case <-ctx.Done():
			shutdownFlush()
			dctx, dcancel := fctxWithTimeout()
			w.drain(dctx, dcancel)
			return

		case req, ok := <-w.in:
			if !ok {
				shutdownFlush()
				return
			}
			pending = append(pending, req)
			if len(pending) == 1 {
				armTimer()
			}
			if len(pending) >= w.cfg.BatchMax {
				flush(ctx)
			}

		case <-timerC:
			flush(ctx)

		case <-ckptC:
			checkpoint(true)
		}
	}
}

func fctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// drain 在 ctx 取消后把还留在 channel 里的请求尽量落库。
func (w *Writer) drain(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	pending := make([]WriteRequest, 0, w.cfg.BatchMax)
	for {
		select {
		case req, ok := <-w.in:
			if !ok {
				if len(pending) > 0 {
					w.flushBatch(ctx, pending)
				}
				return
			}
			pending = append(pending, req)
			if len(pending) >= w.cfg.BatchMax {
				w.flushBatch(ctx, pending)
				pending = pending[:0]
			}
		default:
			if len(pending) > 0 {
				w.flushBatch(ctx, pending)
			}
			return
		}
	}
}

// flushBatch 在**一个事务**里提交一批写入，返回成功写入的条数。
//
// 单条失败不中断整批：SQLite 里单条语句失败（如 NOT NULL 违规）不会污染事务，
// 继续处理其余条目能把损失降到最小；这类失败会记进 Errors 计数。
//
// ⚠️ 回执必须在 Commit **之后**才发。之前踩过这个坑：在循环里逐条 reply，
// 调用方拿到"成功"就立刻去查库，此时事务还没提交，读连接看到的是旧快照——
// 表现为 use_count 数字对不上、刚写的条目查不到。合批 + 读写分离之后，
// "提交完成"是回执能代表的最小语义。
func (w *Writer) flushBatch(ctx context.Context, batch []WriteRequest) int {
	started := time.Now()
	defer func() {
		w.ctr.lastFlushMs.Store(time.Since(started).Milliseconds())
	}()

	tx, err := w.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		w.ctr.writeErrors.Add(1)
		w.log.Error("cannot begin write transaction", "err", err, "batch", len(batch))
		w.replyAll(batch, WriteResult{Err: fmt.Errorf("store: begin tx: %w", err)})
		return 0
	}

	type pendingReply struct {
		ch  chan WriteResult
		res WriteResult
	}
	replies := make([]pendingReply, 0, len(batch))
	written := 0

	for i := range batch {
		req := &batch[i]
		if req.Item.Fingerprint == "" && len(req.Blobs) == 0 {
			// Flush 用的哨兵请求：不需要写任何东西，但它的回执同样要等到提交之后
			replies = append(replies, pendingReply{req.Result, WriteResult{}})
			continue
		}
		res, err := w.writeOne(ctx, tx, req)
		if err != nil {
			w.ctr.writeErrors.Add(1)
			w.log.Error("write failed", "fingerprint", req.Item.Fingerprint, "err", err)
			replies = append(replies, pendingReply{req.Result, WriteResult{Err: err}})
			continue
		}
		written++
		replies = append(replies, pendingReply{req.Result, res})
	}

	if commitErr := tx.Commit(); commitErr != nil {
		w.ctr.writeErrors.Add(1)
		w.log.Error("cannot commit write transaction", "err", commitErr, "batch", len(batch))
		for _, r := range replies {
			if r.res.Err == nil {
				r.res.Err = fmt.Errorf("store: commit: %w", commitErr)
			}
			reply(r.ch, r.res)
		}
		return 0
	}

	for _, r := range replies {
		reply(r.ch, r.res)
	}
	return written
}

func (w *Writer) replyAll(batch []WriteRequest, res WriteResult) {
	for i := range batch {
		reply(batch[i].Result, res)
	}
}

// writeOne 在事务内处理一条请求：先落 blob，再插行。
func (w *Writer) writeOne(ctx context.Context, tx *sql.Tx, req *WriteRequest) (WriteResult, error) {
	paths := make(map[BlobRole]string, len(req.Blobs))

	for _, b := range req.Blobs {
		if len(b.Data) == 0 {
			continue
		}
		rel, shaHex, err := w.blobs.Put(b.Data, b.Ext)
		if err != nil {
			return WriteResult{}, err
		}
		paths[b.Role] = rel
		req.Item.ByteSize += int64(len(b.Data))

		switch b.Role {
		case BlobRoleImage:
			req.Item.ImagePath = &rel
			if b.Thumb {
				thumbRel, _, _, err := w.blobs.PutThumb(b.Data, shaHex)
				if err != nil {
					// 缩略图是派生数据：单张失败只降级，不能让整条记录落不下去
					w.log.Warn("thumbnail generation failed", "blob", rel, "err", err)
				} else {
					req.Item.ThumbPath = &thumbRel
				}
			}
		case BlobRoleRTF:
			req.Item.RTFPath = &rel
		}
	}

	res, err := PutItem(ctx, tx, &req.Item)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		ID:          res.ID,
		UseCount:    res.UseCount,
		FirstSeenAt: res.FirstSeenAt,
		BlobPaths:   paths,
	}, nil
}

func reply(ch chan WriteResult, res WriteResult) {
	if ch == nil {
		return
	}
	select {
	case ch <- res:
	default:
		// Result 约定为缓冲 1 的 channel；满了说明调用方没在读，
		// 但绝不能因此阻塞写入器。
	}
}
