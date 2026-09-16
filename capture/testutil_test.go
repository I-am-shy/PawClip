package capture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/store"
)

// ── 假后端 ──────────────────────────────────────────────────────
//
// docs/HANDOFF-PROMPT.md §五 明确要求：验收 1 / 2 / 3 用**注入的假后端**跑，
// 保证可重复、不依赖真实剪贴板。这个 fake 就是那个注入点。
//
// 它的两个行为刻意模仿真后端：
//   - Write 之后 Read 必须返回这份内容（否则自写入守卫就成了空转）；
//   - Read 的错误可以按需置入，用来验证错误分桶。

type fakeBackend struct {
	mu       sync.Mutex
	raw      *clipboard.Raw
	readErr  error
	writes   []*clipboard.Payload
	ch       chan<- clipboard.Tick
	started  bool
	stopped  bool
	readCnt  int
	writeErr error
}

func newFakeBackend() *fakeBackend { return &fakeBackend{} }

func (b *fakeBackend) Start(ch chan<- clipboard.Tick) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errors.New("fake: already started")
	}
	b.ch, b.started = ch, true
	return nil
}

func (b *fakeBackend) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
}

func (b *fakeBackend) Read() (*clipboard.Raw, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readCnt++
	if b.readErr != nil {
		return nil, b.readErr
	}
	if b.raw == nil {
		return nil, clipboard.ErrEmpty
	}
	return b.raw, nil
}

// Write 记录写入并把"剪贴板内容"换成这份载荷——真后端就是这样。
func (b *fakeBackend) Write(p *clipboard.Payload) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writeErr != nil {
		return b.writeErr
	}
	b.writes = append(b.writes, p)
	b.raw = payloadToRaw(p)
	return nil
}

func (b *fakeBackend) IsPrivate(r *clipboard.Raw) bool {
	return clipboard.IsPrivateTypeNames(r.RawTypes)
}

func (b *fakeBackend) setRaw(r *clipboard.Raw) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.raw = r
}

func (b *fakeBackend) setReadErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readErr = err
}

// push 推一个变更信号。队列满时丢弃——真后端（darwin.go）也是这个策略。
func (b *fakeBackend) push() {
	b.mu.Lock()
	ch := b.ch
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- clipboard.Tick{AtMs: time.Now().UnixMilli()}:
	default:
	}
}

func (b *fakeBackend) writeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.writes)
}

// payloadToRaw 模拟"真后端把这份载荷读回来"。
//
// 忠实度要求：Raw 必须由 Payload 的**同一批字节**派生，
// 这样 (*Payload).Fingerprint() 与 (*Raw).Fingerprint() 才会相等。
func payloadToRaw(p *clipboard.Payload) *clipboard.Raw {
	if p == nil {
		return &clipboard.Raw{}
	}
	r := &clipboard.Raw{
		Text:  p.Text,
		HTML:  p.HTML,
		RTF:   p.RTF,
		Files: p.Files,
	}
	if len(p.PNG) > 0 {
		if img, err := clipboard.ImageFromPNG(p.PNG); err == nil {
			r.Image = img
		}
	}
	if p.Text != nil {
		r.RawTypes = append(r.RawTypes, "public.utf8-plain-text")
	}
	if p.HTML != nil {
		r.RawTypes = append(r.RawTypes, "public.html")
	}
	if len(p.RTF) > 0 {
		r.RawTypes = append(r.RawTypes, "public.rtf")
	}
	if len(p.PNG) > 0 {
		r.RawTypes = append(r.RawTypes, "public.png")
	}
	if len(p.Files) > 0 {
		r.RawTypes = append(r.RawTypes, "nsfilenamespboardtype")
	}
	return r
}

// ── 通用测试脚手架 ──────────────────────────────────────────────

// quietLogger 让测试输出保持干净；需要取证时换成 slog.NewTextHandler(os.Stderr, …)。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func strPtr(s string) *string { return &s }

type pipeline struct {
	cap    *Capture
	db     *store.DB
	writer *store.Writer
	blobs  *store.BlobStore
	dir    string
}

// newTestPipeline 起一条完整的落库链路：真 DB + 真 BlobStore + 真 Writer，
// 只有剪贴板后端是假的。
//
// 写入器用**加快的**合批窗口（5ms / 10 条）——功能测试关心的是"落库结果对不对"，
// 不该被 50ms 的合批窗口拖成几十秒。需要按**生产参数**测延迟的用例
// （latency_test.go）走 newProdPipeline。
func newTestPipeline(t *testing.T, b clipboard.Backend, cfg Config) *pipeline {
	t.Helper()
	return newTestPipelineWith(t, b, cfg, store.WriterConfig{
		FlushInterval: 5 * time.Millisecond,
		BatchMax:      10,
	})
}

// newProdPipeline 用 WriterConfig 的**默认值**（合批窗口 50ms / 20 条，
// 全部来自 store.WriterConfig.withDefaults）起链路，供 §12 的延迟验收使用。
func newProdPipeline(t *testing.T, b clipboard.Backend) *pipeline {
	t.Helper()
	return newTestPipelineWith(t, b, DefaultConfig(), store.WriterConfig{})
}

