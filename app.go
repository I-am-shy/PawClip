package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zego/pawclip/capture"
	"github.com/zego/pawclip/clipboard"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/retention"
	"github.com/zego/pawclip/store"
)

// App 是 Wails 的绑定对象：前端能调用的一切都在这里。
//
// 它同时是**应用生命周期的所有者**（DESIGN.md §2 的四条 goroutine 有三条
// 归它管：监听、捕获、写入；GC 属于 M2 之后的里程碑）。
//
// ⚠️ 构造与初始化是**故意分开**的，这不是洁癖：
//
// Wails 生成前端绑定的方式是**把编译出来的二进制跑一遍**，让运行时反射出
// bound methods 再写出 frontend/wailsjs/。也就是说 `wails build` 会真的执行
// `main()` 一次——但它不会调用 startup（没有窗口、没有 GUI 会话）。
//
// 一开始把 store.Open 放在 NewApp 里，`wails build` 就直接失败了：
//
//	Generating bindings: ERROR ... err="store: read user_version: disk I/O error"
//
// 生成绑定这种纯代码生成的动作不该需要打开数据库，更不该在用户家里落下一个
// clean_shutdown 标记。所以：
//
//	NewApp(boot, …)   只保存配置，零副作用、零磁盘 IO、瞬时返回
//	startup(ctx)      真正开库、建写入器、拉起捕获
//	Shutdown()        按"捕获 → 写入 → 库"逆序收摊
//
// 这样绑定生成跑得飞快，也顺带解决了"只读环境 / 首次运行时 Home 不可写"
// 这类启动脆弱性。
type App struct {
	// ctx 是 Wails 给的上下文，cancel 后 WebView 就没了；不要拿它做长任务。
	ctx context.Context
	// runCtx 是后台链路的上下文，Shutdown 时才取消。
	runCtx context.Context
	cancel context.CancelFunc

	log     *slog.Logger
	boot    BootstrapConfig
	cfgPath string

	// initMu 保护下面这组"startup 之后才存在"的资源。
	initMu  sync.Mutex
	initErr error
	// bootWarn 是启动期"不足以让程序退出、但用户该知道"的一件事
	// （目前只有一种：config.toml 读不了，已改用默认设置）。
	// 见 main.go 里关于"为什么不再直接退出"的说明。
	bootWarn error
	db       *store.DB
	blobs    *store.BlobStore
	writer   *store.Writer
	cap      *capture.Capture
	settings *store.Settings

	// 平台与回写。backend 由回写器共用（同一个剪贴板后端实例，
	// 不新开一个——Windows 上每个后端都要建自己的消息窗口）。
	backend clipboard.Backend
	ctrl    panel.Controller
	wb      *Writeback
	gc      *retention.GC

	// lastActivity 是最后一次用户交互的 Unix 秒。空闲 watcher 读它。
	lastActivity atomic.Int64

	// noPlatformUI 为真时跳过面板接管与托盘安装。
	//
	// 存在的唯一理由是**测试**：Controller.Attach 会等 Wails 的窗口出现，
	// 最长 15 秒（生产环境里这是对的，冷启动建窗口确实可能慢）。而在
	// `go test` 的进程里那个窗口永远不会出现 —— 不跳过的话，每个调用
	// initialize 的用例都要白等 15 秒。真机上跑过一次全量测试时，
	// 三个用例就让它挂到了外层超时（SIGTERM/137）。
	//
	// 面板接管这条链路没有单元测试可写（它全是 AppKit/Win32 的副作用），
	// 靠的是 PanelDiag() 在真机上的取证，见收尾报告。
	noPlatformUI bool

	settingsMu sync.RWMutex
	stopOnce   sync.Once
}

// NewApp 只保存配置。不做任何 IO、不碰磁盘、不启 goroutine。
func NewApp(boot BootstrapConfig, cfgPath string, log *slog.Logger) *App {
	return newApp(boot, cfgPath, log, nil)
}

// newApp 是 NewApp 的完整版本，多一个"启动期警告"。
//
// 为什么不直接给 NewApp 加一个参数：NewApp 被一批测试直接调用，
// 加参数会让它们全部要改，而它们与这件事无关。更重要的是——
// **启动警告只该由 main 产生**（它才知道读配置那一步发生了什么），
// 测试里构造的 App 本来就不该带一个警告。
//
// bootWarn 是一句**已经按当前语言渲染好**的话（同 exportReport 的做法）：
// 前端只负责显示，不负责措辞。
func newApp(boot BootstrapConfig, cfgPath string, log *slog.Logger, bootWarn error) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{
		log:      log,
		boot:     normalizeBootstrap(boot),
		cfgPath:  cfgPath,
		bootWarn: bootWarn,
	}
}

