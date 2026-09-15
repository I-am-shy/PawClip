package retention

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zego/pawclip/store"
)

// ── 测试脚手架 ──────────────────────────────────────────────────

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type harness struct {
	db    *store.DB
	blobs *store.BlobStore
	dir   string
	cfg   Config
}

// newHarness 造一个隔离的库 + blob 目录。
//
// 注意 t.TempDir()：每个用例一个全新库，GC 用例之间不能共享状态
// （淘汰类用例会真的删数据，共享库会让用例顺序影响结果）。
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(dir, "pawclip.db"), Logger: testLogger()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	bs, err := store.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	h := &harness{db: db, blobs: bs, dir: dir, cfg: DefaultConfig()}
	// 默认关掉 VACUUM：用例关心的是"选对了谁"，不是"文件是否被压紧"。
	// 单独有一个用例打开它。
	h.cfg.VacuumAfterDelete = false
	return h
}

func (h *harness) gc(t *testing.T) *GC {
	t.Helper()
	g, err := New(h.db, h.blobs, func() Config { return h.cfg }, testLogger())
	if err != nil {
		t.Fatalf("retention.New: %v", err)
	}
	return g
}

// put 插入一条条目并返回 id。
//
// 直接用 store.PutItem 而不是走写入器：GC 用例要精确控制 first_seen_at /
// expires_at / byte_size，绕开捕获路径反而更清楚。
func (h *harness) put(t *testing.T, it store.Item) int64 {
	t.Helper()
	if it.Fingerprint == "" {
		it.Fingerprint = "sha256:" + randHex()
	}
	if it.Kind == "" {
		it.Kind = "text"
	}
	if it.FirstSeenAt == 0 {
		it.FirstSeenAt = time.Now().Unix()
	}
	if it.CreatedAt == 0 {
		it.CreatedAt = it.FirstSeenAt
	}
	res, err := store.PutItem(context.Background(), h.db.Writer(), &it)
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	return res.ID
}

var hexSeq int

func randHex() string {
	hexSeq++
	// 64 位十六进制串（指纹列没有格式约束，但保持与真实值同形）。
	return padHex(hexSeq)
}

func padHex(n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = '0'
	}
	i := 63
	for n > 0 && i >= 0 {
		out[i] = digits[n%16]
		n /= 16
		i--
	}
	return string(out)
}

func (h *harness) item(t *testing.T, id int64) *store.Item {
	t.Helper()
	it, err := h.db.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%d): %v", id, err)
	}
	if it == nil {
		t.Fatalf("条目 %d 不存在", id)
	}
	return it
}

func (h *harness) countAlive(t *testing.T) int64 {
	t.Helper()
	n, err := h.db.CountAlive(context.Background())
	if err != nil {
		t.Fatalf("CountAlive: %v", err)
	}
	return n
}

func (h *harness) countAll(t *testing.T) int64 {
	t.Helper()
	n, err := h.db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	return n
}

func (h *harness) countTrashed(t *testing.T) int64 {
	t.Helper()
	n, err := h.db.CountTrashed(context.Background())
	if err != nil {
		t.Fatalf("CountTrashed: %v", err)
	}
	return n
}

func ptrStr(s string) *string { return &s }
func ptrI64(n int64) *int64   { return &n }

// ── 步骤 0：默认 TTL 的计算基准 ─────────────────────────────────

