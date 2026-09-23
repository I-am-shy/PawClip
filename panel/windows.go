//go:build windows

package panel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 面板实现。docs/DESIGN.md §8 规定：
//
//	#5 面板抢焦点  → 窗口样式加 WS_EX_NOACTIVATE，不要调用 SetForegroundWindow
//	托盘           → 通知区图标 + 右键菜单（Shell_NotifyIcon）
//	全局热键       → RegisterHotKey
//	自动粘贴       → SendInput 模拟 Ctrl+V，无需授权，但受 UIPI 限制（§8 #4）
//
// ⚠️ 本平台实现**未经真机验证**：开发机是 macOS，仓库里也没有 Windows CI。
// 它只保证 `GOOS=windows go build/vet ./panel/` 通过。首次在 Windows 上跑
// 之前，请把下面这些假设逐条验掉：
//
//	① RegisterHotKey 是**线程相关**的，WM_HOTKEY 只投到注册它的那个线程。
//	   所以热键跑在 runtime.LockOSThread 钉住的专属线程上（专属消息泵）。
//	② WS_EX_NOACTIVATE 的窗口不会被点击激活，但也**收不到键盘输入**——
//	   这与 macOS 的 NSPanel 能"拿键盘但不抢前台"是本质差异。所以 Show
//	   时明确抢一次前台，粘贴完成后把前台还给原窗口（见 writer.go）。
//	③ NOTIFYICONDATAW 的布局按 Vista+ 写（uVersion 段）。XP 不考虑。
//	④ 面板的**几何**（尺寸 / 出现在哪块屏的哪个位置 / 缩放边界）与 mac
//	   对齐的方式：尺寸与定位在下面"面板几何"一节，缩放交给 Wails
//	   （main.go 在 Windows 上设 DisableResize=false + Min/MaxWidth/Height，
//	   见 docs/DESIGN.md §8 第 11 条）。其中**计算**部分（12% 留白、水平居中、
//	   多显示器负原点、上下边界夹取）已被 panel/geometry_test.go 覆盖并在
//	   开发机上跑过；**系统调用**部分（MonitorFromPoint / GetMonitorInfoW /
//	   SetWindowPos / WM_GETMINMAXINFO）仍然只能在真机上验。
//	⑤ 抢前台之后键盘是否真的落进 WebView2 —— Show() 现在会把结果报出来
//	   （拿不到前台就返回错误，上层会推一条前端提示），所以真机上第一次
//	   按热键就能看到答案。若报错，问题在这一层而不在"面板没显示"。

const (
	// wailsDefaultWindowClass 与 main.go 里 windows.Options.WindowClassName
	// 保持一致。改一边就要改另一边。
	wailsDefaultWindowClass = "wailsWindow"

	// gwlExStyle 是 GWL_EXSTYLE == -20。uintptr 是无符号类型，
	// 直接写 -20 会编译不过（constant overflows uintptr），
	// 所以用 ^uintptr(19) 表示它的补码形式。
	gwlExStyle = ^uintptr(19)
	// gwlStyle 是 GWL_STYLE == -16，同样的写法（见上）。
	//
	// ⚠️ 这两处 `^uintptr(n)` 的 n 必须与官方头文件里的负值一一对应
	// （-20 → 19、-16 → 15），差一就是改到隔壁那个字段上去——
	// 而 SetWindowLongPtr 对错误的索引**不报错**，只是改了别的东西。
	gwlStyle = ^uintptr(15)

	wsExNoActivate = 0x08000000
	wsExToolWindow = 0x00000080
	wsExAppWindow  = 0x00040000

	// wsMaximizeBox 要**摘掉**（不是加上）：main.go 在 Windows 上把
	// DisableResize 设成 false（否则面板不可缩放），而 Wails 会顺手把
	// WS_MAXIMIZEBOX 一起加回来（NewWindow 里的 EnableMaxButton）。
	// 一个无边框 + 可最大化的窗口，双击"标题栏"区域会被系统直接最大化，
	// 而我们的拖动正是往 HTCAPTION 上送 WM_NCLBUTTONDOWN —— 等于给
	// 面板装了一个"双击就铺满整屏"的机关。面板是浮层，不该能最大化。
	wsMaximizeBox = 0x00010000

	swShowNoActivate = 6
	swHide           = 0

	swpNoActivate = 0x0010
	swpShowWindow = 0x0040
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	// swpNoZorder 与 swpNoActivate 一起用：只改尺寸或位置，不动层叠关系。
	// 少了它，hWndInsertAfter 传 0 会被当成 HWND_TOP，面板会被顶到所有
	// 窗口之上并压过"总是置顶"的邻居——mac 上没有这个副作用。
	swpNoZorder = 0x0004

	// monitorDefaultToNearest：鼠标坐标落在显示器之间的缝里时也给一个
	// 最近的显示器，而不是返回 0（返回 0 会让面板"呼出后不知道去哪"）。
	monitorDefaultToNearest = 2

	wmHotkey = 0x0312
	wmApp    = 0x8000

	// Drag 用：让无边框窗口进入系统的移动循环。
	wmNclButtonDown = 0x00A1 // WM_NCLBUTTONDOWN
	htCaption       = 2      // HTCAPTION

	// hotkeyID 是 RegisterHotKey 的 id，同一线程内唯一。
	hotkeyID = 1
)

// 修饰键位（MOD_*）。
const (
	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modWin      = 0x0008
	modNoRepeat = 0x4000
)