// startup 由 Wails 在窗口与前端就绪后调用。
//
// 初始化顺序不能随意调换：
//
//	store.Open（建表 + 迁移 + FTS 自检 + 打 clean_shutdown 标记）
//	  → SeedSettings（缺键补默认值，只在首启用得上）
//	  → LoadSettings（拿到运行时设置的强类型视图）
//	  → 按设置纠正 clean_shutdown 标记开关
//	  → BlobStore → Writer → Backend → Capture
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.runCtx, a.cancel = context.WithCancel(context.Background())

	if err := a.initialize(a.runCtx); err != nil {
		// 初始化失败不该让窗口打不开——用户至少能看到诊断信息（Health 会把
		// initError 带出去）。M2 会在这里换成明确的错误页。
		a.log.Error("初始化失败，本次运行不会记录任何内容", "err", err)
		a.initMu.Lock()
		a.initErr = err
		a.initMu.Unlock()
		return
	}

	a.log.Info("PawClip 已启动",
		"version", Version,
		"db", a.db.Path(),
		"fts5", a.db.FTSAvailable(),
		"capture.enabled", a.settings.Capture.Enabled,
		"capture.types", a.settings.Capture.Types,
	)
}

// initialize 打开库、建好整条链路并启动它。
func (a *App) initialize(ctx context.Context) error {
	boot := a.boot

	dbPath := boot.Database.Path
	if dbPath == "" {
		p, err := DefaultDatabasePath()
		if err != nil {
			return err
		}
		dbPath = p
	}

	db, err := store.Open(store.Options{Path: dbPath, Logger: a.log})
	if err != nil {
		return err
	}

	a.initMu.Lock()
	a.db = db
	a.initMu.Unlock()

	if err := store.SeedSettings(ctx, db, boot.UI.Language); err != nil {
		return err
	}
	settings, err := store.LoadSettings(ctx, db)
	if err != nil {
		return err
	}

	// settings 表是运行时设置唯一真源，但标记文件的开关必须在"读到设置之前"
	// 先按默认值 true 工作一次（否则漏检一次异常退出）。所以这里是"事后纠正"。
	if err := db.SetCleanShutdownMarkerEnabled(settings.Storage.CleanShutdownMarker); err != nil {
		a.log.Warn("无法应用 storage.cleanShutdownMarker 设置", "err", err)
	}

	blobs, err := store.NewBlobStore(filepath.Join(db.Dir(), "blobs"))
	if err != nil {
		return err
	}

	writer, err := store.NewWriter(db, blobs, store.WriterConfig{
		// storage.walCheckpointEvery ← 每 N 次写入做一次 wal_checkpoint(TRUNCATE)
		CheckpointEvery: settings.Storage.WALCheckpointEvery,
	}, a.log)
	if err != nil {
		return err
	}

	// 平台后端。Linux 上这里会返回 ErrUnsupported——不阻止应用启动，
	// 只是没有捕获能力（HANDOFF-PROMPT §二：Linux 只留接口骨架）。
	backend, err := clipboard.NewBackend(clipboard.BackendConfig{
		PollIntervalActiveMs: settings.Capture.PollIntervalActiveMs,
		PollIntervalIdleMs:   settings.Capture.PollIntervalIdleMs,
		IdleThresholdSec:     settings.Capture.IdleThresholdSec,
	})
	if err != nil {
		return err
	}

	pipeline, err := capture.New(backend, writer, capture.Config{
		Filter:       filterConfigFrom(settings),
		TextMaxChars: settings.Capture.TextMaxChars,
	}, a.log)
	if err != nil {
		return err
	}

	// 面板控制器。Linux 上 panel.New 返回一个降级实现（ErrUnsupported），
	// 所以这里不需要按 GOOS 分叉——平台差异在 panel 包里收敛掉了。
	//
	// ⚠️ noPlatformUI 时**连构造都不做**（而不是构造完不用）：darwin 的
	// 实现里每个方法最后都会走 PawClip_OnMain，也就是
	// `dispatch_sync(main_queue, ...)`。在 `go test` 的进程里主线程没有跑
	// 事件循环，没人 drain 那个队列 —— 于是调用会**永久阻塞**（不是超时，
	// 是死等）。第一次遇到的表现是整个测试进程被外层 SIGTERM 杀掉（exit 137）。
	// 所以这里传下去的是一个**真正的 nil 接口**，而不是一个"功能关掉的实例"，
	// 这样 Writeback 里的 `w.panel != nil` 会走降级分支。
	var ctrl panel.Controller
	if !a.noPlatformUI {
		ctrl = panel.New(&panelHandler{app: a})
	}

	// 回写器。⚠️ guard 必须来自 pipeline.Guard()——**同一个实例**。
	// 新建一个的话两边各记各的期望指纹，谁也拦不住谁，
	// 直接导致"用一次历史就多一条一模一样的记录"（验收判据 1 会失败）。
	// 末尾两个参数：ui 取设置、T 翻译。
	//
	// 为什么把 a.T 传进去而不是让 writer.go 自己查设置：回写器产出的
	// Note / 错误提示是**直接显示给用户**的，必须跟着 ui.language 走
	// （§14 第 24 条把"错误提示"列为 i18n 易漏位置）。传方法值的好处是
	// 之后改语言立刻生效——它每次调用都重新解析语言，而不是快照。
	wb, err := NewWriteback(backend, pipeline.Guard(), blobs, db, ctrl, a.uiSettings, a.T, a.log)
	if err != nil {
		return err
	}

	// GC。配置用函数传进去（每轮重新读设置），这样设置界面改完立刻生效。
	gc, err := retention.New(db, blobs, a.gcConfig, a.log)
	if err != nil {
		return err
	}

	a.initMu.Lock()
	a.blobs = blobs
	a.writer = writer
	a.cap = pipeline
	a.settings = settings
	a.backend = backend
	a.ctrl = ctrl
	a.wb = wb
	a.gc = gc
	a.initMu.Unlock()

	a.touchActivity()

	// 写入器先起：捕获一旦开始就会立刻往队列里塞东西。
	writer.Start(ctx)

	if err := pipeline.Start(ctx); err != nil {
		// 捕获起不来不算致命：历史库还在，用户至少能看到诊断信息。
		a.log.Error("剪贴板捕获启动失败", "err", err)
	}

	// GC 起在自己的 goroutine 里，按 retention.gcIntervalSec 走。
	gc.Start()

	if a.noPlatformUI {
		// 测试路径：不接管面板、不装托盘（见 noPlatformUI 的字段注释）。
		return nil
	}

	// 面板接管与托盘放在**独立的 goroutine** 里：Wails 是在 OnStartup 回调
	// **返回之后**才创建窗口的，所以这里必须让 startup 先返回。
	// Controller.Attach 内部自己轮询等窗口出现。
	go a.attachPanel(ctrl, settings)

	// 空闲 watcher 同理（它只读原子变量，开销可以忽略）。
	go a.startIdleWatcher(ctx)

	return nil
}

