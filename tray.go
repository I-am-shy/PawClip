package main

import (
	"embed"
	"io/fs"
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
	trayOnce sync.Once
	trayPNG  []byte
)

// trayTemplatePNG 返回适合当前平台的托盘图标字节。
//
// 优先 @2x：macOS 菜单栏是 16pt（Retina 下就是 32px），§15.5 明确说
// "16px 下细节会糊，而 32px 档位下剪贴板轮廓与猫脸均清晰可辨"。
// 找不到 @2x 才退到 1x，再找不到返回 nil（平台侧会用一个空图标，
// 至少不影响功能）。
func trayTemplatePNG() []byte {
	trayOnce.Do(func() {
		for _, name := range []string{"trayTemplate@2x.png", "trayTemplate.png"} {
			if b, err := fs.ReadFile(trayFS, trayIconDir+"/"+name); err == nil && len(b) > 0 {
				trayPNG = b
				return
			}
		}
	})
	return trayPNG
}