// 全部 Win32 入口都走 LazyProc。
//
// 理由：x/sys/windows 的封装层在不同版本之间会**增删**（FindWindow /
// GetWindowLongPtr / ShowWindow 这批在 v0.48 里就不存在了）。直接锁到
// LazyProc 上，升级依赖时不会因为某个 wrapper 被删而编译不过。
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procFindWindowW      = user32.NewProc("FindWindowW")
	procGetWindowLongPtr = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procIsWindowVisible  = user32.NewProc("IsWindowVisible")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procSetForeground    = user32.NewProc("SetForegroundWindow")
	procGetForeground    = user32.NewProc("GetForegroundWindow")
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procAppendMenuW      = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procPostMessage      = user32.NewProc("PostMessageW")
	procSendInput        = user32.NewProc("SendInput")
	procReleaseCapture   = user32.NewProc("ReleaseCapture")
	procSendMessage      = user32.NewProc("SendMessageW")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procMonitorFromPoint = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW  = user32.NewProc("GetMonitorInfoW")
	procGetDpiForWindow  = user32.NewProc("GetDpiForWindow")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procGetLastError     = kernel32.NewProc("GetLastError")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")

	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

// ── Win32 结构体 ────────────────────────────────────────────────

type point struct{ X, Y int32 }

// rect 是 Win32 的 RECT（**物理像素**）。
//
// 命名成 rect 而不是 Rect：本包有一个平台无关的 panel.Rect（geometry.go，
// 逻辑点 + 显式 W/H），两者语义不同——那个是我们自己算坐标用的，
// 这个是从系统读回来的原始结构体。同名会让人以为能互换。
type rect struct{ Left, Top, Right, Bottom int32 }

// monitorInfo 是 GetMonitorInfoW 的入参（MONITORINFO）。
//
// ⚠️ CbSize 必须填 sizeof：Win32 用第一个成员做版本判别，不填会失败。
type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect // 工作区（排除了任务栏/停靠栏）
	DwFlags   uint32
}

