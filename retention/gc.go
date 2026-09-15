// Package retention 实现 DESIGN.md §5 的过期与生命周期管理。
//
// 它只做一件事：按固定顺序跑完 §5.2 那张流程表的每一步，并产出一份可读的
// 报告。所有数据库细节都在 store 包里，这里只负责**顺序**与**策略**。
//
// 顺序为什么不能改（每一步都依赖前一步的清理结果）：
//
//  0. 计算 TTL   没有过期时间的条目先算出 expires_at（§5.1 的③级）
//  1. 保护       置顶的钉成"永不"
//  2. 到期回收   按 onExpire 处理——软删除是默认，也是绝不能省的那一步
//  3. 硬删       回收站里躺够 trashTtlSec 的才真删
//  4. 条数淘汰   超 maxItems 的按"最久没用过"走
//  5. 容量淘汰   超 maxDiskBytes 的按"体积最大"走
//  6. 一致性     VACUUM + 孤儿 blob 扫描
//
// 把 0 放在 1 之前是刻意的：万一某条被置顶的条目在上一轮留下了
// expires_at，步骤 1 会把它清掉；而如果先算 TTL 再保护，那步算出来的
// 过期时间同样会被步骤 1 清掉，两种顺序结果一样。放前面只是让
// "本轮新算出的 TTL"也能被同一轮的保护步骤兜住。
package retention

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/store"
)

// imageKinds 是"大体积"的那几类条目，容量淘汰优先从它们里挑。
//
// 值直接取自 clipboard 包的常量——那才是 items.kind 的权威定义域
// （capture 就是用它算完写进库的）。这里刻意不写字面量，
// 免得将来 kind 取值改了、这里悄悄漏掉，表现为"容量满了也不淘汰图片"。
var imageKinds = []string{clipboard.KindImage, clipboard.KindMixed}

// OrphanMinAgeSec 是孤儿 blob 的"最短年龄"（§5.2 第 6 步：mtime > 1 天）。
//
// 为什么必须有这个门槛：捕获写入是"先落 blob 文件、再写数据库行"，
// 中间有一个很短的窗口里文件存在但没有行引用它。没有年龄门槛的话，
// 恰好在这一瞬间跑到的 GC 会把刚写好的图片删掉——用户看到的是
// "图片偶尔丢"，而且极难复现。
const OrphanMinAgeSec int64 = 86400

// Config 是本轮 GC 的策略参数，取自 settings 表（§9）。
type Config struct {
	DefaultTTLSec int64 // retention.defaultTtlSec
	OnExpire      string
	TrashTTLSec   int64 // retention.trashTtlSec
	MaxItems      int   // retention.maxItems
	MaxDiskBytes  int64 // retention.maxDiskBytes
	GCIntervalSec int   // retention.gcIntervalSec

	// ArchiveCategoryID 是 onExpire=archive 时要移入的分类。
	//
	// ⚠️ 文档缺口：§5.2 第 2 步写了"移入**指定**分类"，但 §9 的设置表里
	// 没有任何 key 能指定是哪个分类。所以这里留一个有默认值 0（不搬）的
	// 出口，行为是"保留内容 + 清掉 expires_at"，分类归属保持原样。
	// 要真正启用搬家，需要一个 settings key，等确认后补。
	ArchiveCategoryID int64

	// VacuumAfterDelete 控制第 6 步是否真的跑 VACUUM。
	//
	// VACUUM 会全库重写一遍，万级库要几百毫秒。只在"本轮真删过行"时才值得跑，
	// 所以 RunOnce 里还会再判断一次；把它设成 false 可以整个关掉。
	VacuumAfterDelete bool
}

