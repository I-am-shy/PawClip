package main

import (
	"embed"
	"io/fs"
	"runtime"
	"sync"
)

// 托盘图标的来源（docs/DESIGN.md §15.4 / §15.5）。
//
// 关键约束（§15.5 最后一段）：**macOS 的菜单栏模板图只由 alpha 承载形状**，
// 颜色由系统按菜单栏明暗渲染（浅色栏黑、深色栏白）。所以源图必须是
// **纯黑 + alpha** 的 png，直接拿彩色的 appicon 当托盘图会在深色菜单栏下
// 完全看不见——这是"能装上、看着像没装"的那种失败。
//
// 设计已经把这几个文件生成好了（assets/icon/dist/tray/），所以这里**不做任何
// 图像处理**，只按平台挑一份最合适的送去。
//
// 为什么用 embed 而不是运行时读文件：托盘图标必须在 startup 里就能拿到，
// 而那时进程可能还没确定自己的 bundle 位置（开发期跑 build/bin，发布期在
// .app 里）。嵌进二进制就与 cwd、bundle 位置全都无关了。
//
//go:embed assets/icon/dist/tray
var trayFS embed.FS

const trayIconDir = "assets/icon/dist/tray"

var (
	trayOnce  sync.Once
	trayBytes []byte
)

// trayIcon 返回适合当前平台的托盘图标字节。
//
// Windows 用 **tray-win.ico**（由 assets/icon/pawclip-source.png 生成）：
// 保留原图标稿的圆角卡片 + 剪贴板 + 猫脸，去掉源图的白色外底（圆角透明）。
// Win32 的 LoadImage(IMAGE_ICON) 不接受 PNG（实测对 PNG 返回 0，于是托盘
// 落回 IDI_APPLICATION 的通用图标），所以必须先转成 ICO。
//
// ⚠️ ICO 只保留 16/20/24/32/40/48/64 这些托盘实际尺寸。带 128/256 大帧的
// ICO 在 Windows 11 上会让 Shell_NotifyIcon 显示图标、但托盘项点不动
// （2026-10-31 反馈"托盘图标又点不了了"；去掉大帧后左键菜单正常）。
// macOS 用纯黑 + alpha 的模板 PNG——菜单栏按明暗自动反色，彩色图在深色
// 菜单栏下会看不见（§15.5）。
//
// macOS 优先 @2x：菜单栏是 16pt（Retina 下就是 32px），16px 下细节会糊。
// 找不到就返回 nil（平台侧用空图标，至少不影响功能）。
func trayIcon() []byte {
	trayOnce.Do(func() {
		names := []string{"trayTemplate@2x.png", "trayTemplate.png"}
		if runtime.GOOS == "windows" {
			names = []string{"tray-win.ico"}
		}
		for _, name := range names {
			if b, err := fs.ReadFile(trayFS, trayIconDir+"/"+name); err == nil && len(b) > 0 {
				trayBytes = b
				return
			}
		}
	})
	return trayBytes
}
