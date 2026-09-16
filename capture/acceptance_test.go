package capture

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/store"
)

// 本文件是 docs/HANDOFF-PROMPT.md §五 的验收 1 / 2 / 3。
//
// 三条都用**注入的假后端**跑（§五 明文要求）：可重复、不依赖真实剪贴板、
// 不需要人工点击。真机抽查（验收 4）在 acceptance_real_darwin_test.go。

// ── 验收 1：无自捕获 ────────────────────────────────────────────
//
// 判据：连续 100 次回贴，历史条目数增加为 **0**。
//
// 这个测试的结构刻意做成"先证明链路是活的、再证明回贴不进去"：
// 前半段不带守卫地捕获一条（CountAlive == 1），后半段连续 100 次回贴，
// 最后断言 CountAlive **仍然是 1**。如果不做前半段，一个"什么都记不下来"
// 的实现也会让这个测试通过——那是假绿。
func TestAcceptance1_NoSelfCapture(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	// ① 先证明捕获链路是活的
	baseline := "baseline：这条应当被记下来"
	b.setRaw(&clipboard.Raw{Text: &baseline, RawTypes: []string{"public.utf8-plain-text"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 1 {
		t.Fatalf("基线捕获失败：CountAlive = %d，想要 1（链路没活，后面的断言没有意义）", n)
	}

	// ② 连续 100 次回贴。每次都是"Arm → Write → 后端内容变了 → 推送信号"，
	//    完全模拟 M2 里"用户点了一条历史 → 我们写回剪贴板"的时序。
	const rounds = 100
	for i := 0; i < rounds; i++ {
		payload := &clipboard.Payload{Text: strPtr(fmt.Sprintf("PawClip 回贴内容 #%d", i))}
		p.cap.Guard().Arm(payload.Fingerprint())

		if err := b.Write(payload); err != nil {
			t.Fatalf("第 %d 次回贴失败：%v", i, err)
		}
		b.push()
		p.waitProcessed(t, int64(i+2)) // 基线那次 +1
	}
	p.flush(t)

	// ③ 断言
	alive := p.countAlive(t)
	if alive != 1 {
		t.Fatalf("自捕获发生：CountAlive = %d，想要仍是 1（100 次回贴应产生 0 条新记录）", alive)
	}
	if got := p.cap.Guard().Dropped(); got != rounds {
		t.Errorf("守卫拦下 %d 次，想要 %d 次", got, rounds)
	}
	if got := p.cap.Stats().Drops["drop:self_write"]; got != rounds {
		t.Errorf("drop:self_write = %d，想要 %d（%+v）", got, rounds, p.cap.Stats().Drops)
	}
	t.Logf("验收 1 通过：100 次回贴 → CountAlive=%d，守卫拦下 %d 次，写回 %d 次",
		alive, p.cap.Guard().Dropped(), b.writeCount())
}

// ── 验收 2：去重正确性 ──────────────────────────────────────────
//
// 判据：同一内容复制 100 次，库中始终 1 行且 use_count = 100。
//
// 这里必须把时钟推着走：默认去抖窗口 120ms，如果 100 次都在同一个
// 去抖窗口内到达，它们会被去抖器合并成 1 次——那也是"1 行"，但 use_count
// 会是 1 而不是 100，测出来的就不是去重路径而是去抖路径了。
// 所以每次复制把注入时钟推进 200ms（> 去抖窗口），让 100 次都真正走到写库。
func TestAcceptance2_DedupeUseCount(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())

	base := time.Unix(1_700_000_000, 0)
	var step atomic.Int64
	p.cap.now = func() time.Time {
		return base.Add(time.Duration(step.Load()) * time.Millisecond)
	}
	p.start(t)

	text := "同一段文字：这条会被复制 100 次"
	payload := &clipboard.Payload{Text: &text}
	fp := payload.Fingerprint()

	const rounds = 100
	for i := 0; i < rounds; i++ {
		step.Store(int64(i * 200)) // 每次推过 120ms 去抖窗口
		b.setRaw(payloadToRaw(payload))
		b.push()
		p.waitProcessed(t, int64(i+1))
	}
	p.flush(t)

	if n := p.countAlive(t); n != 1 {
		t.Fatalf("去重失败：CountAlive = %d，想要 1", n)
	}

	it, err := p.db.GetByFingerprint(context.Background(), fp)
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if it.UseCount != rounds {
		t.Fatalf("use_count = %d，想要 %d", it.UseCount, rounds)
	}
	if it.FirstSeenAt != base.Unix() {
		t.Errorf("first_seen_at = %d，想要 %d（重复复制不该改写首次时间）", it.FirstSeenAt, base.Unix())
	}
	if it.CreatedAt <= it.FirstSeenAt {
		t.Errorf("created_at = %d 没有随最后一次复制前移（first_seen_at=%d），条目不会置顶",
			it.CreatedAt, it.FirstSeenAt)
	}

	// 直接数行数，不相信任何缓存
	var rows int64
	if err := p.db.Reader().QueryRow(
		`SELECT count(*) FROM items WHERE fingerprint = ? AND deleted_at IS NULL`, fp).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("指纹 %s 对应 %d 行，想要 1", fp, rows)
	}
	t.Logf("验收 2 通过：复制 %d 次 → 1 行，use_count=%d，first_seen_at=%d，created_at=%d",
		rounds, it.UseCount, it.FirstSeenAt, it.CreatedAt)
}

// ── 验收 3：断电安全（SIGKILL）────────────────────────────────────
//
// 判据：捕获过程中强杀进程，重启后库可正常打开、无半截记录。
//
// 做法：本测试把自己以子进程方式再跑一遍（环境变量传目标路径），子进程持续
// 捕获直到被 SIGKILL；父进程等它确认"已经在写了"之后开杀——不是"写完再杀"，
// 而是**写在半路上杀**，否则测不到崩溃恢复。

const (
	crashChildEnv    = "PAWCLIP_ACCEPTANCE3_DB"
	crashReadyMarker = "PAWCLIP-CRASH-CHILD-READY"
)

func TestAcceptance3_Kill9Recovery(t *testing.T) {
	if dbPath := os.Getenv(crashChildEnv); dbPath != "" {
		runCrashChild(t, dbPath)
		return // 不会到这里
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "paw.db")
	blobsRoot := filepath.Join(dir, "blobs")

	cmd := exec.Command(os.Args[0], "-test.run=^TestAcceptance3_Kill9Recovery$", "-test.v")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dbPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}

	ready := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if strings.Contains(sc.Text(), crashReadyMarker) {
				readyOnce.Do(func() { close(ready) })
			}
		}
		// 继续排空 stdout，避免子进程写满管道被阻塞
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("子进程 30s 内没有报告就绪")
	}

	// 让它真的写进去若干批（FlushInterval=5ms，800ms 足够几十批），
	// 然后**在它还在写的时候**开杀。
	time.Sleep(800 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil { // SIGKILL，没有 defer / 没有 Close
		t.Fatalf("kill -9: %v", err)
	}
	_ = cmd.Wait()

	// ① 标记文件必须还在：进程没走正常退出路径，clean_shutdown 没被删掉。
	//    这正是 §4 第 7 条"用标记文件代替无条件 integrity_check"的触发条件。
	if _, err := os.Stat(filepath.Join(dir, "clean_shutdown")); err != nil {
		t.Fatalf("强杀后 clean_shutdown 标记应当存在：%v", err)
	}

	// ② 重启：能打开 + 启动自检（因为见到标记，这次会真跑 integrity_check）
	ctx := context.Background()
	db, err := store.Open(store.Options{Path: dbPath, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("强杀后无法重新打开数据库：%v", err)
	}
	defer db.Close()

	if note := db.IntegrityNote(); note != "" {
		t.Fatalf("启动自检报告了问题：%s", note)
	}

	var verdict string
	if err := db.Reader().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&verdict); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(verdict), "ok") {
		t.Fatalf("integrity_check = %q，想要 ok", verdict)
	}

	// ③ 确实落了数据（否则"无半截记录"是因为压根没有记录）
	all, err := db.CountAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alive, err := db.CountAlive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all == 0 {
		t.Fatal("强杀后库里一条记录都没有，说明子进程没真正写进去，这个测试没测到东西")
	}

	// ④ 无半截记录：逐行校验不变量 + blob 回指的文件必须真实存在
	halfWritten := checkNoHalfWritten(t, db, blobsRoot)
	if halfWritten != 0 {
		t.Fatalf("发现 %d 条半截记录", halfWritten)
	}
	t.Logf("验收 3 通过：kill -9 后重开成功，integrity_check=ok，count(*) = %d（存活 %d），无半截记录",
		all, alive)
}