// attachPanel 等 Wails 窗口出现，把 WebView 接管进免抢焦点面板，并装托盘。
//
// 全部失败路径都只记日志：面板/托盘/热键都是"增强"，任何一项起不来
// 都不该让捕获链路停摆——用户的剪贴板历史照记不误。
func (a *App) attachPanel(ctrl panel.Controller, s *store.Settings) {
	cfg := panel.Config{
		Hotkey:  s.UI.Hotkey,
		Width:   panelDefaultWidth,
		Height:  panelDefaultHeight,
		Tooltip: a.T(msgTrayTooltip),
	}
	if err := ctrl.Attach(cfg); err != nil {
		switch {
		case errors.Is(err, panel.ErrHotkeyTaken):
			// 这不是致命错误，但**必须让用户知道**——否则他会反复按一个
			// 永远没反应的键。通过事件推给前端，由前端显式提示。
			a.log.Warn("全局热键被别的应用占用", "hotkey", s.UI.Hotkey, "err", err)
			pushEvent(EventShow, a.T(msgNotifyHotkeyTaken))
			a.notify(a.T(msgTrayTooltip), a.T(msgNotifyHotkeyTaken))
		case errors.Is(err, panel.ErrUnsupported):
			a.log.Info("当前平台没有面板实现（Linux 只留骨架）")
			return
		default:
			a.log.Warn("面板接管失败", "err", err)
			return
		}
	}

	// Accessory 激活策略：不占 Dock、不抢前台。它是免抢焦点的**必要条件**
	// （DESIGN §0.2 实测），首选做法是 plist 里的 LSUIElement，
	// 这里是窗口已建出来之后的兜底。
	if err := ctrl.SetActivationPolicyAccessory(); err != nil {
		a.log.Warn("切换到 Accessory 激活策略失败", "err", err)
	}

	a.installTray()
	a.log.Info("面板与托盘就绪", "hotkey", s.UI.Hotkey, "diag", ctrl.Diag())
}