// 这条钉住一个很容易写错、而且错了以后**完全看不出来**的语义：
// expires_at 必须锚在 first_seen_at 上，不能锚在"现在"。
//
// 若用 now + ttl，每轮 GC 都会把过期时间往后推，条目永远不到期——
// 所有用例都还是绿的，只有"内容永远不会被清掉"这一个后果。
func TestGC_DefaultTTLAnchoredToFirstSeenAndIdempotent(t *testing.T) {
	h := newHarness(t)
	h.cfg.DefaultTTLSec = 30 * 86400
	now := time.Now().Unix()
	firstSeen := now - 10*86400 // 10 天前首次复制

	id := h.put(t, store.Item{FirstSeenAt: firstSeen, Preview: "十年前的今天"})

	g := h.gc(t)
	rep := g.RunOnce(context.Background())
	if rep.TTLApplied != 1 {
		t.Fatalf("TTLApplied = %d，想要 1", rep.TTLApplied)
	}

	got := h.item(t, id)
	want := firstSeen + 30*86400
	if got.ExpiresAt == nil || *got.ExpiresAt != want {
		t.Fatalf("expires_at = %v，想要 %d（first_seen + 30d）—— 用 now 当基准就会算成 %d",
			derefI64(got.ExpiresAt), want, now+30*86400)
	}
	if got.TTLSource == nil || *got.TTLSource != store.TTLSourceGlobal {
		t.Fatalf("ttl_source = %v，想要 %q", derefStr(got.TTLSource), store.TTLSourceGlobal)
	}

	// 幂等性：再跑一轮，值不能变。这是"每轮往后推"那个 bug 的直接探针。
	g.RunOnce(context.Background())
	again := h.item(t, id)
	if again.ExpiresAt == nil || *again.ExpiresAt != want {
		t.Fatalf("第二轮之后 expires_at = %v，想要仍然是 %d（TTL 被当成 now 基准往后推了）",
			derefI64(again.ExpiresAt), want)
	}
}

func derefI64(p *int64) string {
	if p == nil {
		return "nil"
	}
	return time.Unix(*p, 0).UTC().Format(time.RFC3339)
}

func derefStr(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}

// ── 步骤 1：置顶保护 ───────────────────────────────────────────

func TestGC_PinnedNeverExpires(t *testing.T) {
	h := newHarness(t)
	h.cfg.DefaultTTLSec = 86400
	past := time.Now().Unix() - 3600

	// 故意造出"置顶 + 已过期"这个理论不该存在的组合（用户先设了过期、
	// 之后才置顶；或者手改过库）。GC 必须把它救回来。
	id := h.put(t, store.Item{
		Pinned:    true,
		ExpiresAt: &past,
		Preview:   "收藏的东西",
	})

	rep := h.gc(t).RunOnce(context.Background())
	if rep.Protected != 1 {
		t.Fatalf("Protected = %d，想要 1", rep.Protected)
	}
	it := h.item(t, id)
	if it.ExpiresAt != nil {
		t.Fatalf("置顶条目的 expires_at 应被清空，实际 = %s", derefI64(it.ExpiresAt))
	}
	if it.TTLSource == nil || *it.TTLSource != store.TTLSourceNever {
		t.Fatalf("ttl_source = %v，想要 never", derefStr(it.TTLSource))
	}
	if h.countAlive(t) != 1 {
		t.Fatal("置顶条目被回收了")
	}
	// 关键：置顶条目也不能因为过期而进回收站。
	if h.countTrashed(t) != 0 {
		t.Fatalf("回收站里有 %d 条，置顶条目不该进去", h.countTrashed(t))
	}
}

// 置顶还要挡住"条数淘汰"：maxItems 很小的时候，置顶那几条必须活下来。
func TestGC_PinnedSurvivesCountEviction(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxItems = 2
	h.cfg.DefaultTTLSec = 365 * 86400

	pinned := h.put(t, store.Item{Pinned: true, Preview: "钉住"})
	for i := 0; i < 5; i++ {
		h.put(t, store.Item{Preview: "普通", FirstSeenAt: time.Now().Unix() - int64(i+1)})
	}

	h.gc(t).RunOnce(context.Background())

	if h.item(t, pinned) == nil {
		t.Fatal("置顶条目被淘汰了")
	}
	if got := h.countAlive(t); got != 2 {
		t.Fatalf("存活 = %d，想要 2（淘汰到 maxItems）", got)
	}
	// 淘汰必须是软删除：用户还能从回收站捞回来。
	if got := h.countTrashed(t); got != 4 {
		t.Fatalf("回收站 = %d，想要 4（淘汰=进回收站，不是物理删除）", got)
	}
}

