// PawClip · 喵喵贴 —— macOS + Windows 单机剪贴板历史管理器。
//
// M1 的范围是**捕获链路 + 落库**（纯后端，无 UI），本文件只做一件事：
// 把引导配置 → 数据目录 → 捕获链路 → Wails 运行时串起来。
//
// 真正的业务在三个包里：
//
//	store/      持久化（schema / 写入路径 / blob / 设置）
//	clipboard/  平台原生剪贴板层（darwin cgo / windows x/sys / linux 骨架）
//	capture/    捕获流水线（Read → filter → normalize → fingerprint → 落库）
//
// 占位页面（frontend/）只是为了让 `wails build` 跑通，并顺手把后端计数
// 摊在屏幕上证明链路是活的。
package main

import (
	"embed"
	"flag"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

// assets 是前端构建产物。
//
// ⚠️ `frontend/dist` 里只有 `gitkeep` 是入库的（其余是 Vite 产物，已在
// .gitignore 里）。这样做是为了让 `go build ./...` 在没有跑过 npm 的机器上
// 也能通过——embed 一个不存在的目录会直接编译失败。
// 正常流程 `wails build` 会先跑 `npm run build`，把真正的 index.html 与
// assets/ 生成在这里。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	configPath := flag.String("config", "", "config.toml 路径（默认按平台约定）")
	showPaths := flag.Bool("paths", false, "打印数据目录与配置路径后退出")
	flag.Parse()

	boot, cfgPath, err := LoadBootstrap(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *showPaths {
		dir, derr := DefaultDir()
		if derr != nil {
			fmt.Fprintln(os.Stderr, derr)
			os.Exit(1)
		}
		dbPath := boot.Database.Path
		if dbPath == "" {
			if dbPath, derr = DefaultDatabasePath(); derr != nil {
				fmt.Fprintln(os.Stderr, derr)
				os.Exit(1)
			}
		}
		fmt.Printf("配置：%s\n数据目录：%s\n数据库：%s\n", cfgPath, dir, dbPath)
		return
	}

	log := newLogger(boot.Log.Level)

	// NewApp 零副作用（不开库、不碰磁盘），所以 `wails build` 生成绑定时
	// 跑这一遍是廉价且安全的。真正的初始化在 startup 里。见 app.go 的说明。
	app := NewApp(boot, cfgPath, log)

	err = wails.Run(&options.App{
		Title:  "PawClip · 喵喵贴",
		Width:  460,
		Height: 620,
		// 面板尺寸、免抢焦点、Accessory 激活策略都属于 M2（§0.2 已定方案）。
		// M1 只要求窗口能开、页面能显示后端状态。
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 244, G: 244, B: 246, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Mac: &mac.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		// wails.Run 返回时窗口已经关了；先把后台链路收干净再退出。
		app.Shutdown()
		log.Error("wails.Run 失败", "err", err)
		os.Exit(1)
	}
}
