package panel

// 本文件放**平台无关的窗口几何计算**。
//
// 为什么单独一个文件，而不是塞进 windows.go：windows.go 带
// `//go:build windows`，在开发机（macOS）上根本不会被编译——改错了本地
// 也看不见，只能等 CI 的交叉编译发现语法错误，而**算得对不对**连 CI 都
// 发现不了。把能纯函数化的那部分抽出来，它就落在所有平台都会编译、
// 也都会跑测试的那一半里（panel/geometry_test.go），
// 与 hotkey 解析走同一套路子（那个也是"平台无关的解析 + 各平台各自的键码表"）。

// Rect 是一块矩形。
//
// 不叫 Screen 或 Frame，是因为它同时用来表示两种东西：显示器的工作区、
// 以及窗口自身的矩形。**调用方必须保证两边同一单位**（都逻辑点，或都
// 物理像素）——混用能算出一个看起来合理、但差着一个缩放倍数的坐标，
// 那种错很难从结果上认出来。
type Rect struct {
	X, Y, W, H int
}

// recenterTopRatio 是面板距工作区顶部的相对留白。
//
// 0.12 抄自 panel_darwin.m 的 paw_panel_recenter（那里对它的解释是
// "距屏幕顶部约 12% 处居中：手指不用移动太多就能扫到列表"）。
// **两处必须一起改**：那边是 ObjC，与这里没有共享代码可抽——这也是
// 为什么本文件的公式要写得能在单测里被钉住（见 geometry_test.go）。
const recenterTopRatio = 0.12

// RecenterIn 计算把一块 winW×winH 的面板放到 work 这块工作区上时的左上角。
//
// 语义与 panel_darwin.m 的 paw_panel_recenter 对应：
//
//	x 水平居中（面板偏上的形态下，居中最自然）
//	y 取工作区顶部往下 12% 的留白处 —— **不是垂直居中**：
//	  面板偏上，视线不用往下移就能扫到列表
//	y 不得把面板底部推出工作区（面板比工作区还高时，贴住工作区顶部，
//	  而不是让顶端跑到屏幕外）
//
// ⚠️ 坐标系：work 用**左上角原点**（Win32 的 RECT / GetMonitorInfoW 就是
// 这个约定）。macOS 的 NSScreen.visibleFrame 是**左下角原点**，所以 ObjC
// 那份写的是 `origin.y + height - h - height*0.12`，表达的是同一件事——
// 抄公式时别把坐标系一起抄过来（这是最容易在这里出错的一步）。
//
// ⚠️ 与 mac 的一处**有意偏差**：ObjC 那份只有"贴住工作区底部"一条 clamp，
// 于是面板比工作区还高时会把**顶端**推到屏幕外——在 macOS 上还能从底边
// 拉回来，Windows 上面板无标题栏、顶端一旦出界就够不着拖动条了。
// 所以这里多一条 y=max(work.Y, ...)，让顶边始终留在工作区内。
// mac 侧要不要跟上是另一件事（见 docs/DESIGN.md §8 的备注），
// 但在那之前，这是"同一个函数的两个实现"里唯一被记录下来的差异。
func RecenterIn(work Rect, winW, winH int) (int, int) {
	if work.W <= 0 || work.H <= 0 || winW <= 0 || winH <= 0 {
		// 拿不到可靠输入时给工作区原点：`(0,0)` 会被当成"顶点"这个合法
		// 坐标，调用方没法据此判断失败，所以这里不出错值、只出保守值。
		return work.X, work.Y
	}

	x := work.X + (work.W-winW)/2

	// 12% 留白，再夹一次：先保证不把底边推出工作区，再保证顶边不出上界。
	y := work.Y + int(float64(work.H)*recenterTopRatio)
	if y > work.Y+work.H-winH {
		y = work.Y + work.H - winH
	}
	if y < work.Y {
		y = work.Y
	}
	return x, y
}