// 面板默认尺寸（逻辑点）。DESIGN §11 P0 的面板尺寸。
const (
	panelDefaultWidth  = 420
	panelDefaultHeight = 520
)

// filterConfigFrom 把 settings 视图翻译成过滤器的输入。
//
// 这是唯一的"设置 → 捕获行为"的翻译点，M2 做设置界面时热重载也改这里。
func filterConfigFrom(s *store.Settings) clipboard.FilterConfig {
	return clipboard.FilterConfig{
		Enabled:             s.Capture.Enabled,
		Types:               s.Capture.Types,
		ExcludeApps:         s.Exclude.Apps,
		ExcludePrivateTypes: s.Exclude.PrivateTypes,
		ImageMaxBytes:       s.Capture.ImageMaxBytes,
		DebounceMs:          s.Capture.DebounceMs,
	}
}

// shutdown 是 Wails 的 OnShutdown 钩子。
func (a *App) shutdown(context.Context) { a.Shutdown() }

// Shutdown 按"捕获 → 写入 → 库"的顺序收摊，可重复调用；
// 初始化没成功（或半途失败）时也能安全调用。
func (a *App) Shutdown() {
	a.stopOnce.Do(func() {
		a.initMu.Lock()
		db, writer, pipeline := a.db, a.writer, a.cap
		gc, ctrl := a.gc, a.ctrl
		a.initMu.Unlock()

		// ⓪ 先把面板/托盘收掉。放在最前面是因为它们会引用 DB 与 blobs：
		//    托盘菜单项里要读设置（"暂停记录"的勾选态），
		//    留到库关掉之后再响应点击就会撞上已关闭的 db。
		if ctrl != nil {
			ctrl.Close()
		}
		// ① 停 GC：它会在库里做删除与 VACUUM，必须早于写入器关闭。
		if gc != nil {
			gc.Stop()
		}
		// ② 取消后台 ctx：捕获循环会停掉后端监听，写入器会把手头的批次刷完。
		if a.cancel != nil {
			a.cancel()
		}
		// ③ 再显式等捕获退出，确保没有新的 Enqueue 进来。
		if pipeline != nil {
			pipeline.Stop()
		}
		// ④ 关写入器：排空队列 + 最后一次 checkpoint。
		if writer != nil {
			if err := writer.Close(); err != nil {
				a.log.Error("关闭写入器失败", "err", err)
			}
		}
		// ⑤ 关库。这一步会删掉 clean_shutdown 标记——标记还在就代表
		//    上次是异常退出，下次启动要跑 integrity_check。
		if db != nil {
			if err := db.Close(); err != nil {
				a.log.Error("关闭数据库失败", "err", err)
			} else {
				a.log.Info("已正常退出，clean_shutdown 标记已清除")
			}
		}
	})
}

// baseCtx 给绑定方法用一个不会在半路被取消的 ctx。
func (a *App) baseCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// ready 返回已初始化好的资源；未就绪时返回错误。
//
// 先把三个字段取出来再放锁，是因为"没就绪"这句话要按当前语言生成，
// 而取语言会读设置（另一把锁）。在持有 initMu 的时候去拿 settingsMu，
// 会让"初始化中"与"改设置"这两条路径互相等待。
func (a *App) ready() (*store.DB, *store.Writer, *capture.Capture, error) {
	a.initMu.Lock()
	db, w, cap, initErr := a.db, a.writer, a.cap, a.initErr
	a.initMu.Unlock()

	if initErr != nil {
		return nil, nil, nil, initErr
	}
	if db == nil || w == nil || cap == nil {
		return nil, nil, nil, errors.New(a.T(msgErrNotReady))
	}
	return db, w, cap, nil
}

// ── 前端绑定（M1 只读诊断面，无 UI 业务）─────────────────────────