// DefaultConfig 返回 §9 的默认值（与 store.DefaultSettings 同源，避免两处漂移）。
func DefaultConfig() Config {
	s := store.DefaultSettings()
	return Config{
		DefaultTTLSec:     s.Retention.DefaultTTLSec,
		OnExpire:          s.Retention.OnExpire,
		TrashTTLSec:       s.Retention.TrashTTLSec,
		MaxItems:          s.Retention.MaxItems,
		MaxDiskBytes:      s.Retention.MaxDiskBytes,
		GCIntervalSec:     s.Retention.GCIntervalSec,
		VacuumAfterDelete: true,
	}
}

// withDefaults 把零值补成合理值。
//
// 注意 MaxItems / MaxDiskBytes 的 0 一律解释为"不限"，而不是"补成默认"——
// 用户把上限设成 0 来表达"我不想被淘汰"，是个合理的意图，不该被我们覆盖。
func (c Config) withDefaults() Config {
	out := c
	def := DefaultConfig()
	if out.DefaultTTLSec == 0 {
		out.DefaultTTLSec = def.DefaultTTLSec
	}
	if out.OnExpire == "" {
		out.OnExpire = def.OnExpire
	}
	switch out.OnExpire {
	case store.OnExpireTrash, store.OnExpireDelete, store.OnExpireArchive:
	default:
		out.OnExpire = store.OnExpireTrash
	}
	if out.TrashTTLSec == 0 {
		out.TrashTTLSec = def.TrashTTLSec
	}
	if out.GCIntervalSec <= 0 {
		out.GCIntervalSec = def.GCIntervalSec
	}
	return out
}

// Report 是一轮 GC 的结果。
//
// 它有两个用途：写日志，以及给统计面板展示"上次清理干了什么"。
// Errors 是**非致命**错误的集合——某一步失败不该让整轮中止，
// 否则一次权限问题就会让所有后续清理永远停下。
type Report struct {
	StartedAt  time.Time `json:"startedAt"`
	TookMs     int64     `json:"tookMs"`
	TTLApplied int64     `json:"ttlApplied"`
	Protected  int64     `json:"protected"`
	Trashed    int64     `json:"trashed"`
	Deleted    int64     `json:"deleted"`
	Archived   int64     `json:"archived"`
	Purged     int64     `json:"purged"`
	Evicted    int64     `json:"evicted"`
	// EvictedBytes 是第 4/5 步淘汰掉的条目的 byte_size 合计（估算，不是实测文件大小）。
	EvictedBytes   int64 `json:"evictedBytes"`
	OrphansRemoved int64 `json:"orphansRemoved"`
	OrphanBytes    int64 `json:"orphanBytes"`
	Vacuumed       bool  `json:"vacuumed"`

	// Alive / TotalBytes 是收尾时的库状态，便于观察趋势。
	Alive      int64 `json:"alive"`
	TotalBytes int64 `json:"totalBytes"`

	Errors []string `json:"errors,omitempty"`
}

