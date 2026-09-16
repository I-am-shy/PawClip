// Package capture 把"平台后端的变更信号"变成"落库条目"。
//
// 它是 docs/DESIGN.md §2 goroutine 模型里的[捕获 goroutine]：
//
//	[监听 goroutine] ──Tick──▶ [捕获 goroutine] ──WriteRequest──▶ [写入 goroutine]
//	  (clipboard.Backend)        Read → filter → normalize → fingerprint      (store.Writer)
//
// 为什么单独一个包、而不是塞进 clipboard/：
//
//  1. clipboard/ 的定位是"平台原生层"（§10），它不该知道数据库的存在。
//     单独成包后 clipboard 仍然零持久化依赖，那 46 个单测不需要任何 DB。
//  2. §2 的图里捕获 goroutine 位于 Backend 之上，它的依赖方向是
//     capture → {clipboard, store}。
//  3. 避免将来的环：M2 的回写/自动粘贴（writer.go）同时需要 store 与
//     clipboard，若捕获逻辑住在 clipboard 里，clipboard→store 就定死了，
//     反过来 store 想引用 clipboard 的 Payload 类型时立刻成环。
package capture

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/store"
)

// Config 是捕获流水线的调参。字段全部来自 settings 表（docs/DESIGN.md §9）。
type Config struct {
	// Filter ← capture.* / exclude.*，决策规则。
	Filter clipboard.FilterConfig
	// TextMaxChars ← capture.textMaxChars。
	//
	// 注意：docs/HANDOFF-PROMPT.md §4 第 4 条要求"指纹对完整内容做 sha256，
	// 只截断存储"。这个上限同时用于 html_content——§4.1 的 items 表
	// 没有 html_path 列，HTML 只能内联，而 §9 也没有单独的 html 上限键。
	TextMaxChars int
	// TickBuffer 是变更信号 channel 的容量。
	//
	// 故意做小：「监听侧」因为队列满而丢弃信号是安全的（下一次捕获读到的
	// 永远是剪贴板当前状态），但把它开大反而会积压一串过期信号，
	// 让同一次复制被重复读多次。
	TickBuffer int
}

// DefaultConfig 返回 §9 默认值对应的捕获配置。
func DefaultConfig() Config {
	return Config{
		Filter:       clipboard.DefaultFilterConfig(),
		TextMaxChars: clipboard.TextMaxCharsDefault,
		TickBuffer:   8,
	}
}

func (c Config) withDefaults() Config {
	out := c
	if out.TextMaxChars <= 0 {
		out.TextMaxChars = clipboard.TextMaxCharsDefault
	}
	if out.TickBuffer <= 0 {
		out.TickBuffer = 8
	}
	return out
}

// Stats 是流水线的累计计数，仅供诊断与验收取证。
//
// 关于 Reads 与 Processed 的区别（测试里当屏障用，别搞混）：
//
//	Reads     在读成功后**立即**自增——此刻 filter/buildRequest/Enqueue 都还没跑。
//	Processed 在一次快照**完全处理完**之后自增——要么已经 Enqueue 进写入队列，
//	          要么已经被过滤器丢弃、要么入队失败。它才是"这一跳已经了结"的信号。
//
// 拿 Reads 当屏障、紧接着再 Flush 是不安全的：捕获 goroutine 可能正好被抢占在
// "读完"和"入队"之间，Flush 的哨兵就会排在那条还没入队的内容**前面**被写入器
// 处理掉，Flush 提前返回，断言于是看到少一条。这不是假绿而是假红：它偶发、与
// 流水线正确性无关，却会掩盖真正的回归。所以 testutil_test.go 里刻意**不提供**
// 基于 Reads 的等待辅助函数——凡是"等这一跳的落库结果"，一律用 Processed。
type Stats struct {
	Ticks      int64            // 收到的变更信号数
	Reads      int64            // 成功的 Read 次数（不含后续过滤/入队）
	Processed  int64            // 读取成功后完整走完 accept 的快照数
	Accepted   int64            // 通过过滤、已交给写入器的条目数
	EnqueueErr int64            // 写入队列满/已关闭导致的丢弃
	ReadErrs   int64            // Read 的真实错误（不含 Empty/Busy/Flapping）
	Empty      int64            // 剪贴板里没有我们认识的表示
	Busy       int64            // 剪贴板被别的进程占住
	Flapping   int64            // 读期间 changeCount 仍在变
	Drops      map[string]int64 // 各丢弃原因的计数
	LastError  string
}

// Capture 是捕获流水线。零值不可用，用 New 构造。
type Capture struct {
	backend clipboard.Backend
	writer  *store.Writer
	guard   *clipboard.SelfWriteGuard
	filter  *clipboard.Filter
	cfg     Config
	log     *slog.Logger

	ticks chan clipboard.Tick
	stop  chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	started bool
	stopped bool

	// now 可注入，便于测试控制时间。
	now func() time.Time

	ticksCtr  atomic.Int64
	readsCtr  atomic.Int64
	procCtr   atomic.Int64
	acceptCtr atomic.Int64
	enqErrCtr atomic.Int64
	readErr   atomic.Int64
	emptyCtr  atomic.Int64
	busyCtr   atomic.Int64
	flapCtr   atomic.Int64
	lastErrMu sync.Mutex
	lastErr   string
}