type msgT struct {
	Hwnd     windows.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32 // Windows 8+ 追加；保留以免结构体偏小被覆写
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type keybdInput struct {
	Vk      uint16
	Scan    uint16
	Flags   uint32
	Time    uint32
	ExtraIn uintptr
}

type input struct {
	Type uint32
	_    uint32 // union 的对齐
	Ki   keybdInput
	_    [8]byte
}

const (
	inputKeyboard  = 1
	keyeventfKeyUp = 0x0002

	vkControl = 0x11
	vkV       = 0x56

	// hwndMessage == (HWND)-3，message-only 窗口的父句柄。
	hwndMessage = ^uintptr(2)
	// hwndTopmost == (HWND)-1
	hwndTopmost = ^uintptr(0)
)

// notifyIconDataW 是 Shell_NotifyIcon 的入参（Vista+ 布局）。
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

const (
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfChecked   = 0x00000008
	mfDisabled  = 0x00000002

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNonotify    = 0x0080

	imageIcon      = 1
	lrLoadFromFile = 0x0010
	lrDefaultSize  = 0x0040

	idiApplication          = 32512
	errorClassAlreadyExists = 1410
)

// trayMsgWindowClass 是托盘回调消息窗口的类名。
const trayMsgWindowClass = "PawClipTrayMsgWnd"

// winController 是 Windows 的面板控制器。
type winController struct {
	mu      sync.Mutex
	handler Handler
	hwnd    windows.Handle // Wails 主窗口
	ready   bool

	// 面板几何。三者的关系是"我们要什么 / 已经显示过没有 / 位置归谁"。
	//
	// ⚠️ 这三个字段的存在理由都一样：Windows 上**这个窗口就是用户看到的面板**
	//（macOS 上的面板是 panel_darwin.m 自建的 NSPanel，与 Wails 宿主窗口
	// 无关），所以"面板应该多大、出现在哪"这件事必须在这里落实，
	// 而 cfg.Width/Height 只在 Attach 时来一次。
	wantW, wantH int  // 我们要的尺寸（逻辑点）；0 表示没拿到值，不要动窗口
	shown        bool // 是否已经显示过至少一次（见 Size() 的说明）
	placed       bool // 用户亲手拖过或缩放过：位置从此归用户，呼出时不再传送

	// geomNote 记下几何落实时**没做成但不能让启动失败**的事。
	//
	// 为什么不直接返回错误：attachPanel 收到 Attach 的错误会**连托盘一起
	// 跳过**（app.go），而托盘是没有 Dock 图标时唯一的退出入口——
	// 尺寸没设上不该让用户连"退出"都找不到。所以走 Diag()，
	// 那条会被 attachPanel 在启动日志里打出来，也能从界面上的诊断面板取到。
	geomNote string

	hwndTray windows.Handle // message-only 窗口，收托盘回调
	tray     bool

	hotkeyCombo string
	hotkeyErr   error

	trayIcon  []byte
	trayItems []TrayItem

	stop chan struct{}
}

func newController(h Handler) Controller {
	bindHandler(h)
	return &winController{handler: h}
}

// New 构造本平台的面板控制器。
func New(h Handler) Controller { return newController(h) }

// ── 全局热键 ────────────────────────────────────────────────────
//
// RegisterHotKey 是线程相关的：WM_HOTKEY 只投到注册它的那个线程。
// 所以开一个 LockOSThread 的专属线程跑消息泵，而不是复用 Wails 的 UI 线程
// （docs/DESIGN.md §8 #1 对剪贴板监听给了同样的理由：别和 UI 线程的事件循环耦合）。
func (c *winController) runHotkeyThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	c.mu.Lock()
	combo := c.hotkeyCombo
	c.mu.Unlock()

	if combo != "" {
		if err := registerHotkeyOnThisThread(combo); err != nil {
			c.mu.Lock()
			c.hotkeyErr = err
			c.mu.Unlock()
		}
	}

	var m msgT
	for {
		select {
		case <-c.stop:
			unregisterHotkeyOnThisThread()
			return
		default:
		}
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT，-1 = 错误
			unregisterHotkeyOnThisThread()
			return
		}
		if m.Message == wmHotkey && m.WParam == hotkeyID {
			emitHotkey()
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func registerHotkeyOnThisThread(combo string) error {
	hk, err := ParseHotkey(combo)
	if err != nil {
		return err
	}
	vk, mods, err := winHotkey(hk)
	if err != nil {
		return err
	}
	ret, _, callErr := procRegisterHotKey.Call(0, hotkeyID, uintptr(mods), uintptr(vk))
	if ret == 0 {
		// ERROR_HOTKEY_ALREADY_REGISTERED = 1409
		return fmt.Errorf("%w：RegisterHotKey(%s) 失败：%v", ErrHotkeyTaken, combo, callErr)
	}
	return nil
}

func unregisterHotkeyOnThisThread() { procUnregisterHotKey.Call(0, hotkeyID) }

// winHotkey 把平台无关的 Hotkey 翻成 (VK, MOD_*)。
//
// CmdOrCtrl 在这里落成 Ctrl —— 同一份设置 "CmdOrCtrl+Shift+V"
// 在 macOS 上是 ⌘⇧V、在 Windows 上是 Ctrl+Shift+V，
// 这正是那个写法存在的意义（docs/DESIGN.md §9）。
func winHotkey(h Hotkey) (uint32, uint32, error) {
	vk, ok := winVirtualKeys[h.Key]
	if !ok {
		return 0, 0, fmt.Errorf("panel: Windows 上没有为键 %q 定义虚拟键码", h.Key)
	}
	mods := uint32(modNoRepeat)
	if h.CmdOrCtrl || h.Ctrl {
		mods |= modControl
	}
	if h.Shift {
		mods |= modShift
	}
	if h.Alt {
		mods |= modAlt
	}
	if h.Cmd {
		mods |= modWin
	}
	return vk, mods, nil
}

// winVirtualKeys 是 VK_* 表。
//
// VK_A..VK_Z == 'A'..'Z'、VK_0..VK_9 == '0'..'9'，与 ASCII 顺序一致，
// 所以字母数字可以直接由字符算出，不必列 36 项。
var winVirtualKeys = func() map[string]uint32 {
	m := map[string]uint32{
		"ENTER": 0x0D, "TAB": 0x09, "SPACE": 0x20, "BACKSPACE": 0x08,
		"ESC": 0x1B, "DELETE": 0x2E,
		"LEFT": 0x25, "UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28,
		"HOME": 0x24, "END": 0x23, "PAGEUP": 0x21, "PAGEDOWN": 0x22,
		"F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73, "F5": 0x74, "F6": 0x75,
		"F7": 0x76, "F8": 0x77, "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
	}
	for ch := 'A'; ch <= 'Z'; ch++ {
		m[string(ch)] = uint32(ch)
	}
	for ch := '0'; ch <= '9'; ch++ {
		m[string(ch)] = uint32(ch)
	}
	return m
}()

// ── 窗口样式（免抢焦点）─────────────────────────────────────────
func findWailsWindow() windows.Handle {
	name, err := windows.UTF16PtrFromString(wailsDefaultWindowClass)
	if err != nil {
		return 0
	}
	ret, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(name)), 0)
	return windows.Handle(ret)
}

func applyPanelWindowStyle(hwnd windows.Handle) error {
	exStyle, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), gwlExStyle)
	// WS_EX_TOOLWINDOW：不在任务栏与 Alt+Tab 出现（等价于 macOS 的 Accessory）
	// WS_EX_NOACTIVATE：点击不激活（docs/DESIGN.md §8 #5）
	want := exStyle | wsExNoActivate | wsExToolWindow
	if want&wsExAppWindow != 0 {
		want &^= wsExAppWindow
	}
	if want != exStyle {
		if err := setWindowLongPtrVerified(hwnd, gwlExStyle, want, "GWL_EXSTYLE"); err != nil {
			return err
		}
	}

	// 摘掉 WS_MAXIMIZEBOX。为什么要在这里补一刀、而不是靠 main.go 的选项：
	// Windows 上为了拿到缩放能力把 DisableResize 设成了 false，而 Wails 的
	// NewWindow 会顺手 `EnableMaxButton(!DisableResize)` —— 也就是把这个位置
	// 加回来。可我们的拖动是往 HTCAPTION 上送 WM_NCLBUTTONDOWN，而无边框 +
	// 可最大化的窗口"双击标题栏"就是最大化：用户在标题栏上快点两下，
	// 面板会直接铺满整屏。面板是浮层，不该有最大化这个形态。
	if style, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), gwlStyle); style != 0 && style&wsMaximizeBox != 0 {
		if err := setWindowLongPtrVerified(hwnd, gwlStyle, style&^wsMaximizeBox, "GWL_STYLE"); err != nil {
			return err
		}
	}
	return nil
}