// runCrashChild 是验收 3 的子进程主体：持续捕获，永不主动退出。
//
// 它故意交替写文本与图片——图片会先落 blob 再插行，被强杀时才有可能
// 出现"行指向不存在的文件"这种半截状态。只写文本的话这条不变量根本
// 不会被触碰。
func runCrashChild(t *testing.T, dbPath string) {
	dir := filepath.Dir(dbPath)
	db, err := store.Open(store.Options{Path: dbPath, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("子进程 store.Open: %v", err)
	}
	blobs, err := store.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("子进程 NewBlobStore: %v", err)
	}
	// 合批窗口压到 5ms：800ms 内能攒下几十次 commit，保证有东西可测。
	w, err := store.NewWriter(db, blobs, store.WriterConfig{
		FlushInterval: 5 * time.Millisecond,
		BatchMax:      4,
	}, quietLogger())
	if err != nil {
		t.Fatalf("子进程 NewWriter: %v", err)
	}
	w.Start(context.Background())

	b := newFakeBackend()
	c, err := New(b, w, DefaultConfig(), quietLogger())
	if err != nil {
		t.Fatalf("子进程 capture.New: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("子进程 capture.Start: %v", err)
	}

	fmt.Println(crashReadyMarker)
	_ = os.Stdout.Sync()

	for i := 0; ; i++ {
		if i%2 == 0 {
			text := fmt.Sprintf("crash-text-%d-%s", i, strings.Repeat("x", 256))
			b.setRaw(&clipboard.Raw{Text: &text, RawTypes: []string{"public.utf8-plain-text"}})
		} else {
			// 图片内容随 i 变化 → PNG 字节与 sha256 都不同 → 每次都要落一个新
			// blob。"先落 blob 再插行"的崩溃窗口只有在这里才会被真正压到。
			text := fmt.Sprintf("crash-image-%d", i)
			b.setRaw(&clipboard.Raw{
				Text:     &text,
				Image:    mustImage(t, makeSeededPNG(t, 64, 48, i)),
				RawTypes: []string{"public.utf8-plain-text", "public.png"},
			})
		}
		b.push()
		time.Sleep(time.Millisecond)
	}
}

// checkNoHalfWritten 逐行校验 items 的不变量，返回违规行数。
func checkNoHalfWritten(t *testing.T, db *store.DB, blobsRoot string) int {
	t.Helper()
	rows, err := db.Reader().QueryContext(context.Background(),
		`SELECT id, kind, fingerprint, use_count, text_content, image_path, thumb_path, rtf_path
		   FROM items`)
	if err != nil {
		t.Fatalf("查询 items: %v", err)
	}
	defer rows.Close()

	bad := 0
	for rows.Next() {
		var (
			id        int64
			kind, fp  string
			useCount  int64
			text      sql.NullString
			imgPath   sql.NullString
			thumbPath sql.NullString
			rtfPath   sql.NullString
		)
		if err := rows.Scan(&id, &kind, &fp, &useCount, &text, &imgPath, &thumbPath, &rtfPath); err != nil {
			t.Fatalf("scan: %v", err)
		}
		report := func(format string, args ...any) {
			bad++
			t.Errorf("半截记录 id=%d: "+format, append([]any{id}, args...)...)
		}
		if kind == "" {
			report("kind 为空")
		}
		if fp == "" {
			report("fingerprint 为空")
		}
		if useCount < 1 {
			report("use_count = %d（一次成功的落库至少代表复制了一次）", useCount)
		}
		if !text.Valid && !imgPath.Valid && !rtfPath.Valid {
			report("既没有文本也没有 blob 路径")
		}
		// blob 路径必须在磁盘上真实存在。写入顺序是"先落盘 blob，再插行"，
		// 所以只要这个断言成立，就不可能出现指向不存在文件的悬空行。
		for _, p := range []struct {
			name string
			ns   sql.NullString
		}{{"image_path", imgPath}, {"thumb_path", thumbPath}, {"rtf_path", rtfPath}} {
			if !p.ns.Valid || p.ns.String == "" {
				continue
			}
			abs := filepath.Join(blobsRoot, filepath.FromSlash(p.ns.String))
			if _, err := os.Stat(abs); err != nil {
				report("%s=%q 指向的文件不存在: %v", p.name, p.ns.String, err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return bad
}