// newTestPipelineWith 是上面两个的公共实现。
func newTestPipelineWith(t *testing.T, b clipboard.Backend, cfg Config, wcfg store.WriterConfig) *pipeline {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(store.Options{
		Path:   filepath.Join(dir, "paw.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	blobs, err := store.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("store.NewBlobStore: %v", err)
	}
	w, err := store.NewWriter(db, blobs, wcfg, quietLogger())
	if err != nil {
		t.Fatalf("store.NewWriter: %v", err)
	}
	w.Start(context.Background())

	c, err := New(b, w, cfg, quietLogger())
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}

	p := &pipeline{cap: c, db: db, writer: w, blobs: blobs, dir: dir}
	t.Cleanup(func() {
		c.Stop()
		_ = w.Close()
		_ = db.Close()
	})
	return p
}

// waitStats 轮询直到 cond 成立，返回成立时的快照。
//
// 为什么统一走轮询而不是 sleep 一个固定时长：流水线是异步的，固定 sleep
// 既慢又不稳；轮询既能把"等待"压到毫秒级，又能把超时时的真实 stats 打进
// 失败信息里——排查"到底卡在哪一步"时这比任何断言都值钱。
func (p *pipeline) waitStats(t *testing.T, what string, cond func(Stats) bool) Stats {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last Stats
	for time.Now().Before(deadline) {
		last = p.cap.Stats()
		if cond(last) {
			return last
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %s 超时；stats=%+v", what, last)
	return last
}

// waitTicks 等到累计收到 n 个变更信号。
func (p *pipeline) waitTicks(t *testing.T, n int64) {
	t.Helper()
	p.waitStats(t, "ticks", func(s Stats) bool { return s.Ticks >= n })
}

// waitProcessed 等到累计 n 次快照**完整处理完**（已入队或已丢弃）。
//
// 这是"可以安全断言下游结果"的屏障，也是本包里**唯一**该用来同步的等待：
// 返回时，第 n 跳要么已经在写入队列里、要么已经走完过滤被丢弃，之后调
// p.flush(t) 一定能把它的落库结果冲下去。
//
// 不要改用 Stats.Reads 来等：那个计数在"读到内容"之后就加了，filter /
// buildRequest / Enqueue 全都没跑；捕获 goroutine 恰好被抢占在那里时，
// Flush 的哨兵会插队到还没入队的内容前面，断言就会看到少一条（偶发假红，
// 见 capture.Stats 的注释）。本包已不提供基于 Reads 的等待辅助函数，
// 就是为了不给人留下这个坑。
func (p *pipeline) waitProcessed(t *testing.T, n int64) {
	t.Helper()
	p.waitStats(t, "processed", func(s Stats) bool { return s.Processed >= n })
}

// waitEmpty 等到累计 n 次"剪贴板里没有我们认识的表示"。
func (p *pipeline) waitEmpty(t *testing.T, n int64) {
	t.Helper()
	p.waitStats(t, "empty", func(s Stats) bool { return s.Empty >= n })
}

// waitAccepted 等到累计放行条目数达到 n。
func (p *pipeline) waitAccepted(t *testing.T, n int64) {
	t.Helper()
	p.waitStats(t, "accepted", func(s Stats) bool { return s.Accepted >= n })
}

func (p *pipeline) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.writer.Flush(ctx); err != nil {
		t.Fatalf("writer.Flush: %v", err)
	}
}

func (p *pipeline) countAlive(t *testing.T) int64 {
	t.Helper()
	n, err := p.db.CountAlive(context.Background())
	if err != nil {
		t.Fatalf("CountAlive: %v", err)
	}
	return n
}

// makePNG 生成一张 w×h 的噪声 PNG，用作图片载荷。
func makePNG(t *testing.T, w, h int) []byte { return makeSeededPNG(t, w, h, 0) }

// makeSeededPNG 让像素随 seed 变化，用于需要"每次都不同的图片"的场景
// （例如验收 3 里持续产生新 blob 才能压出崩溃窗口）。
func makeSeededPNG(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x*7 + seed*31) % 256),
				G: uint8((y*11 + seed*17) % 256),
				B: uint8((x ^ y ^ seed) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// itemRow 是测试用的 items 投影（store 未导出 itemColumns，这里只取需要的列）。
type itemRow struct {
	ID        int64
	Kind      string
	Text      string
	HTML      string
	Preview   string
	ImagePath sql.NullString
	ThumbPath sql.NullString
	FilePaths sql.NullString
	ByteSize  int64
}

func (r itemRow) files(t *testing.T) []string {
	t.Helper()
	if !r.FilePaths.Valid || r.FilePaths.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(r.FilePaths.String), &out); err != nil {
		t.Fatalf("decode file_paths %q: %v", r.FilePaths.String, err)
	}
	return out
}

func allItems(t *testing.T, db *store.DB) []itemRow {
	t.Helper()
	rows, err := db.Reader().QueryContext(context.Background(),
		`SELECT id, kind, coalesce(text_content,''), coalesce(html_content,''),
		        coalesce(preview,''), image_path, thumb_path, file_paths, byte_size
		   FROM items WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	defer rows.Close()

	var out []itemRow
	for rows.Next() {
		var r itemRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Text, &r.HTML, &r.Preview,
			&r.ImagePath, &r.ThumbPath, &r.FilePaths, &r.ByteSize); err != nil {
			t.Fatalf("scan item: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate items: %v", err)
	}
	return out
}

func dumpItems(t *testing.T, db *store.DB) {
	t.Helper()
	for _, r := range allItems(t, db) {
		t.Logf("item id=%d kind=%s byte_size=%d preview=%q text=%q image_path=%v thumb_path=%v files=%v",
			r.ID, r.Kind, r.ByteSize, r.Preview, r.Text,
			r.ImagePath.String, r.ThumbPath.String, r.FilePaths.String)
	}
}