// setWindowLongPtrVerified 改一个窗口字段并回读确认。
//
// 为什么要回读：SetWindowLongPtrW 的返回值**不可靠**——原值为 0 时它同样
// 返回 0，而 0 也可能是"失败"，两者从返回值上分不开。改完再读一遍是唯一
// 能分辨的办法。
func setWindowLongPtrVerified(hwnd windows.Handle, index, value uintptr, what string) error {
	procSetWindowLongPtr.Call(uintptr(hwnd), index, value)
	if after, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), index); after != value {
		code, _, _ := procGetLastError.Call()
		return fmt.Errorf("panel: SetWindowLongPtrW(%s) 失败：errno=%d", what, code)
	}
	return nil
}

// ── 面板几何：尺寸与定位 ────────────────────────────────────────
//
// Windows 上这个窗口**就是**用户看到的面板（macOS 上另有 panel_darwin.m
// 自建的 NSPanel，与 Wails 宿主窗口无关），所以"多大、出现在哪"必须在这里
// 落实：Wails 只知道 main.go 里那对建窗占位值，用户存进 settings 的尺寸
// 只有这里读得到。
//
// ⚠️ 缩放能力**不在**本节：它由 main.go 在 Windows 上把 DisableResize 设成
// false 交给 Wails（后者的运行时 JS 负责边缘光标与 resize:<edge> 消息，
// 再进系统的 SC_SIZE 循环），拖动时的尺寸边界由 Wails 从
// options.App 的 Min/MaxWidth/Height 拿去做 WM_GETMINMAXINFO。

// applyPanelSize 把窗口客户区设成 w×h **逻辑点**（入参已经夹取过）。
//
// ⚠️ 不传 SWP_NOSIZE 正是这一步的重点：它是**唯一**能把 WebView2 从
// "0×0 或建窗值"里拉回来的动作。WebView2 的尺寸只在 Wails 收到 WM_SIZE
// 时才刷新（windows/frontend.go 挂在 mainWindow.OnSize 上），所以只要我们
// 真的改一次窗口尺寸，它就会跟着重算——哪怕窗口此刻不可见。
// 反过来说，一个客户区是 0×0 的隐藏窗口，如果谁都不去动它，它就一直是
// 0×0，显示出来就是"一圈死边框、里面什么都没有"。
func applyPanelSize(hwnd windows.Handle, w, h int) error {
	mw, mh, err := physicalSize(hwnd, w, h)
	if err != nil {
		return err
	}
	ret, _, callErr := procSetWindowPos.Call(uintptr(hwnd), 0, 0, 0,
		uintptr(mw), uintptr(mh), swpNoZorder|swpNoMove|swpNoActivate)
	if ret == 0 {
		return fmt.Errorf("panel: SetWindowPos 设尺寸为 %d×%d（物理像素）失败：%v", mw, mh, callErr)
	}
	return nil
}

// physicalSize 把逻辑点折成窗口所在显示器 DPI 下的物理像素。
func physicalSize(hwnd windows.Handle, w, h int) (int, int, error) {
	dpi := windowDPI(hwnd)
	if dpi <= 0 {
		return 0, 0, fmt.Errorf("panel: 拿不到窗口 DPI")
	}
	return roundToDPI(w, dpi), roundToDPI(h, dpi), nil
}

// windowDPI 取窗口的 DPI；拿不到时按 96（100%）处理。
//
// GetDpiForWindow 是 Win10 1607+ 的入口，老系统上 LazyProc 调用会失败。
// 按 96 处理等于"不做缩放"，比按一个猜出来的值处理要安全。
func windowDPI(hwnd windows.Handle) int {
	if v, _, _ := procGetDpiForWindow.Call(uintptr(hwnd)); v > 0 {
		return int(v)
	}
	return 96
}

// roundToDPI 逻辑点 → 物理像素（四舍五入）。
func roundToDPI(v, dpi int) int { return (v*dpi + 48) / 96 } // 48 == 96/2

// roundFromDPI 物理像素 → 逻辑点（四舍五入）。
//
// 两个方向都必须四舍五入：在 120/144/192 这些标准 DPI 下往返是精确的，
// 但自定义 DPI（比如 133%）下会带小数，而截断会让"设 560 → 读回 559 →
// 落库 559 → 下次设 559"每启动一次缩一点——一条只在真机上、只在非标准
// 缩放下才出现的缓慢漂移。
func roundFromDPI(v, dpi int) int {
	if dpi <= 0 {
		return v
	}
	return (v*96 + dpi/2) / dpi
}

// clientPoints 读窗口客户区当前的**逻辑点**尺寸；读不到或非正返回 (0,0)。
func clientPoints(hwnd windows.Handle) (int, int) {
	var r rect
	if ret, _, _ := procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); ret == 0 {
		return 0, 0
	}
	w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	dpi := windowDPI(hwnd)
	return roundFromDPI(w, dpi), roundFromDPI(h, dpi)
}