// ── 步骤 2：三种到期行为 ───────────────────────────────────────

func TestGC_OnExpireModes(t *testing.T) {
	past := time.Now().Unix() - 10

	seed := func(t *testing.T, h *harness, n int) {
		for i := 0; i < n; i++ {
			h.put(t, store.Item{ExpiresAt: &past, TTLSource: ptrStr(store.TTLSourceItem), Preview: "过期了"})
		}
	}

	t.Run("trash", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.OnExpire = store.OnExpireTrash
		seed(t, h, 3)

		rep := h.gc(t).RunOnce(context.Background())
		if rep.Trashed != 3 {
			t.Fatalf("Trashed = %d，想要 3", rep.Trashed)
		}
		if h.countAlive(t) != 0 || h.countTrashed(t) != 3 || h.countAll(t) != 3 {
			t.Fatalf("alive=%d trashed=%d all=%d，想要 0/3/3",
				h.countAlive(t), h.countTrashed(t), h.countAll(t))
		}
	})

	t.Run("delete", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.OnExpire = store.OnExpireDelete
		seed(t, h, 3)

		rep := h.gc(t).RunOnce(context.Background())
		if rep.Deleted != 3 {
			t.Fatalf("Deleted = %d，想要 3", rep.Deleted)
		}
		if h.countAll(t) != 0 {
			t.Fatalf("all = %d，delete 模式应物理删除", h.countAll(t))
		}
	})

	t.Run("archive", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.OnExpire = store.OnExpireArchive
		seed(t, h, 2)

		rep := h.gc(t).RunOnce(context.Background())
		if rep.Archived != 2 {
			t.Fatalf("Archived = %d，想要 2", rep.Archived)
		}
		if h.countAlive(t) != 2 {
			t.Fatal("archive 模式不该让条目消失")
		}
		// 归档 = 清掉过期时间并标记 never，之后再跑 GC 也不能回收它。
		for _, it := range h.allItems(t) {
			if it.ExpiresAt != nil {
				t.Fatalf("归档条目的 expires_at 应为 NULL，实际 %s", derefI64(it.ExpiresAt))
			}
			if it.TTLSource == nil || *it.TTLSource != store.TTLSourceNever {
				t.Fatalf("归档条目的 ttl_source 应为 never，实际 %v", derefStr(it.TTLSource))
			}
		}
		rep2 := h.gc(t).RunOnce(context.Background())
		if rep2.Trashed+rep2.Deleted+rep2.Archived != 0 {
			t.Fatalf("归档之后再跑一轮又处理了 %d 条——归档没生效",
				rep2.Trashed+rep2.Deleted+rep2.Archived)
		}
	})

	t.Run("archive 带分类搬家", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.OnExpire = store.OnExpireArchive
		catID, err := h.db.CreateCategory(context.Background(), &store.Category{Name: "长期归档"})
		if err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		h.cfg.ArchiveCategoryID = catID
		seed(t, h, 2)

		if rep := h.gc(t).RunOnce(context.Background()); rep.Archived != 2 {
			t.Fatalf("Archived = %d，想要 2", rep.Archived)
		}
		for _, it := range h.allItems(t) {
			if it.CategoryID == nil || *it.CategoryID != catID {
				t.Fatalf("归档条目没进分类：category_id = %v，想要 %d", it.CategoryID, catID)
			}
		}
	})
}

// allItems 返回库里的全部条目（含回收站）。
//
// 直接查表而不是走 db.List：Query 没有"含回收站的全量"这一档
// （只有 Trashed 开关），而本文件好几处要的是"一条不漏"。
func (h *harness) allItems(t *testing.T) []store.Item {
	t.Helper()
	rows, err := h.db.Writer().Query("SELECT id FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("查询全部 id: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 id: %v", err)
	}
	out := make([]store.Item, 0, len(ids))
	for _, id := range ids {
		if it := h.item(t, id); it != nil {
			out = append(out, *it)
		}
	}
	return out
}

