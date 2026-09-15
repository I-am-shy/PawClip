package main

import (
	"context"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct
type App struct {
	ctx       context.Context
	mode      int
	accessory bool
}

func NewApp(mode int, accessory bool) *App {
	return &App{mode: mode, accessory: accessory}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// 让 Wails 先把窗口和前端都准备好，再开始探测
	go func() {
		time.Sleep(600 * time.Millisecond)
		runProbe(a.mode, a.accessory)
		// 留几秒让面板上的结论可见，然后自动退出
		time.Sleep(5 * time.Second)
		runtime.Quit(a.ctx)
	}()
}

// ProbeLog 供前端轮询显示验证进度
func (a *App) ProbeLog() []string {
	out := make([]string, len(probeLogLines))
	copy(out, probeLogLines)
	return out
}

// ProbeState 返回 running/done 供前端判断
func (a *App) ProbeState() map[string]bool {
	return map[string]bool{
		"running": probeRunning,
		"done":    probeDone,
	}
}

// ModeName 让前端显示当前跑的是哪一组
func (a *App) ModeName() string { return modeName(a.mode) }

// Accessory 让前端显示附属模式开关
func (a *App) Accessory() bool { return a.accessory }