// recenterToCursorMonitor 把面板放到**鼠标所在显示器**的顶部居中。
//
// 语义与 panel_darwin.m 的 paw_panel_recenter 对齐（同样是"鼠标在哪块屏就
// 放哪块屏，横向居中、距顶 12%"）：多显示器上用户看的是鼠标那块屏，
// 按主屏放会让面板出现在另一块显示器上——这是"按了热键却没看见面板"
// 最常见的成因，而且看起来像热键失灵。
//
// 公式本身抽在 geometry.go 的 RecenterIn 里，那张表在开发机（macOS）上
// 就能跑：这一段是 Windows 专属代码里唯一能被 CI 真的验证到计算的逻辑。
//
// ⚠️ 全程用**物理像素**：GetWindowRect 与 GetMonitorInfoW 都是物理的，
// 而 RecenterIn 只要求"两边同一单位"。在这里额外折一次 DPI 是多余的，
// 还容易和 SetWindowPos 的单位搞混。
func recenterToCursorMonitor(hwnd windows.Handle) error {
	var pt point
	if ret, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ret == 0 {
		return fmt.Errorf("panel: GetCursorPos 失败")
	}

	// ⚠️ MonitorFromPoint 的 POINT 是**按值**传的 8 字节结构（x64 调用约定
	// 下整个结构进一个寄存器），所以要自己拼成"低 32 位 = x、高 32 位 = y"。
	// 传指针是最容易犯的错：系统会把指针的低位当成 x 用，面板于是出现在
	// 一个毫无道理的坐标上。（本文件其余部分同样按 x64 写，不支持 386。）
	packed := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
	mon, _, _ := procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
	if mon == 0 {
		return fmt.Errorf("panel: MonitorFromPoint 失败")
	}

	mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ret, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ret == 0 {
		return fmt.Errorf("panel: GetMonitorInfoW 失败")
	}

	var wr rect
	if ret, _, _ := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wr))); ret == 0 {
		return fmt.Errorf("panel: GetWindowRect 失败")
	}

	work := mi.RcWork
	x, y := RecenterIn(Rect{
		X: int(work.Left), Y: int(work.Top),
		W: int(work.Right - work.Left), H: int(work.Bottom - work.Top),
	}, int(wr.Right-wr.Left), int(wr.Bottom-wr.Top))

	ret, _, callErr := procSetWindowPos.Call(uintptr(hwnd), 0,
		uintptr(x), uintptr(y), 0, 0, swpNoZorder|swpNoSize|swpNoActivate)
	if ret == 0 {
		return fmt.Errorf("panel: SetWindowPos 定位到 (%d,%d) 失败：%v", x, y, callErr)
	}
	return nil
}

// ── Controller 实现 ────────────────────────────────────────────

// Attach 等 Wails 窗口出现、加上免抢焦点样式，并在专属线程上注册热键。
func (c *winController) Attach(cfg Config) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hwnd := findWailsWindow()
		if hwnd == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// 尺寸在拿锁之前先算好：ClampPanelSize 是纯函数，不必在临界区里跑。
		// (0,0) 的含义见 panel.ClampPanelSize —— "设置里没有可用的值"，
		// 那就不要动窗口（Wails 建窗时给的是 main.go 的 560×760）。
		w, h := ClampPanelSize(cfg.Width, cfg.Height)

		c.mu.Lock()
		c.hwnd = hwnd
		c.ready = true
		c.wantW, c.wantH = w, h
		c.hotkeyCombo = cfg.Hotkey
		c.stop = make(chan struct{})
		c.mu.Unlock()

		if err := applyPanelWindowStyle(hwnd); err != nil {
			return err
		}

		// 把设置里的面板尺寸真正设上去。
		//
		// ⚠️ 这一步在 mac 上是隐式成立的：darwin.go 把 cfg.Width/Height 交给
		// paw_attach，NSPanel 直接按那个尺寸建出来。Windows 上**这个窗口
		// 就是面板**，而 Wails 用的是 main.go 里那对"建窗占位值"——
		// 不设的话用户拖出来的尺寸在 Windows 上永远不生效，
		// 两个平台的面板每次都长得不一样。
		if w > 0 && h > 0 {
			if err := applyPanelSize(hwnd, w, h); err != nil {
				c.setGeomNote(fmt.Sprintf("设置面板尺寸失败（沿用建窗值）：%v", err))
			}
		} else {
			c.setGeomNote("设置里没有可用的面板尺寸，沿用建窗值")
		}

		// 托盘需要自己的消息窗口。失败不挡启动：面板与捕获都还能用。
		_ = c.startTrayMessageWindow()

		if cfg.Hotkey != "" {
			go c.runHotkeyThread()
			// 等线程把注册结果回填，好让 ErrHotkeyTaken 在启动阶段就能被看见。
			time.Sleep(150 * time.Millisecond)
			c.mu.Lock()
			err := c.hotkeyErr
			c.mu.Unlock()
			if err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("panel: 等了 15s 也没等到窗口 %q：%w", wailsDefaultWindowClass, ErrNoWindow)
}

// setGeomNote 记下一条几何相关的说明（见 geomNote 字段）。
func (c *winController) setGeomNote(s string) {
	c.mu.Lock()
	c.geomNote = s
	c.mu.Unlock()
}