// New 构造捕获流水线（尚未启动）。
//
// guard 由本类型持有并在内部创建：过滤器的自写入判定与回写路径（M2 的
// writer.go）必须用**同一个**守卫实例，否则"我们刚写的"永远认不出来。
// 通过 Guard() 把它交给回写路径。
func New(backend clipboard.Backend, writer *store.Writer, cfg Config, log *slog.Logger) (*Capture, error) {
	if backend == nil {
		return nil, errors.New("capture: nil clipboard backend")
	}
	if writer == nil {
		return nil, errors.New("capture: nil store writer")
	}
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()

	guard := clipboard.NewSelfWriteGuard()
	return &Capture{
		backend: backend,
		writer:  writer,
		guard:   guard,
		filter:  clipboard.NewFilter(cfg.Filter, guard),
		cfg:     cfg,
		log:     log,
		ticks:   make(chan clipboard.Tick, cfg.TickBuffer),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		now:     time.Now,
	}, nil
}

// Guard 返回自写入守卫。回写路径必须在 Write 之前 Arm 它。
func (c *Capture) Guard() *clipboard.SelfWriteGuard { return c.guard }

// Filter 返回当前过滤器（只读用途，例如取 Stats）。
func (c *Capture) Filter() *clipboard.Filter { return c.filter }

// ApplyFilter 热替换过滤配置（设置界面改完立刻生效）。
//
// 为什么不需要停捕获循环：Filter 内部把配置装在一个不可变状态里、
// 用 atomic.Pointer 整体替换（见 clipboard/filter.go 的 filterState 注释），
// 所以捕获 goroutine 下一次 Decide 就会用上新规则，不会有半更新状态。
//
// ⚠️ 一个真实存在的时序缺口（不是 bug，是设计取舍）：macOS 是 0.2s 轮询，
// 所以"用户在设置里关掉记录"到"真的不再记"之间最多有 200ms 的窗口。
// 要消掉它就得让捕获循环每轮读一次配置，代价是每条热路径都多一次同步；
// 200ms 的记录延迟对剪贴板工具无所谓，所以选择保留。
func (c *Capture) ApplyFilter(cfg clipboard.FilterConfig) error {
	if c.filter == nil {
		return errors.New("capture: filter not initialized")
	}
	c.filter.Apply(cfg)
	c.log.Info("过滤配置已热更新",
		"enabled", cfg.Enabled,
		"types", cfg.Types,
		"excludeApps", len(cfg.ExcludeApps),
	)
	return nil
}

// Backend 返回底层平台后端，供回写路径调用 Write。
func (c *Capture) Backend() clipboard.Backend { return c.backend }

// Start 启动后端监听与捕获 goroutine。
//
// ctx 取消等价于 Stop()：会关掉后端并让捕获 goroutine 退出。
func (c *Capture) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("capture: already started")
	}
	if c.stopped {
		c.mu.Unlock()
		return errors.New("capture: already stopped")
	}
	c.mu.Unlock()

	// 先起后端：它失败时我们不留下一个空转的捕获 goroutine。
	if err := c.backend.Start(c.ticks); err != nil {
		return err
	}

	c.mu.Lock()
	c.started = true
	c.mu.Unlock()

	go c.loop(ctx)
	return nil
}

// Stop 停止捕获。可重复调用；未启动（或 Start 失败）时也能安全调用。
func (c *Capture) Stop() {
	c.mu.Lock()
	first := !c.stopped
	c.stopped = true
	started := c.started
	c.mu.Unlock()

	if first {
		close(c.stop)
	}
	if started {
		// 未启动时 done 永远不会关闭，这里必须跳过，否则 Stop 会死等。
		<-c.done
	}
}

func (c *Capture) loop(ctx context.Context) {
	defer close(c.done)

	for {
		select {
		case <-ctx.Done():
			c.backend.Stop()
			return
		case <-c.stop:
			c.backend.Stop()
			return
		case tk := <-c.ticks:
			c.handle(tk)
		}
	}
}