// ── 步骤 3：硬删回收站 ─────────────────────────────────────────

func TestGC_PurgeTrashAfterTTL(t *testing.T) {
	h := newHarness(t)
	h.cfg.TrashTTLSec = 7 * 86400
	now := time.Now().Unix()

	old := h.put(t, store.Item{Preview: "躺了 8 天"})
	fresh := h.put(t, store.Item{Preview: "昨天删的"})

	if _, err := h.db.SoftDelete(context.Background(), []int64{old}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := h.db.SoftDelete(context.Background(), []int64{fresh}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	// 把 old 的 deleted_at 手动推到 8 天前。
	if _, err := h.db.Writer().Exec(
		"UPDATE items SET deleted_at = ? WHERE id = ?", now-8*86400, old); err != nil {
		t.Fatalf("backdate deleted_at: %v", err)
	}

	rep := h.gc(t).RunOnce(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("Purged = %d，想要 1（只有躺够 7 天的那条该被硬删）", rep.Purged)
	}
	if h.countAll(t) != 1 {
		t.Fatalf("all = %d，想要 1", h.countAll(t))
	}
	if it := h.item(t, fresh); it == nil || it.DeletedAt == nil {
		t.Fatal("昨天删的那条不该被硬删")
	}
}

// ── 步骤 4/5：淘汰 ─────────────────────────────────────────────

// 条数淘汰要按"最久没用过"挑，而不是按创建时间或 id。
func TestGC_CountEvictionPicksLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxItems = 3
	h.cfg.DefaultTTLSec = 365 * 86400
	now := time.Now().Unix()

	// last_used_at 越早越该先走。
	fresh := h.put(t, store.Item{Preview: "刚用过", LastUsedAt: ptrI64(now - 10)})
	newer := h.put(t, store.Item{Preview: "前几天用过", LastUsedAt: ptrI64(now - 5*86400)})
	older := h.put(t, store.Item{Preview: "很久没用", LastUsedAt: ptrI64(now - 30*86400)})
	oldest := h.put(t, store.Item{Preview: "最久没用", LastUsedAt: ptrI64(now - 90*86400)})

	if got := h.countAlive(t); got != 4 {
		t.Fatalf("前置条件：存活 %d，想要 4", got)
	}
	h.gc(t).RunOnce(context.Background())

	if got := h.countAlive(t); got != 3 {
		t.Fatalf("存活 = %d，想要 3", got)
	}
	if h.item(t, oldest).DeletedAt == nil {
		t.Fatal("最久没用过的那条应被淘汰")
	}
	// 其余三条必须都还在——淘汰"多挑了一条"是最容易犯的 off-by-one。
	for _, id := range []int64{fresh, newer, older} {
		if it := h.item(t, id); it.DeletedAt != nil {
			t.Fatalf("条目 %d 被误淘汰（应只淘汰 1 条）", id)
		}
	}
}

// 从未被取用过的条目（last_used_at IS NULL）应该**先**走，
// 而不是因为 NULL 排到最后而永远留着。
func TestGC_CountEvictionTreatsNeverUsedAsOldest(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxItems = 1
	h.cfg.DefaultTTLSec = 365 * 86400
	now := time.Now().Unix()

	// 关键构造：让两条的排序键**不同**，否则分不出对错。
	//   used  : last_used_at = now            → COALESCE = now
	//   never : last_used_at = NULL,
	//           first_seen_at = now - 3600    → COALESCE = now-3600（更旧，该先走）
	// 若实现把 NULL 当成"最大/最新"，先走的会变成 used —— 判定随之翻转。
	used := h.put(t, store.Item{Preview: "刚用过", LastUsedAt: ptrI64(now)})
	never := h.put(t, store.Item{Preview: "从没用过", FirstSeenAt: now - 3600, LastUsedAt: nil})

	h.gc(t).RunOnce(context.Background())

	if it := h.item(t, never); it.DeletedAt == nil {
		t.Fatal("从没用过的条目应优先淘汰（COALESCE(last_used_at, first_seen_at) 没生效？）")
	}
	if it := h.item(t, used); it.DeletedAt != nil {
		t.Fatal("刚用过的条目被误淘汰")
	}
}

// 容量淘汰要挑占地方最大的。
func TestGC_DiskEvictionPicksLargest(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxItems = 0 // 关掉条数淘汰，隔离出容量这条路径
	h.cfg.DefaultTTLSec = 365 * 86400
	h.cfg.MaxDiskBytes = 1000 // 很小，逼出淘汰

	big := h.put(t, store.Item{Kind: "image", ByteSize: 5000, Preview: "大图"})
	mid := h.put(t, store.Item{Kind: "image", ByteSize: 800, Preview: "中图"})
	small := h.put(t, store.Item{Kind: "text", ByteSize: 100, Preview: "小文本"})

	before, err := h.db.TotalAliveBytes(context.Background())
	if err != nil {
		t.Fatalf("TotalAliveBytes: %v", err)
	}
	if before <= h.cfg.MaxDiskBytes {
		t.Fatalf("前置条件不满足：占用 %d 未超过上限 %d", before, h.cfg.MaxDiskBytes)
	}

	rep := h.gc(t).RunOnce(context.Background())
	if rep.Evicted < 1 {
		t.Fatalf("Evicted = %d，想要至少 1", rep.Evicted)
	}
	if h.item(t, big).DeletedAt == nil {
		t.Fatal("最大的图片应被容量淘汰优先挑走")
	}
	if it := h.item(t, small); it.DeletedAt != nil {
		t.Fatal("小文本不该被优先淘汰（容量淘汰优先大体积）")
	}
	if h.item(t, mid) == nil {
		t.Fatal("中图被误删？")
	}
}

// ── 步骤 6：孤儿扫描 ───────────────────────────────────────────

// 这一条的三个子用例，每一个都对应一种"删错文件"的真实路径。
func TestGC_OrphanSweep(t *testing.T) {
	t.Run("年轻孤儿不动", func(t *testing.T) {
		h := newHarness(t)
		h.put(t, store.Item{Preview: "有个条目，免得触发空库保护"})

		// 刚落盘、还没有条目引用的文件——正是"先落文件后写行"窗口的样子。
		rel, _, err := h.blobs.Put([]byte("fresh-bytes"), "png")
		if err != nil {
			t.Fatalf("blobs.Put: %v", err)
		}

		rep := h.gc(t).RunOnce(context.Background())
		if rep.OrphansRemoved != 0 {
			t.Fatalf("OrphansRemoved = %d，想要 0——年龄门槛没起作用，会删掉正在写入的文件",
				rep.OrphansRemoved)
		}
		if !h.blobs.Exists(rel) {
			t.Fatal("刚写的 blob 被删了")
		}
	})

	t.Run("老孤儿要删", func(t *testing.T) {
		h := newHarness(t)
		h.put(t, store.Item{Preview: "条目"})

		rel, _, err := h.blobs.Put([]byte("stale-bytes"), "png")
		if err != nil {
			t.Fatalf("blobs.Put: %v", err)
		}
		// 把 mtime 推到 2 天前。
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(h.blobs.Abs(rel), old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		rep := h.gc(t).RunOnce(context.Background())
		if rep.OrphansRemoved != 1 {
			t.Fatalf("OrphansRemoved = %d，想要 1", rep.OrphansRemoved)
		}
		if h.blobs.Exists(rel) {
			t.Fatal("老孤儿没被删掉")
		}
	})

	t.Run("被回收站引用的不能删", func(t *testing.T) {
		h := newHarness(t)
		rel, sha, err := h.blobs.Put([]byte("trashed-image"), "png")
		if err != nil {
			t.Fatalf("blobs.Put: %v", err)
		}
		id := h.put(t, store.Item{Kind: "image", ImagePath: &rel, ByteSize: 13, Preview: "进回收站了"})
		if _, err := h.db.SoftDelete(context.Background(), []int64{id}); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
		_ = sha

		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(h.blobs.Abs(rel), old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		if rep := h.gc(t).RunOnce(context.Background()); rep.OrphansRemoved != 0 {
			t.Fatalf("OrphansRemoved = %d，想要 0——回收站条目的 blob 被当孤儿删了，"+
				"用户恢复出来是一张空图", rep.OrphansRemoved)
		}
		if !h.blobs.Exists(rel) {
			t.Fatal("回收站条目的 blob 被删了")
		}
	})

	t.Run("空库时整步跳过", func(t *testing.T) {
		h := newHarness(t)
		// 什么都不插 —— 模拟"数据库路径被改到新位置"这类事故。
		rel, _, err := h.blobs.Put([]byte("orphan-from-a-former-life"), "png")
		if err != nil {
			t.Fatalf("blobs.Put: %v", err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(h.blobs.Abs(rel), old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		rep := h.gc(t).RunOnce(context.Background())
		if rep.OrphansRemoved != 0 {
			t.Fatalf("OrphansRemoved = %d，想要 0——items 表为空时必须跳过孤儿扫描，"+
				"否则换库路径会一次清空所有图片", rep.OrphansRemoved)
		}
		if !h.blobs.Exists(rel) {
			t.Fatal("空库保护没生效，blob 被删了")
		}
	})
}

// ── 暂停 ──────────────────────────────────────────────────────

func TestGC_PauseSkipsRound(t *testing.T) {
	h := newHarness(t)
	h.cfg.DefaultTTLSec = 86400
	past := time.Now().Unix() - 10
	h.put(t, store.Item{ExpiresAt: &past, TTLSource: ptrStr(store.TTLSourceItem), Preview: "该过期了"})

	g := h.gc(t)
	g.Pause()
	if !g.Paused() {
		t.Fatal("Pause 之后 Paused() 应为 true")
	}
	rep := g.RunOnce(context.Background())
	if rep.Trashed != 0 || rep.TTLApplied != 0 {
		t.Fatalf("暂停期间不该动数据：%+v", rep)
	}
	if h.countAlive(t) != 1 {
		t.Fatal("暂停期间条目被回收了")
	}

	g.Resume()
	if g.Paused() {
		t.Fatal("Resume 之后 Paused() 应为 false")
	}
	rep = g.RunOnce(context.Background())
	if rep.Trashed != 1 {
		t.Fatalf("恢复后应正常回收，Trashed = %d", rep.Trashed)
	}
}

// 嵌套暂停：导出与导入重叠时，一次 Resume 不能把 GC 提前放开。
func TestGC_NestedPause(t *testing.T) {
	h := newHarness(t)
	g := h.gc(t)
	g.Pause() // 导出开始
	g.Pause() // 导入开始（模拟重叠）
	g.Resume()
	if !g.Paused() {
		t.Fatal("还有一层没 Resume，不该解除暂停")
	}
	g.Resume()
	if g.Paused() {
		t.Fatal("两层都 Resume 之后应解除暂停")
	}
	// 多 Resume 一次不该把计数弄成负数、更不该 panic。
	g.Resume()
	if g.Paused() {
		t.Fatal("多余的 Resume 之后仍应是未暂停态")
	}
}

// ── 收尾与报告 ────────────────────────────────────────────────

func TestGC_VacuumOnlyAfterPhysicalDelete(t *testing.T) {
	h := newHarness(t)
	h.cfg.VacuumAfterDelete = true
	h.cfg.TrashTTLSec = 86400

	// 没有物理删除的一轮：不该 VACUUM。
	h.put(t, store.Item{Preview: "活着"})
	rep := h.gc(t).RunOnce(context.Background())
	if rep.Vacuumed {
		t.Fatal("没有物理删除却跑了 VACUUM（每轮全库重写，代价很大）")
	}

	// 造一条躺够时间的回收站条目，这轮就该 VACUUM。
	id := h.put(t, store.Item{Preview: "该被硬删"})
	if _, err := h.db.SoftDelete(context.Background(), []int64{id}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := h.db.Writer().Exec("UPDATE items SET deleted_at = ? WHERE id = ?",
		time.Now().Unix()-3*86400, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	rep = h.gc(t).RunOnce(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("Purged = %d，想要 1", rep.Purged)
	}
	if !rep.Vacuumed {
		t.Fatal("物理删除之后应跑 VACUUM")
	}
}

func TestGC_ReportSummaryNonEmpty(t *testing.T) {
	h := newHarness(t)
	rep := h.gc(t).RunOnce(context.Background())
	if s := rep.Summary(); s == "" {
		t.Fatal("Summary() 返回空串")
	}
	if last := h.gc(t).Last(); last != nil {
		t.Fatal("新实例的 Last() 应为 nil")
	}
	g := h.gc(t)
	g.RunOnce(context.Background())
	if g.Last() == nil {
		t.Fatal("跑过一轮之后 Last() 不该是 nil")
	}
	if g.Runs() != 1 {
		t.Fatalf("Runs() = %d，想要 1", g.Runs())
	}
}

// GC 的一整轮不该有任何非致命错误攒进 Report.Errors。
// 这条是"配置组合跑一遍"的冒烟测试：上限全开、三种模式各跑一次。
func TestGC_NoErrorsAcrossConfigCombos(t *testing.T) {
	modes := []string{store.OnExpireTrash, store.OnExpireDelete, store.OnExpireArchive}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.cfg = Config{
				DefaultTTLSec:     86400,
				OnExpire:          mode,
				TrashTTLSec:       3600,
				MaxItems:          50,
				MaxDiskBytes:      10 << 20,
				GCIntervalSec:     60,
				VacuumAfterDelete: true,
			}
			for i := 0; i < 5; i++ {
				h.put(t, store.Item{Kind: "image", ByteSize: 1000, Preview: "内容"})
			}
			rep := h.gc(t).RunOnce(context.Background())
			if len(rep.Errors) > 0 {
				t.Fatalf("有错误：%v", rep.Errors)
			}
		})
	}
}

// 周期循环能被 Stop 干净地停掉（不泄漏 goroutine、不 panic）。
func TestGC_StartStopIsIdempotentAndClean(t *testing.T) {
	h := newHarness(t)
	h.cfg.GCIntervalSec = 1
	g := h.gc(t)
	g.Start()
	g.Start() // 第二次调用应是空操作
	time.Sleep(50 * time.Millisecond)
	g.Stop()

	// 未 Start 就 Stop 也不能炸。
	g2 := h.gc(t)
	g2.Stop()
}

// 间隔非法（0 / 负数）时回退到默认，不能变成"忙循环"。
func TestGC_ConfigWithDefaults(t *testing.T) {
	zero := Config{}
	got := zero.withDefaults()
	def := DefaultConfig()
	if got.GCIntervalSec != def.GCIntervalSec || got.OnExpire != def.OnExpire ||
		got.TrashTTLSec != def.TrashTTLSec || got.DefaultTTLSec != def.DefaultTTLSec {
		t.Fatalf("零值补齐不对：%+v", got)
	}

	// MaxItems / MaxDiskBytes 的 0 是"不限"，不能被补成默认值。
	if got.MaxItems != 0 || got.MaxDiskBytes != 0 {
		t.Fatalf("0 应解释为不限，实际 %d / %d", got.MaxItems, got.MaxDiskBytes)
	}

	// 非法的 onExpire 要退回 trash（默认），不能原样传下去变成"什么都不做"。
	bad := Config{OnExpire: "explode", GCIntervalSec: -5}
	got = bad.withDefaults()
	if got.OnExpire != store.OnExpireTrash {
		t.Fatalf("非法 onExpire 应回退为 trash，实际 %q", got.OnExpire)
	}
	if got.GCIntervalSec <= 0 {
		t.Fatalf("非法间隔应回退为正数，实际 %d", got.GCIntervalSec)
	}
}

var _ = filepath.Join
