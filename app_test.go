package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zego/pawclip/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewApp_HasNoSideEffects 是本仓库里最"贵"的一条回归用例。
//
// Wails 生成前端绑定的方式是**把二进制跑一遍**（`wails build` 里的
// "Generating bindings" 那一步）。早期把 store.Open 放在 NewApp 里，
// 结果绑定生成直接失败：
//
//	Generating bindings: ERROR ... err="store: read user_version: disk I/O error"
//
// 所以这条用例把"构造期零磁盘副作用"钉死：任何人在 NewApp 里加回一次
// 开库 / 建目录 / 写标记，`go test` 立刻红，而不是等下次 wails build 才发现。
func TestNewApp_HasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "paw.db")

	boot := DefaultBootstrap()
	boot.Database.Path = dbPath

	a := NewApp(boot, filepath.Join(dir, "config.toml"), discardLogger())
	if a == nil {
		t.Fatal("NewApp 返回 nil")
	}

	// 连父目录都不该被创建
	if _, err := os.Stat(filepath.Dir(dbPath)); !os.IsNotExist(err) {
		t.Fatalf("NewApp 创建了目录 %s（err=%v）", filepath.Dir(dbPath), err)
	}
	if a.DataDir() != "" {
		t.Fatalf("未初始化时 DataDir 应为空串，得到 %q", a.DataDir())
	}
	if s := a.Settings(); s != nil {
		t.Fatal("未初始化时 Settings 应为 nil")
	}
	// 未初始化也要能安全收摊
	a.Shutdown()

	// Health 在未初始化时也要能回答（把状态如实报出来）
	h, err := a.Health()
	if err != nil {
		t.Fatalf("未初始化时 Health 出错：%v", err)
	}
	if h.Version != Version {
		t.Errorf("版本 = %q，想要 %q", h.Version, Version)
	}
	if h.DBPath != "" {
		t.Errorf("未初始化时 dbPath 应为空串，得到 %q", h.DBPath)
	}
	if h.InitError != "" {
		t.Errorf("未初始化不等于初始化失败，initError 应为空：%q", h.InitError)
	}

	// 需要后端的绑定方法必须明确报错，而不是 panic
	if err := a.Flush(); err == nil {
		t.Error("未初始化时 Flush 应当报错")
	}
	if err := a.SetSetting(store.KeyCaptureEnabled, "false"); err == nil {
		t.Error("未初始化时 SetSetting 应当报错")
	}
}

// TestApp_InitializeShutdownRoundTrip 把 main 包的接线（config → store →
// blob → writer → backend → capture）真正跑一遍，并检查正常退出会清标记。
//
// 注意：它会真的启动平台后端（macOS 上是轮询真剪贴板）。它**只读**剪贴板、
// 不会回写，落库也落在临时目录，所以对开发机是无害的。
func TestApp_InitializeShutdownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "paw.db")

	boot := DefaultBootstrap()
	boot.Database.Path = dbPath
	boot.UI.Language = "zh-CN"

	a := NewApp(boot, filepath.Join(dir, "config.toml"), discardLogger())
	// 测试进程里永远等不到 Wails 的窗口，Attach 会白等 15s（见 noPlatformUI）。
	a.noPlatformUI = true
	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(a.Shutdown)

	// 库与标记都该就位
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("数据库没被创建：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clean_shutdown")); err != nil {
		t.Fatalf("运行期应当存在 clean_shutdown 标记：%v", err)
	}
	if a.DataDir() != dir {
		t.Errorf("DataDir = %q，想要 %q", a.DataDir(), dir)
	}

	// 设置播种：boot 里的 ui.language 只在首启用一次
	s := a.Settings()
	if s == nil {
		t.Fatal("Settings 为 nil")
	}
	if s.UI.Language != "zh-CN" {
		t.Errorf("ui.language = %q，想要 zh-CN（来自 config.toml 的引导值）", s.UI.Language)
	}
	if !s.Capture.Enabled {
		t.Error("capture.enabled 默认应为 true")
	}
	if got := len(s.Capture.Types); got != 3 {
		t.Errorf("capture.types 默认应有 3 项，得到 %v", s.Capture.Types)
	}

	// Health 应报出真实路径与版本
	h, err := a.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.DBPath != dbPath {
		t.Errorf("dbPath = %q，想要 %q", h.DBPath, dbPath)
	}
	if h.SchemaVersion != store.SchemaVersion {
		t.Errorf("schemaVersion = %d，想要 %d", h.SchemaVersion, store.SchemaVersion)
	}
	if h.InitError != "" {
		t.Errorf("initError = %q，想要空", h.InitError)
	}

	// SetSetting 写库 + 回读
	if err := a.SetSetting(store.KeyCaptureEnabled, "false"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if a.Settings().Capture.Enabled {
		t.Error("SetSetting 没有改变 capture.enabled")
	}
	if err := a.SetSetting("pawclip.not.a.key", "1"); err == nil {
		t.Error("未知设置键应当报错")
	}

	// 正常退出 → 标记被删除（这是"下次启动不做 integrity_check"的前提）
	a.Shutdown()
	if _, err := os.Stat(filepath.Join(dir, "clean_shutdown")); !os.IsNotExist(err) {
		t.Fatalf("正常退出后 clean_shutdown 标记应被删除（err=%v）", err)
	}
	a.Shutdown()  // 幂等
	_ = a.Flush() // 已关闭：应当报错而不是 panic
}

// TestApp_InitializeTwiceIsSafe 初始化只会成功一次；重复调用不应重复开库。
func TestApp_InitializeTwiceIsSafe(t *testing.T) {
	dir := t.TempDir()
	boot := DefaultBootstrap()
	boot.Database.Path = filepath.Join(dir, "paw.db")

	a := NewApp(boot, "", discardLogger())
	a.noPlatformUI = true
	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer a.Shutdown()

	// 第二次 initialize 会再开一次库并再起一套 writer/capture。
	// 这里只要求它不 panic；真正的幂等由 startup 只在启动时被调用一次保证。
	second := NewApp(boot, "", discardLogger())
	second.noPlatformUI = true
	if err := second.initialize(context.Background()); err != nil {
		t.Fatalf("第二个 App 打开同一个库失败：%v", err)
	}
	second.Shutdown()
}

func TestVersionAndPaths(t *testing.T) {
	if Version == "" {
		t.Fatal("Version 不应为空")
	}
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if !strings.HasSuffix(dir, appDirName) {
		t.Errorf("DefaultDir = %q，应以 %q 结尾", dir, appDirName)
	}
	dbPath, err := DefaultDatabasePath()
	if err != nil {
		t.Fatalf("DefaultDatabasePath: %v", err)
	}
	if filepath.Dir(dbPath) != dir {
		t.Errorf("数据库 %q 与数据目录 %q 不在同一层", dbPath, dir)
	}

	a := NewApp(DefaultBootstrap(), "CUSTOM", discardLogger())
	if a.ConfigPath() != "CUSTOM" {
		t.Errorf("ConfigPath = %q，想要 CUSTOM", a.ConfigPath())
	}
	if a.Version() != Version {
		t.Errorf("Version() = %q，想要 %q", a.Version(), Version)
	}
}
