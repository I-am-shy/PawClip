// PawClip · 喵喵贴 —— macOS + Windows 单机剪贴板历史管理器。
//
// 本文件只做一件事：把引导配置 → 数据目录 → 捕获链路 → Wails 运行时串起来。
// 真正的业务在各个包里：
//
//	store/      持久化（schema / 写入路径 / blob / 检索 / 生命周期 / 导入导出）
//	clipboard/  平台原生剪贴板层（darwin cgo / windows x/sys / linux 骨架）
//	capture/    捕获流水线（Read → filter → normalize → fingerprint → 落库）
//	panel/      免抢焦点面板 + 全局热键 + 托盘（darwin AppKit / windows Win32）
//	retention/  GC 六步（TTL → 回收站 → 淘汰 → VACUUM）
//	backup/     .clipbak 导出 / 导入
//
// Wails 侧的角色**只是一个被接管的宿主**：它把窗口和 WKWebView 建出来，
// 然后 panel/ 在 startup 之后把 contentView 搬进自己的真 NSPanel（§0.2）。
// 所以下面的窗口选项基本都是"建出来就别管"的配置。
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

const (
	// panelWidth / panelHeight 是面板的逻辑尺寸。
	//
	// ⚠️ 这两个数只是"建窗时先用着"的初始值，**不是**默认尺寸：真正的默认
	// 尺寸是 store.DefaultSettings 里的 ui.panelWidth / ui.panelHeight
	// （用户拖过边缘之后以库里存的为准），面板拿到的是 panel.Config 里的值。
	// M0 实测的坑是"Wails 仍持有原窗口引用，它后续的 SetSize 会作用在已被
	// 掏空的原窗口上"，所以尺寸一律以 panel.Config 为准。
	//
	// 这里跟默认值保持一致，纯粹是为了避免创建瞬间出现一个尺寸不对的窗口。
	panelWidth  = 560
	panelHeight = 760
)