// handle 处理一次变更信号。
//
// 这里刻意**不**把阅读失败升级为致命错误：剪贴板是全局共享资源，
// 别的进程正持有时读失败是常态，跳过这一次、等下一个信号即可。
func (c *Capture) handle(tk clipboard.Tick) {
	c.ticksCtr.Add(1)

	raw, err := c.backend.Read()
	if err != nil {
		switch {
		case errors.Is(err, clipboard.ErrEmpty):
			c.emptyCtr.Add(1)
		case errors.Is(err, clipboard.ErrBusy):
			c.busyCtr.Add(1)
		case errors.Is(err, clipboard.ErrFlapping):
			c.flapCtr.Add(1)
		case errors.Is(err, clipboard.ErrUnsupported):
			c.readErr.Add(1)
			c.setLastError(err)
			c.log.Warn("clipboard backend unsupported", "err", err)
		default:
			c.readErr.Add(1)
			c.setLastError(err)
			c.log.Warn("clipboard read failed", "err", err)
		}
		return
	}
	c.readsCtr.Add(1)

	c.accept(raw)

	// 放在 accept 之后：这一刻起，本次快照要么已在写入队列里、要么已被丢弃。
	// 测试靠它当屏障（见 Stats 的注释）。
	c.procCtr.Add(1)
}

// accept 执行 过滤 → 归一化 → 指纹 → 入队。
func (c *Capture) accept(raw *clipboard.Raw) {
	content := raw.Content()
	// 指纹必须对**完整内容**算（docs/HANDOFF-PROMPT.md §4 第 4 条），
	// 一旦在这里先截断再算，超长文本的重复复制就不会被去重命中。
	fp := clipboard.Fingerprint(content)
	now := c.now()

	if d := c.filter.Decide(raw, fp, now); d.Dropped() {
		return
	}

	req := c.buildRequest(raw, content, fp, now)
	if err := c.writer.Enqueue(req); err != nil {
		c.enqErrCtr.Add(1)
		c.setLastError(err)
		c.log.Warn("cannot enqueue clipboard item", "fingerprint", fp, "err", err)
		return
	}
	c.acceptCtr.Add(1)
}

// buildRequest 把一次快照翻译成一条落库请求。
func (c *Capture) buildRequest(raw *clipboard.Raw, content clipboard.Content, fp string, now time.Time) store.WriteRequest {
	sec := now.Unix()
	kind := content.Kind()

	it := store.Item{
		Kind:          kind,
		Fingerprint:   fp,
		SourceAppID:   raw.SourceAppID,
		SourceAppName: raw.SourceAppName,
		FirstSeenAt:   sec,
		CreatedAt:     sec,
		LastUsedAt:    &sec,
		// expires_at 留 NULL（= 永不过期）。TTL 分级属于 §5 生命周期，
		// 不在 M1 范围内（docs/HANDOFF-PROMPT.md §三 明确排除 GC）。
	}

	var blobs []store.BlobWrite

	// 文本组：截断存储、完整指纹。空字符串不落列——Content.Groups() /
	// Kind() 已经把空串当成"没有这个表示"，存储层要对齐这个语义。
	if content.Text != nil && *content.Text != "" {
		t, _ := clipboard.TruncateText(*content.Text, c.cfg.TextMaxChars)
		it.TextContent = &t
		it.ByteSize += int64(len(t))
	}
	if content.HTML != nil && *content.HTML != "" {
		// §4.1 的 items 没有 html_path，M1 一律内联（见收尾报告的"文档缺口"）。
		h, _ := clipboard.TruncateText(*content.HTML, c.cfg.TextMaxChars)
		it.HTMLContent = &h
		it.ByteSize += int64(len(h))
	}

	// RTF 与图片走 blob。图片额外生成缩略图。
	if len(content.RTF) > 0 {
		blobs = append(blobs, store.BlobWrite{
			Role: store.BlobRoleRTF, Data: content.RTF, Ext: "rtf",
		})
	}
	if len(content.PNG) > 0 {
		blobs = append(blobs, store.BlobWrite{
			Role: store.BlobRoleImage, Data: content.PNG, Ext: "png", Thumb: true,
		})
	}

	// 文件列表内联为 JSON（items.file_paths）。
	if len(content.Files) > 0 {
		it.FilePaths = content.Files
		for _, f := range content.Files {
			it.ByteSize += int64(len(f))
		}
	}

	var w, h int
	if raw.Image != nil {
		w, h = raw.Image.Width, raw.Image.Height
	}
	it.Preview = clipboard.Preview(content, kind, w, h)

	return store.WriteRequest{Item: it, Blobs: blobs}
}

// Stats 返回累计计数快照。
func (c *Capture) Stats() Stats {
	return Stats{
		Ticks:      c.ticksCtr.Load(),
		Reads:      c.readsCtr.Load(),
		Processed:  c.procCtr.Load(),
		Accepted:   c.acceptCtr.Load(),
		EnqueueErr: c.enqErrCtr.Load(),
		ReadErrs:   c.readErr.Load(),
		Empty:      c.emptyCtr.Load(),
		Busy:       c.busyCtr.Load(),
		Flapping:   c.flapCtr.Load(),
		Drops:      c.filter.Stats(),
		LastError:  c.loadLastError(),
	}
}

func (c *Capture) setLastError(err error) {
	if err == nil {
		return
	}
	c.lastErrMu.Lock()
	c.lastErr = err.Error()
	c.lastErrMu.Unlock()
}

func (c *Capture) loadLastError() string {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()
	return c.lastErr
}