// Show 显示面板。
//
// 与 macOS 的**有意差异**：Win32 没有"拿键盘但不抢前台"的等价能力
// （WS_EX_NOACTIVATE 的窗口收不到键盘输入），所以这里明确抢一次前台，
// 由 writer.go 在粘贴完成后把前台还给原窗口。
func (c *winController) Show() error {
	c.mu.Lock()
	hwnd, w, h := c.hwnd, c.wantW, c.wantH
	shown, placed := c.shown, c.placed
	c.mu.Unlock()
	if hwnd == 0 {
		return ErrNoWindow
	}

	if !shown {
		// 首次呼出：把几何断言一次再显示。
		//
		// 尺寸在 Attach 里已经设过，这里再设一次不是冗余，它兜的是两类事：
		//
		//   · 客户区退化成 0×0（DPI 切换、WebView2 初始化失败、别的程序动过
		//     窗口）。WebView2 的尺寸只在 Wails 收到 WM_SIZE 时才刷新，所以
		//     只要这一次 SetWindowPos 没被吞掉，它就会重新撑开——这正是
		//     "面板只剩一圈死边框"的恢复路径。
		//   · Attach 改尺寸时**太早**：Wails 的 Run 里 setupChromium（它会
		//     用当时的客户区尺寸初始化 WebView2）与 mainWindow.OnSize 的绑定
		//     之间存在一个几十微秒的缝隙（见 windows/frontend.go）。我们的
		//     Attach 是轮询找窗口的，理论上可能正好落在里面——那一次尺寸
		//     变化就没有人接。呼出时再设一次，等于把那条缝补上。
		if w > 0 && h > 0 {
			if err := applyPanelSize(hwnd, w, h); err != nil {
				c.setGeomNote(fmt.Sprintf("呼出时重设面板尺寸失败：%v", err))
			}
		}
		if !placed {
			if err := recenterToCursorMonitor(hwnd); err != nil {
				c.setGeomNote(fmt.Sprintf("面板定位失败（沿用上次位置）：%v", err))
			}
		}
		c.mu.Lock()
		c.shown = true
		c.mu.Unlock()
	} else if !placed {
		// 已经显示过：用户可能亲手拖过边缘，尺寸与我们要的不一致就是证据。
		//
		// 位置一旦归用户就不再传送 —— 与 mac 的 g_userPlaced 同一个语义。
		// 那边是拖动/缩放时直接置位，这里只能这么反推：Win32 没有"拖动
		// 或缩放结束"的通知可挂（WM_EXITSIZEMOVE 属于非客户区循环，
		// 而我们是把鼠标事件从 JS 侧转交进去的）。
		if cw, ch := clientPoints(hwnd); cw > 0 && (cw != w || ch != h) {
			c.mu.Lock()
			c.placed = true
			c.mu.Unlock()
		}
	}

	procShowWindow.Call(uintptr(hwnd), swShowNoActivate)
	procSetWindowPos.Call(uintptr(hwnd), hwndTopmost, 0, 0, 0, 0,
		swpNoActivate|swpShowWindow|swpNoSize|swpNoMove)
	procSetForeground.Call(uintptr(hwnd))

	// 与 mac 的 Show() 契约对齐：那边 paw_show() 拿不到键盘就返回错误，
	// 上层（resources.go 的 showPanel）会把它推成一条前端可见的提示。
	//
	// 这里同样只报**事实**：Win32 上抢前台本身就是有意的（见函数注释），
	// 抢不到就意味着键盘不会进面板——而那正是用户需要知道的事，
	// 否则表现是"面板看得见、敲字没反应"，界面上一个字都不说。
	if fg, _, _ := procGetForeground.Call(); windows.Handle(fg) != hwnd {
		return fmt.Errorf("panel: 面板已显示但没能取得前台（键盘可能不会进入面板）")
	}
	return nil
}

// Hide 隐藏面板。
func (c *winController) Hide() {
	c.mu.Lock()
	hwnd := c.hwnd
	c.mu.Unlock()
	if hwnd != 0 {
		procShowWindow.Call(uintptr(hwnd), swHide)
	}
}

// Visible 报告面板可见性。
func (c *winController) Visible() bool {
	c.mu.Lock()
	hwnd := c.hwnd
	c.mu.Unlock()
	if hwnd == 0 {
		return false
	}
	ret, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	return ret != 0
}

// Drag 开始一次原生窗口拖动。
//
// Win32 惯用法：ReleaseCapture + WM_NCLBUTTONDOWN(HTCAPTION)，让 DefWindowProc
// 进入 SC_MOVE 循环，窗口就跟着鼠标走了——与给非客户区加标题栏是同一回事。
// ⚠️ 本平台实现未经真机验证（见文件头说明）。
func (c *winController) Drag() {
	c.mu.Lock()
	hwnd := c.hwnd
	// 位置从此归用户：呼出时不再把面板传送回"鼠标屏顶部居中"。
	// 与 mac 的 paw_panel_drag（先置 g_userPlaced 再进拖动循环）同一顺序：
	// 拖动循环会阻塞到鼠标松开，放在后面会让这次拖动之后的那次呼出
	// 仍然把面板拉走。
	c.placed = true
	c.mu.Unlock()
	if hwnd == 0 {
		return
	}
	procReleaseCapture.Call(uintptr(hwnd))
	procSendMessage.Call(uintptr(hwnd), wmNclButtonDown, htCaption, 0)
}