func main() {
	configPath := flag.String("config", "", "config.toml 路径（默认按平台约定）")
	showPaths := flag.Bool("paths", false, "打印数据目录与配置路径后退出")
	flag.Parse()

	// ⚠️ 读配置失败**不再直接退出**。
	//
	// 原来的写法是 `fmt.Fprintln(os.Stderr, err); os.Exit(1)`。问题在于
	// GUI 程序的 stderr 没人看得到：用户双击图标，进程起来又立刻死掉，
	// 界面上什么都不会出现——最坏的失败模式（"程序是不是坏了？"）。
	//
	// 而同一个文件里 normalizeBootstrap 的原则恰恰写的是"把明显非法的取值
	// 拉回默认值，而不是让应用起不来"。原来这两个决定是矛盾的：值域错了可以
	// 继续跑，但**语法**错了就整个不启动。
	//
	// 现在统一成一条纪律：**能启动就必须启动**，然后让用户看到发生了什么。
	// 读不到配置时用默认值继续，把原因交给 Health（→ 界面上的常驻横幅）。
	// 真的连数据目录都确定不了（环境级故障）也不在这里死——那时
	// store.Open 会失败，而它失败是会显示在界面上的（initError）。
	//
	// 同时仍然打一份到 stderr：从终端启动的用户与看日志的人需要它。
	var bootWarn error
	boot, cfgPath, err := LoadBootstrap(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		bootWarn = err
		boot = DefaultBootstrap()
		if cfgPath == "" {
			cfgPath, _ = DefaultConfigPath()
		}
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

	// §12 的空闲内存要能和"Wails/WebKit 占了多少"分开，否则 55MB 这样的
	// 数字无法归因：是 Go + SQLite + 捕获链路的开销，还是建窗口拉起来的
	// WebView 框架？判据只有一个——**建窗口之前**量一次。
	// wails.Run 一返回，窗口与 WebKit 就已经起来了，之后再量都太晚，
	// 所以这条基线必须打在 wails.Run 之前。
	// 与 resprobe.go 的两条「资源快照」（第 5s / 第 25s）一起看，三点连成
	// 一条曲线：基线 → 刚建完窗口 → 稳态。
	if rss := ownRSSBytes(); rss >= 0 {
		log.Debug("启动基线（wails.Run 之前，窗口与 WebView 都还没有）", "rssMB", rss/(1024*1024))
	}

	// NewApp 零副作用（不开库、不碰磁盘），所以 `wails build` 生成绑定时
	// 跑这一遍是廉价且安全的。真正的初始化在 startup 里。见 app.go 的说明。
	app := newApp(boot, cfgPath, log, bootWarn)

	err = wails.Run(&options.App{
		Title:  "PawClip · 喵喵贴",
		Width:  panelWidth,
		Height: panelHeight,

		// ⚠️ M2 明确要求的第一个遗留点（见 docs/DESIGN.md §16 M2）：
		// Wails 创建窗口时会**激活一次 App**，实测首次显示时 frontmost 变成
		// 自己。对一个"热键呼出的后台工具"来说，开机抢一次焦点是不能接受的。
		// StartHidden 让宿主窗口自始至终不主动 show，第一次可见完全由
		// panel.Show()（orderFrontRegardless）决定。
		StartHidden: true,

		// Frameless / AlwaysOnTop 照 M0 PoC 的可用组合设置。
		// 宿主窗口的边框本来也看不见——contentView 已经被搬到 NSPanel 上，
		// 留下的只是一个空壳。
		Frameless:   true,
		AlwaysOnTop: true,

		// 这一项只作用于这个永远隐藏的宿主窗口。用户真正看到的面板是
		// panel/ 在 startup 之后自建的 NSPanel，那边是可拖动、可缩放的
		//（panel_darwin.m：Resizable styleMask + 380×480~760×1100 的边界），
		// 与这里的取值无关。
		DisableResize: true,

		// ⚠️ 这一行是"缩略图能不能显示"的总开关。
		//
		// docs/DESIGN.md §14 第 1 条：列表只传元数据，**图片绝不走 IPC**（100 张
		// 缩略图 base64 塞进 IPC 会让首屏卡 1 秒以上、内存翻倍）。前端拿到的
		// 是 `blob/9f/2a/<sha>.thumb.png` 这样的相对 URL，由这个 handler
		// 直接从 blobs/ 目录把字节递给 WebView。
		//
		// Handler 只在"内嵌前端产物里找不到这个文件"时才被调用，所以它不会
		// 干扰正常的前端资源加载。缺了它，所有图片会一律 404 破图。
		//
		// 用 unexported 的 app.blobStore 而不是加一个导出的 BlobStore()：
		// Wails 会把 App 的**导出方法**全变成前端绑定，多一个导出方法就多一个
		// 前端可达的入口。这个 handler 只需要一个"惰性取"的闭包——启动时
		// blobs 还是 nil（NewApp 零副作用），必须等 startup 里赋值之后才拿得到。
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: newBlobServer(app.blobStore, func(f string, a ...any) { log.Debug(f, a...) }),
		},

		// 面板底色贴近主图的浅灰卡片。用浅色而不是跟随系统：面板是一张
		// 浮在别人窗口上的卡片，纯白/浅灰在深浅两种系统主题下都成立。
		BackgroundColour: &options.RGBA{R: 244, G: 244, B: 246, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,

		Bind: []interface{}{app},

		Mac: &mac.Options{
			// 面板本身要能透出圆角，但 WebView 不能透明——透明 WebView 在
			// WKWebView 上会让文字抗锯齿走样。M0 PoC 用的也是这一组。
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "PawClip · 喵喵贴",
				Message: "跨平台剪贴板历史管理器\n本地存储，无账号，无同步。",
			},
		},
	})
	if err != nil {
		// wails.Run 返回时窗口已经关了；先把后台链路收干净再退出。
		app.Shutdown()
		log.Error("wails.Run 失败", "err", err)
		os.Exit(1)
	}
}
