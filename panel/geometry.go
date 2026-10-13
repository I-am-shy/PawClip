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

// PanelSizePlan 是"把窗口对齐到目标尺寸"这一步该走的动作。
type PanelSizePlan int

const (
	// PanelSizeInvalid 目标尺寸不可用（调用方没拿到有效值）：什么都别做。
	PanelSizeInvalid PanelSizePlan = iota
	// PanelSizeKeep 尺寸已经一致，且不需要逼 WM_SIZE：什么都不做。
	PanelSizeKeep
	// PanelSizeSet 尺寸不一致：设成目标值。这次真实变化本身会产生 WM_SIZE。
	PanelSizeSet
	// PanelSizeNudge 尺寸已经一致，但**必须**产生一次 WM_SIZE：
	// 退一个逻辑点再设回来（见 windows.go 的 syncPanelSize）。
	PanelSizeNudge
)

// PlanPanelSize 决定把窗口对齐到 wantW×wantH 该走哪一步。
//
// 抽成纯函数是为了**能在开发机上被钉住**。这里唯一容易写错、又最难在真机上
// 复现的判断是"尺寸已经一致时不能什么都不做"：
//
//	Win32 只在尺寸**真的变了**的时候才发 WM_SIZE，而 Wails 把 WebView2 的
//	视口刷新挂在 mainWindow.OnSize 上（windows/frontend.go）。于是"再设一次
//	同样的尺寸"是一个不产生任何效果的空转 —— 而它看起来完全正确：代码确实
//	调了 SetWindowPos，日志里也真的执行了。
//
// 这条恢复路径在 Windows 上就是这么失效的：注释写着"呼出时再断言一次尺寸"，
// 尺寸却早在 Attach 里就成了目标值，于是 WebView2 一直停在 Embed 时的视口上
// ——"UI 缩在窗口一角"，而且 Wails 那段边缘缩放热区是在 WebView2 里按视口算
// 的，视口比窗口小的时候右/下边缘在数学上不可达，"边缘拖不动"。
//
// force 的语义：调用方知道"这一次必须让 WebView2 重算视口"（首次呼出、窗口
// 还没显示出来的时候），于是即便尺寸已经对了也走一次 PanelSizeNudge。
//
// wantW/wantH 必须 > 1：nudge 要退一个点，1 个点或更小的目标值本身就不是
// 一个可用的面板尺寸（真机上是 380×480 起，见 panel.go 的 ClampPanelSize）。
func PlanPanelSize(curW, curH, wantW, wantH int, force bool) PanelSizePlan {
	if wantW <= 1 || wantH <= 1 {
		return PanelSizeInvalid
	}
	if curW == wantW && curH == wantH {
		if force {
			return PanelSizeNudge
		}
		return PanelSizeKeep
	}
	return PanelSizeSet
}