// Size 报告面板的逻辑尺寸。
//
// ⚠️ 两条"不要落库"的路径都在这里（Controller.Size 的契约）：
//
//   - **面板还没被显示过**：此刻客户区里是 Wails 的建窗占位值
//     （main.go 的 560×760），不是用户的选择。把它当尺寸写回去会覆盖掉
//     用户在 macOS 上拖出来的尺寸 —— settings 是两个平台共用的，
//     这种污染完全静默，而且用户只会觉得"mac 上的面板尺寸老是变回去"。
//   - 拿不到或非正的客户区（窗口还没建出来 / 已经销毁）。
//
// Wails 的窗口带 DPI 缩放，所以这里读到的物理像素要折回逻辑点再落库
// （两个方向都用四舍五入，见 roundFromDPI）。
func (c *winController) Size() (int, int) {
	c.mu.Lock()
	hwnd, shown := c.hwnd, c.shown
	c.mu.Unlock()
	if hwnd == 0 || !shown {
		return 0, 0
	}
	return clientPoints(hwnd)
}

// RegisterHotkey 换绑热键（重启热键线程以切到新组合）。
func (c *winController) RegisterHotkey(combo string) error {
	if _, err := ParseHotkey(combo); err != nil {
		return err
	}
	c.mu.Lock()
	old := c.stop
	c.stop = make(chan struct{})
	c.hotkeyCombo = combo
	c.hotkeyErr = nil
	c.mu.Unlock()

	if old != nil {
		close(old)
		time.Sleep(80 * time.Millisecond) // 等旧线程退出并注销
	}
	go c.runHotkeyThread()
	time.Sleep(150 * time.Millisecond)

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hotkeyErr
}

// UnregisterHotkey 注销热键并停掉热键线程。
func (c *winController) UnregisterHotkey() {
	c.mu.Lock()
	stop := c.stop
	c.stop = nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// ── 托盘 ────────────────────────────────────────────────────────
// 托盘要靠窗口句柄收回调消息，所以自建一个 message-only 窗口（HWND_MESSAGE），
// 与剪贴板监听的做法一致（docs/DESIGN.md §8 #1）。
var (
	trayWndProcOnce sync.Once
	trayWndProcPtr  uintptr
	trayTarget      *winController
)

func (c *winController) startTrayMessageWindow() error {
	trayWndProcOnce.Do(func() {
		trayWndProcPtr = syscall.NewCallback(trayWndProc)
	})
	trayTarget = c

	hInst, _, _ := procGetModuleHandle.Call(0)
	className, err := windows.UTF16PtrFromString(trayMsgWindowClass)
	if err != nil {
		return err
	}

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   trayWndProcPtr,
		HInstance:     windows.Handle(hInst),
		LpszClassName: className,
	}
	if ret, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		// 已注册过（ERROR_CLASS_ALREADY_EXISTS）也算成功。
		if code, _, _ := procGetLastError.Call(); code != errorClassAlreadyExists {
			return fmt.Errorf("panel: RegisterClassExW(%s) 失败：errno=%d", trayMsgWindowClass, code)
		}
	}

	hw, _, callErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0,
		hwndMessage, 0, uintptr(hInst), 0)
	if hw == 0 {
		return fmt.Errorf("panel: CreateWindowExW(%s) 失败：%v", trayMsgWindowClass, callErr)
	}
	c.mu.Lock()
	c.hwndTray = windows.Handle(hw)
	c.mu.Unlock()
	return nil
}

func trayWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if t := trayTarget; t != nil && msg == wmApp+1 {
		switch uint32(lparam) & 0xFFFF {
		case wmLButtonUp, wmRButtonUp, wmContextMenu:
			t.popupMenu()
		}
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

// SetTray 安装托盘图标与菜单。
func (c *winController) SetTray(iconPNG []byte, items []TrayItem) error {
	c.mu.Lock()
	c.trayIcon = iconPNG
	c.mu.Unlock()
	if err := c.updateIcon(); err != nil {
		return err
	}
	return c.UpdateTrayMenu(items)
}

func (c *winController) updateIcon() error {
	c.mu.Lock()
	hwnd := c.hwndTray
	icon := c.trayIcon
	already := c.tray
	c.mu.Unlock()
	if hwnd == 0 {
		return ErrNoWindow
	}

	const tip = "PawClip · 喵喵贴"
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmApp + 1,
	}
	copy(nid.SzTip[:], windows.StringToUTF16(tip))

	// LoadImage 只认文件路径或资源 ID，不认内存里的字节 → 先落盘。
	// Vista 起 LoadImage 支持 PNG，不必先转 ICO。
	if len(icon) > 0 {
		if path, err := writeTempIcon(icon); err == nil {
			if p, err := windows.UTF16PtrFromString(path); err == nil {
				h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)),
					imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
				nid.HIcon = windows.Handle(h)
			}
		}
	}
	if nid.HIcon == 0 {
		h, _, _ := procLoadIconW.Call(0, idiApplication)
		nid.HIcon = windows.Handle(h)
	}

	verb := uintptr(nimAdd)
	if already {
		verb = nimModify
	}
	if ret, _, callErr := procShellNotifyIcon.Call(verb, uintptr(unsafe.Pointer(&nid))); ret == 0 {
		return fmt.Errorf("panel: Shell_NotifyIcon 失败：%v", callErr)
	}
	c.mu.Lock()
	c.tray = true
	c.mu.Unlock()
	return nil
}

// UpdateTrayMenu 记下菜单项；真正弹出时惰性构造菜单。
func (c *winController) UpdateTrayMenu(items []TrayItem) error {
	c.mu.Lock()
	c.trayItems = append([]TrayItem(nil), items...)
	c.mu.Unlock()
	return nil
}

