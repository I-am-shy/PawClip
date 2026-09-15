package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/zego/pawclip/capture"
	"github.com/zego/pawclip/clipboard"
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
	initMu   sync.Mutex
	initErr  error
	db       *store.DB
	blobs    *store.BlobStore
	writer   *store.Writer
	cap      *capture.Capture
	settings *store.Settings

	settingsMu sync.RWMutex
	stopOnce   sync.Once
}

// NewApp 只保存配置。不做任何 IO、不碰磁盘、不启 goroutine。
func NewApp(boot BootstrapConfig, cfgPath string, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{
		log:     log,
		boot:    normalizeBootstrap(boot),
		cfgPath: cfgPath,
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

	a.initMu.Lock()
	a.blobs = blobs
	a.writer = writer
	a.cap = pipeline
	a.settings = settings
	a.initMu.Unlock()

	// 写入器先起：捕获一旦开始就会立刻往队列里塞东西。
	writer.Start(ctx)

	if err := pipeline.Start(ctx); err != nil {
		// 捕获起不来不算致命：历史库还在，用户至少能看到诊断信息。
		a.log.Error("剪贴板捕获启动失败", "err", err)
	}
	return nil
}

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
		a.initMu.Unlock()

		// ① 先取消后台 ctx：捕获循环会停掉后端监听，写入器会把手头的批次刷完。
		if a.cancel != nil {
			a.cancel()
		}
		// ② 再显式等捕获退出，确保没有新的 Enqueue 进来。
		if pipeline != nil {
			pipeline.Stop()
		}
		// ③ 关写入器：排空队列 + 最后一次 checkpoint。
		if writer != nil {
			if err := writer.Close(); err != nil {
				a.log.Error("关闭写入器失败", "err", err)
			}
		}
		// ④ 关库。这一步会删掉 clean_shutdown 标记——标记还在就代表
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
func (a *App) ready() (*store.DB, *store.Writer, *capture.Capture, error) {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	if a.initErr != nil {
		return nil, nil, nil, a.initErr
	}
	if a.db == nil || a.writer == nil || a.cap == nil {
		return nil, nil, nil, errors.New("pawclip: 后端尚未初始化完成")
	}
	return a.db, a.writer, a.cap, nil
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
	initErr, db := a.initErr, a.db
	a.initMu.Unlock()
	if initErr != nil {
		h.InitError = initErr.Error()
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
// ⚠️ M1 限制：改 `capture.*` / `exclude.*` 只写进库，**不会热生效**——
// 过滤器的配置是构造时固化的（捕获 goroutine 不想每读一条都加锁）。
// 热重载（重建 Filter + 重置去抖）属于 M2，与设置界面一起做。
func (a *App) SetSetting(key, value string) error {
	if !slices.Contains(store.SettingKeys(), key) {
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
	defer a.settingsMu.Unlock()
	a.initMu.Lock()
	s := a.settings
	a.initMu.Unlock()
	if s == nil {
		return nil
	}
	return store.LoadSettingsInto(ctx, db, s)
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