// Summary 给日志与 UI 用的一句话概括。
func (r *Report) Summary() string {
	if r == nil {
		return "GC: 尚未运行"
	}
	return "GC: 到期 " + itoa(r.Trashed+r.Deleted+r.Archived) +
		"（回收站 " + itoa(r.Trashed) + " / 删除 " + itoa(r.Deleted) + " / 归档 " + itoa(r.Archived) + "）" +
		"，硬删 " + itoa(r.Purged) +
		"，淘汰 " + itoa(r.Evicted) +
		"，孤儿 " + itoa(r.OrphansRemoved) +
		"，存活 " + itoa(r.Alive) +
		"，耗时 " + itoa(r.TookMs) + "ms"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// GC 是生命周期管理器。
type GC struct {
	db    *store.DB
	blobs *store.BlobStore
	log   *slog.Logger

	// cfg 每轮重新取一次，这样设置界面改完 TTL 上限立刻生效，
	// 不需要重启 App（§9 的运行时设置真源是 settings 表）。
	cfg func() Config

	stop chan struct{}
	done chan struct{}
	once sync.Once

	// pauseDepth 是"暂停嵌套计数"。导出与导入都会暂停 GC，
	// 用计数而不是布尔可以让两者重叠时不会互相把对方放开。
	pauseDepth atomic.Int64
	pausedFlag atomic.Bool

	last atomic.Pointer[Report]

	// runs 是累计执行轮数，供测试与诊断。
	runs atomic.Int64
}

// New 构造 GC。cfg 为 nil 时用 §9 默认值。
func New(db *store.DB, blobs *store.BlobStore, cfg func() Config, log *slog.Logger) (*GC, error) {
	if db == nil {
		return nil, errors.New("retention: 需要 store.DB")
	}
	if blobs == nil {
		return nil, errors.New("retention: 需要 BlobStore")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg == nil {
		cfg = DefaultConfig
	}
	return &GC{db: db, blobs: blobs, log: log, cfg: cfg}, nil
}

// Start 启动周期任务。重复调用是幂等的。
func (g *GC) Start() {
	g.once.Do(func() {
		g.stop = make(chan struct{})
		g.done = make(chan struct{})
		go g.loop()
	})
}

// Stop 停止周期任务并等待当前这轮跑完。
func (g *GC) Stop() {
	if g.stop == nil {
		return
	}
	close(g.stop)
	<-g.done
}

// Pause 暂停 GC（导出/导入期间用）。与 Resume 必须成对。
func (g *GC) Pause() {
	g.pauseDepth.Add(1)
	g.pausedFlag.Store(true)
}

// Resume 解除一次暂停。
func (g *GC) Resume() {
	if g.pauseDepth.Add(-1) <= 0 {
		g.pauseDepth.Store(0)
		g.pausedFlag.Store(false)
	}
}

// Paused 报告当前是否暂停。
func (g *GC) Paused() bool { return g.pausedFlag.Load() }

// Last 返回上一轮报告（可能为 nil）。
func (g *GC) Last() *Report { return g.last.Load() }

// Runs 返回累计执行轮数。
func (g *GC) Runs() int64 { return g.runs.Load() }

func (g *GC) loop() {
	defer close(g.done)
	for {
		// 每轮都重新读间隔：设置为 0/负值时退到默认（withDefaults 已处理）。
		interval := time.Duration(g.cfg().withDefaults().GCIntervalSec) * time.Second
		if interval <= 0 {
			interval = time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-g.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if g.Paused() {
			continue
		}
		// 用带超时的 context：某一轮卡住（例如 VACUUM 遇到大库）时，
		// 至少不会永远占着写句柄不放。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		rep := g.RunOnce(ctx)
		cancel()
		if len(rep.Errors) > 0 {
			g.log.Warn("GC 完成但有错误", "summary", rep.Summary(), "errors", rep.Errors)
		} else {
			g.log.Debug("GC 完成", "summary", rep.Summary())
		}
	}
}

// stepErr 把某一步的错误记进报告而不是中断整轮。
func (r *Report) stepErr(step string, err error) {
	if err == nil {
		return
	}
	r.Errors = append(r.Errors, step+": "+err.Error())
}

// RunOnce 跑完一整轮 GC 并返回报告。
//
// 它**不返回 error**：每一步的失败都进 Report.Errors，因为保留策略是
// 尽力而为的维护任务——一次失败不该让调用方（周期循环）退出。
func (g *GC) RunOnce(ctx context.Context) *Report {
	start := time.Now()
	g.runs.Add(1)

	cfg := g.cfg().withDefaults()
	rep := &Report{StartedAt: start}

	// 暂停时直接返回空报告，但**不改变** Last()：暂停期间的"上次报告"
	// 仍然是上一次真正跑过的结果，UI 不该被清空。
	if g.Paused() {
		rep.TookMs = time.Since(start).Milliseconds()
		rep.Errors = append(rep.Errors, "GC 已暂停（导出/导入进行中），本轮跳过")
		return rep
	}

	// ── 步骤 0：按三级优先级算出还没算过的 expires_at ──────────────
	//
	// ①级（items.expires_at 显式）与②级（categories.ttl_seconds）在写入
	// 时就已经落到 expires_at 上了（见 SetExpiry / RecalcCategoryTTL），
	// 这里只补③级（全局默认）。
	n, err := g.db.ApplyDefaultTTL(ctx, cfg.DefaultTTLSec)
	rep.stepErr("计算默认 TTL", err)
	rep.TTLApplied = n

	// ── 步骤 1：保护 ──────────────────────────────────────────────
	n, err = g.db.ProtectPinned(ctx)
	rep.stepErr("保护置顶条目", err)
	rep.Protected = n

	// ── 步骤 2：到期回收 ─────────────────────────────────────────
	//
	// archive 模式要"移入指定分类"，这个搬家必须和"清掉 expires_at"
	// 在同一条 SQL 里完成——分两条的话，中间崩溃会留下一批
	// "已经永不回收、但没进归档分类"的条目，用户再也找不到它们。
	var archiveCat *int64
	if cfg.ArchiveCategoryID > 0 {
		id := cfg.ArchiveCategoryID
		archiveCat = &id
	}
	trashed, deleted, archived, err := g.db.ClearExpired(ctx, start.Unix(), cfg.OnExpire, archiveCat)
	rep.stepErr("到期回收", err)
	rep.Trashed, rep.Deleted, rep.Archived = trashed, deleted, archived

	// ── 步骤 3：硬删回收站里躺够时间的 ────────────────────────────
	if cfg.TrashTTLSec > 0 {
		n, err = g.db.PurgeTrashedBefore(ctx, start.Unix()-cfg.TrashTTLSec)
		rep.stepErr("硬删回收站", err)
		rep.Purged = n
	}

	// ── 步骤 4：条数淘汰 ─────────────────────────────────────────
	var evictedIDs []int64
	if cfg.MaxItems > 0 {
		alive, err := g.db.CountAlive(ctx)
		if err != nil {
			rep.stepErr("统计存活条数", err)
		} else if over := int(alive) - cfg.MaxItems; over > 0 {
			ids, err := g.db.EvictIDs(ctx, over, nil)
			rep.stepErr("挑选条数淘汰目标", err)
			if len(ids) > 0 {
				// 条数淘汰也是**软删除**：淘汰不等于"我不想要了"，
				// 而是"太旧了"。用户仍应能在回收站里把它捞回来。
				if n, err := g.db.SoftDelete(ctx, ids); err != nil {
					rep.stepErr("条数淘汰", err)
				} else {
					rep.Evicted += n
					evictedIDs = append(evictedIDs, ids...)
				}
			}
		}
	}

	// ── 步骤 5：容量淘汰 ─────────────────────────────────────────
	if cfg.MaxDiskBytes > 0 {
		total, err := g.db.TotalAliveBytes(ctx)
		if err != nil {
			rep.stepErr("统计占用", err)
		} else if total > cfg.MaxDiskBytes {
			// 先按体积挑（§5.2 第 5 步"优先淘汰大体积图片"），
			// 一次挑 64 条，然后看够不够；不够再按"最久没用过"补一批。
			// 反复全量挑会很慢，所以先粗算需要腾多少条：
			// 用平均条大小估算，再取 2 倍余量。
			alive, _ := g.db.CountAlive(ctx)
			var avg int64 = 1
			if alive > 0 {
				avg = total / alive
				if avg < 1 {
					avg = 1
				}
			}
			need := int((total - cfg.MaxDiskBytes) / avg)
			if need < 1 {
				need = 1
			}
			need = need*2 + 8 // 余量：估算不准时多挑一点，宁可多清也不白跑
			if need > 2000 {
				need = 2000
			}

			ids, err := g.db.EvictLargestIDs(ctx, need, imageKinds, evictedIDs)
			rep.stepErr("挑选容量淘汰目标", err)
			if len(ids) > 0 {
				sizes := make([]int64, 0, len(ids))
				for _, id := range ids {
					if it, err := g.db.GetByID(ctx, id); err == nil {
						sizes = append(sizes, it.ByteSize)
					}
				}
				if n, err := g.db.SoftDelete(ctx, ids); err != nil {
					rep.stepErr("容量淘汰", err)
				} else {
					rep.Evicted += n
					for _, s := range sizes {
						rep.EvictedBytes += s
					}
				}
			}
		}
	}

	// ── 步骤 6：一致性 ───────────────────────────────────────────
	removed, bytes, err := g.sweepOrphans(ctx, start)
	rep.stepErr("孤儿 blob 扫描", err)
	rep.OrphansRemoved, rep.OrphanBytes = removed, bytes

	// VACUUM 只在真删过行时才跑（§5.2 第 6 步 + store.Vacuum 的注释）。
	if cfg.VacuumAfterDelete && rep.Purged > 0 {
		rep.stepErr("VACUUM", g.db.Vacuum(ctx))
		rep.Vacuumed = true
	} else {
		// 没有物理删除时也顺手把 WAL 截断一下。§14 第 5 条要求
		// "每 1000 次写入或每小时 TRUNCATE"，这里顺带覆盖了后者。
		rep.stepErr("WAL checkpoint", g.db.Checkpoint())
	}

	// ── 收尾状态 ────────────────────────────────────────────────
	if alive, err := g.db.CountAlive(ctx); err == nil {
		rep.Alive = alive
	}
	if total, err := g.db.TotalAliveBytes(ctx); err == nil {
		rep.TotalBytes = total
	}
	rep.TookMs = time.Since(start).Milliseconds()
	g.last.Store(rep)
	return rep
}

// sweepOrphans 扫描 blobs/ 里没有任何条目引用、且年龄超过一天的文件并删除。
//
// 三条安全阀，缺一不可：
//
//  1. **年龄门槛**（OrphanMinAgeSec）：捕获是"先落文件、后写行"，
//     窗口期内文件没有引用是正常的，不能删。
//  2. **条数前提**：items 表完全为空时**整步跳过**。这是防"数据库换了路径
//     指向一个空库"这种事故的——那种情况下所有 blob 看起来都是孤儿，
//     一次 GC 就能把用户几年的图片清空。宁可留着垃圾也不能赌。
//  3. **引用集合来自全部条目**（含回收站）。回收站里的还能恢复，
//     它的图片不能被当成孤儿。
func (g *GC) sweepOrphans(ctx context.Context, now time.Time) (removed int64, bytes int64, err error) {
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}

	// 安全阀 2。
	total, err := g.db.CountAll(ctx)
	if err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, nil
	}

	refs, err := g.db.AllBlobRefs(ctx)
	if err != nil {
		return 0, 0, err
	}

	cutoff := now.Add(-time.Duration(OrphanMinAgeSec) * time.Second)
	var dirs []string

	walkErr := g.blobs.Walk(func(bi store.BlobInfo) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, ok := refs[bi.Rel]; ok {
			return nil
		}
		// 安全阀 1。
		if bi.ModTime.After(cutoff) {
			return nil
		}
		if err := os.Remove(bi.Abs); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			// 单个文件删不掉不中断整轮扫描。
			g.log.Warn("删除孤儿 blob 失败", "path", bi.Rel, "err", err)
			return nil
		}
		removed++
		bytes += bi.Size
		dirs = append(dirs, filepath.Dir(bi.Abs))
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, context.DeadlineExceeded) {
		return removed, bytes, walkErr
	}

	// 顺手清掉空的二级目录（分片目录被清空后留着也没用）。
	// 只删空目录，永不递归删非空目录。
	seen := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		_ = os.Remove(d) // 非空时必然失败，这正是我们要的
		// 再往上一级也试一次（上一级也是分片目录，空了就没意义）。
		_ = os.Remove(filepath.Dir(d))
	}
	return removed, bytes, nil
}