// RemoveTray 移除托盘图标。
func (c *winController) RemoveTray() {
	c.mu.Lock()
	hwnd := c.hwndTray
	already := c.tray
	c.tray = false
	c.mu.Unlock()
	if hwnd == 0 || !already {
		return
	}
	nid := notifyIconDataW{
		CbSize: uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:   hwnd,
		UID:    1,
	}
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

// popupMenu 在鼠标位置弹出托盘菜单。
func (c *winController) popupMenu() {
	c.mu.Lock()
	items := append([]TrayItem(nil), c.trayItems...)
	hwnd := c.hwndTray
	c.mu.Unlock()
	if len(items) == 0 || hwnd == 0 {
		return
	}

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	for i, it := range items {
		if it.Separator {
			procAppendMenuW.Call(menu, mfSeparator, 0, 0)
			continue
		}
		label, err := windows.UTF16PtrFromString(it.Label)
		if err != nil {
			continue
		}
		flags := uintptr(mfString)
		if it.Checked {
			flags |= mfChecked
		}
		if it.Disabled {
			flags |= mfDisabled
		}
		// 菜单项 id = 下标 + 100，避开 0（"没选中任何项"）。
		procAppendMenuW.Call(menu, flags, uintptr(i+100), uintptr(unsafe.Pointer(label)))
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// TrackPopupMenu 要求先置前台，否则点击菜单外面菜单不会消失。
	procSetForeground.Call(uintptr(hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmRightButton|tpmReturnCmd|tpmNonotify,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	// 约定：菜单关闭后给窗口发一条空消息，避免菜单"粘"在屏幕上。
	procPostMessage.Call(uintptr(hwnd), 0, 0, 0)

	if cmd >= 100 {
		if idx := int(cmd) - 100; idx >= 0 && idx < len(items) {
			emitAction(items[idx].Action)
		}
	}
}

// ── 自动粘贴 ────────────────────────────────────────────────────
//
// SendInput 不需要辅助功能授权（相对 macOS 的优势），但受 UIPI 限制：
// 目标窗口以更高完整性级别运行时按键会被静默丢弃（§8 #4）。
// 这里不做完整性级别检测——降级表现为"粘贴没反应"，
// writer.go 会把这种情况记为一次失败并在 UI 上提示。
func (c *winController) AutoPaste() error {
	seq := [4]input{
		{Type: inputKeyboard, Ki: keybdInput{Vk: vkControl}},
		{Type: inputKeyboard, Ki: keybdInput{Vk: vkV}},
		{Type: inputKeyboard, Ki: keybdInput{Vk: vkV, Flags: keyeventfKeyUp}},
		{Type: inputKeyboard, Ki: keybdInput{Vk: vkControl, Flags: keyeventfKeyUp}},
	}
	ret, _, callErr := procSendInput.Call(
		uintptr(len(seq)), uintptr(unsafe.Pointer(&seq[0])), unsafe.Sizeof(seq[0]))
	if int(ret) != len(seq) {
		return fmt.Errorf("panel: SendInput 只被接受 %d/%d 个事件：%v", int(ret), len(seq), callErr)
	}
	return nil
}

// CanAutoPaste 在 Windows 上恒为 true（不需要授权）。
func (c *winController) CanAutoPaste() bool { return true }

// RequestAutoPaste 在 Windows 上无需引导。
func (c *winController) RequestAutoPaste() {}

// SetActivationPolicyAccessory 在 Windows 上等价于"不在任务栏出现"，
// 已由 Attach 里的 WS_EX_TOOLWINDOW 完成。
func (c *winController) SetActivationPolicyAccessory() error { return nil }

// Diag 返回诊断快照。
//
// 几何三项（want / live / placed）是刻意放进去的：面板"尺寸不对、位置乱跑"
// 这类问题在真机上只能靠这几个数字定位，而 Windows 侧没有开发机，
// 下一次取证很可能就要靠这一行 JSON。geomNote 记着几何落实失败的原因
// ——它不会被当成启动错误（见那个字段的说明），但必须留痕，
// 否则就成了"静默地沿用了一个错的尺寸"。
func (c *winController) Diag() string {
	c.mu.Lock()
	hwnd, tray := c.hwnd, c.tray
	wantW, wantH := c.wantW, c.wantH
	shown, placed := c.shown, c.placed
	note := c.geomNote
	c.mu.Unlock()

	// 在锁外读活尺寸：clientPoints 是系统调用，没必要占着锁。
	liveW, liveH := 0, 0
	if hwnd != 0 && shown {
		liveW, liveH = clientPoints(hwnd)
	}

	return fmt.Sprintf(
		`{"platform":"windows","windowClass":%q,"hwnd":%d,"tray":%t,`+
			`"wantW":%d,"wantH":%d,"liveW":%d,"liveH":%d,"shown":%t,"placed":%t,"geomNote":%q}`,
		wailsDefaultWindowClass, uint64(hwnd), tray,
		wantW, wantH, liveW, liveH, shown, placed, note)
}

// Close 收摊。
func (c *winController) Close() {
	c.UnregisterHotkey()
	c.RemoveTray()
	c.mu.Lock()
	hwnd := c.hwndTray
	c.mu.Unlock()
	if hwnd != 0 {
		procDestroyWindow.Call(uintptr(hwnd))
	}
}

// writeTempIcon 把 PNG 落到临时文件供 LoadImage 使用。
func writeTempIcon(png []byte) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "PawClip", "tray.png")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, png, 0o600); err != nil {
		return "", err
	}
	return p, nil
}