// Health 是给前端的后端状态快照。
//
// M1 没有 UI，这个方法的唯一用途就是"让占位页面能证明后端活着、并且在
// 真的记东西"。字段名与 frontend/src/App.tsx 里的 Health 类型一一对应。
//
// 初始化失败时也返回它（而不是报错），只把 InitError 填上：诊断信息要尽量给全。
type Health struct {
	Version         string `json:"version"`
	Platform        string `json:"platform"`
	InitError       string `json:"initError"`
	BootWarning     string `json:"bootWarning"`
	DBPath          string `json:"dbPath"`
	SchemaVersion   int    `json:"schemaVersion"`
	FTSAvailable    bool   `json:"ftsAvailable"`
	IntegrityNote   string `json:"integrityNote"`
	AliveItems      int64  `json:"aliveItems"`
	TrashedItems    int64  `json:"trashedItems"`
	AllItems        int64  `json:"allItems"`
	TotalAliveBytes int64  `json:"totalAliveBytes"`
	Capture         Stats  `json:"capture"`
	Writer          WStats `json:"writer"`
}

// Stats 是捕获流水线的计数快照（对 capture.Stats 的 JSON 投影）。
type Stats struct {
	Ticks      int64            `json:"ticks"`
	Reads      int64            `json:"reads"`
	Accepted   int64            `json:"accepted"`
	EnqueueErr int64            `json:"enqueueErr"`
	ReadErrs   int64            `json:"readErrs"`
	Empty      int64            `json:"empty"`
	Busy       int64            `json:"busy"`
	Flapping   int64            `json:"flapping"`
	Drops      map[string]int64 `json:"drops"`
	LastError  string           `json:"lastError"`
}

// WStats 是写入器的计数快照（对 store.WriterStats 的 JSON 投影）。
type WStats struct {
	Enqueued    int64 `json:"enqueued"`
	Rejected    int64 `json:"rejected"`
	Items       int64 `json:"items"`
	Batches     int64 `json:"batches"`
	Errors      int64 `json:"errors"`
	Checkpoints int64 `json:"checkpoints"`
	LastFlushMs int64 `json:"lastFlushMs"`
}

// Health 返回后端状态。
func (a *App) Health() (*Health, error) {
	h := &Health{
		Version:       Version,
		Platform:      runtime.GOOS,
		SchemaVersion: store.SchemaVersion,
	}

	a.initMu.Lock()
	initErr, db, bootWarn := a.initErr, a.db, a.bootWarn
	a.initMu.Unlock()
	if initErr != nil {
		h.InitError = initErr.Error()
	}
	// 启动警告按**当前语言**在此渲染（同 exportReport 的做法）：
	// 前端只显示，不措辞。放在 Health 里而不是 pushEvent，是因为它描述的是
	// 一个**持续状态**（"这次运行用的是默认配置"），不是一个瞬时事件——
	// 用事件推的话，用户错过那条 toast 就再也看不到原因了。
	if bootWarn != nil {
		h.BootWarning = a.render(msgBootConfigBroken, bootWarn)
	}
	if db == nil {
		return h, nil
	}

	ctx := a.baseCtx()
	h.DBPath = db.Path()
	h.FTSAvailable = db.FTSAvailable()
	h.IntegrityNote = db.IntegrityNote()

	var err error
	if h.AliveItems, err = db.CountAlive(ctx); err != nil {
		return nil, err
	}
	if h.TrashedItems, err = db.CountTrashed(ctx); err != nil {
		return nil, err
	}
	if h.AllItems, err = db.CountAll(ctx); err != nil {
		return nil, err
	}
	if h.TotalAliveBytes, err = db.TotalAliveBytes(ctx); err != nil {
		return nil, err
	}

	_, writer, pipeline, err := a.ready()
	if err != nil {
		return h, nil
	}

	cs := pipeline.Stats()
	h.Capture = Stats{
		Ticks:      cs.Ticks,
		Reads:      cs.Reads,
		Accepted:   cs.Accepted,
		EnqueueErr: cs.EnqueueErr,
		ReadErrs:   cs.ReadErrs,
		Empty:      cs.Empty,
		Busy:       cs.Busy,
		Flapping:   cs.Flapping,
		Drops:      cs.Drops,
		LastError:  cs.LastError,
	}

	ws := writer.Stats()
	h.Writer = WStats{
		Enqueued:    ws.Enqueued,
		Rejected:    ws.Rejected,
		Items:       ws.Items,
		Batches:     ws.Batches,
		Errors:      ws.Errors,
		Checkpoints: ws.Checkpoints,
		LastFlushMs: ws.LastFlushMs,
	}
	return h, nil
}

// Settings 返回运行时设置（只读快照）。后端未就绪时返回 nil。
func (a *App) Settings() *store.Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	a.initMu.Lock()
	s := a.settings
	a.initMu.Unlock()
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// SetSetting 写一个设置项，value 是它的 JSON 字面量
// （字符串要带引号：`"zh-CN"`；数组：`["text","image"]`）。
//
// 改完**立刻生效**（M2 的热重载）。三条各不相同的作用路径，别搞混：
//
//	capture.* / exclude.*  → 重建过滤器推给捕获链路（Capture.ApplyFilter）
//	ui.hotkey              → 注销旧热键、注册新的（可能失败：被占用）
//	其它 ui.* / retention.* → 天然生效，因为它们本来就是"每次读一次"的
//	                        （回写器走 uiSettings()，GC 走 gcConfig()）
func (a *App) SetSetting(key, value string) error {
	if !slices.Contains(store.SettingKeys(), key) {
		// diag: 前端契约违约（传了一个后端不认识的设置键）。这句话是给
		// 开发者定位问题的，不是给用户的提示——用户改不了这个。
		return fmt.Errorf("pawclip: 未知设置键 %q", key)
	}
	db, _, _, err := a.ready()
	if err != nil {
		return err
	}
	ctx := a.baseCtx()
	if err := db.SetRaw(ctx, key, value); err != nil {
		return err
	}

	a.settingsMu.Lock()
	a.initMu.Lock()
	s := a.settings
	a.initMu.Unlock()
	var reloadErr error
	if s != nil {
		reloadErr = store.LoadSettingsInto(ctx, db, s)
	}
	a.settingsMu.Unlock()
	if reloadErr != nil {
		// 设置已经写进库了（那一行是真的），只是内存里的视图没更新。
		// 如实报错，而不是"看起来成功但内存里还是旧值"。
		return msgf(msgErrSettingsLoad, reloadErr, reloadErr)
	}

	a.applySettingChange(key)
	return nil
}

// applySettingChange 把"设置变了"这件事推到真正受影响的组件上。
//
// 为什么按 key 分派而不是"每次全推一遍"：重建过滤器是有代价的
// （会重置去抖计时器），热键换绑会短暂注销——只改一个主题色不该引起这些。
func (a *App) applySettingChange(key string) {
	switch {
	case strings.HasPrefix(key, "capture."), strings.HasPrefix(key, "exclude."):
		if cap := a.capturePipeline(); cap != nil {
			if err := cap.ApplyFilter(filterConfigFrom(a.Settings())); err != nil {
				a.log.Warn("把新的过滤设置推给捕获链路失败", "key", key, "err", err)
			}
		}
		// 暂停/恢复会改变托盘的勾选态。
		a.refreshTray()
	case key == "ui.hotkey":
		ctrl := a.panelController()
		if ctrl == nil {
			return
		}
		s := a.Settings()
		if s == nil {
			return
		}
		if err := ctrl.RegisterHotkey(s.UI.Hotkey); err != nil {
			// 换绑失败时**旧热键已经注销了**（RegisterHotkey 的契约是"先卸后装"），
			// 所以这是一个用户可感知的失败：必须显式提示，不能只记日志。
			a.log.Error("换绑全局热键失败", "hotkey", s.UI.Hotkey, "err", err)
			a.notify(a.T(msgTrayTooltip), a.T(msgNotifyHotkeyTaken))
			pushEvent(EventSettings, a.T(msgNotifyHotkeyTaken))
		}
	case key == "ui.language":
		// 语言变了要重刷托盘文案（它是唯一"后端自己渲染的文字"）。
		a.refreshTray()
	}
}

// Flush 强制把写入队列排空并提交。拍验收证据、或用户手动导出前用得上。
func (a *App) Flush() error {
	_, writer, _, err := a.ready()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return writer.Flush(ctx)
}

// Version 返回版本号。
func (a *App) Version() string { return Version }

// ConfigPath 返回本次生效的 config.toml 路径。
func (a *App) ConfigPath() string { return a.cfgPath }

// DataDir 返回数据目录（库、blobs/、标记文件都在这里）；后端未就绪时返回空串。
func (a *App) DataDir() string {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	if a.db == nil {
		return ""
	}
	return a.db.Dir()
}
