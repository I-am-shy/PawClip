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
// 之前，请把下面三个假设逐条验掉：
//
//	① RegisterHotKey 是**线程相关**的，WM_HOTKEY 只投到注册它的那个线程。
//	   所以热键跑在 runtime.LockOSThread 钉住的专属线程上（专属消息泵）。
//	② WS_EX_NOACTIVATE 的窗口不会被点击激活，但也**收不到键盘输入**——
//	   这与 macOS 的 NSPanel 能"拿键盘但不抢前台"是本质差异。所以 Show
//	   时明确抢一次前台，粘贴完成后把前台还给原窗口（见 writer.go）。
//	③ NOTIFYICONDATAW 的布局按 Vista+ 写（uVersion 段）。XP 不考虑。

const (
	// wailsDefaultWindowClass 与 main.go 里 windows.Options.WindowClassName
	// 保持一致。改一边就要改另一边。
	wailsDefaultWindowClass = "wailsWindow"

	// gwlExStyle 是 GWL_EXSTYLE == -20。uintptr 是无符号类型，
	// 直接写 -20 会编译不过（constant overflows uintptr），
	// 所以用 ^uintptr(19) 表示它的补码形式。
	gwlExStyle = ^uintptr(19)

	wsExNoActivate = 0x08000000
	wsExToolWindow = 0x00000080
	wsExAppWindow  = 0x00040000

	swShowNoActivate = 6
	swHide           = 0

	swpNoActivate = 0x0010
	swpShowWindow = 0x0040
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002

	wmHotkey = 0x0312
	wmApp    = 0x8000

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
	procLoadImageW       = user32.NewProc("LoadImageW")
	procGetLastError     = kernel32.NewProc("GetLastError")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")

	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

// ── Win32 结构体 ────────────────────────────────────────────────

type point struct{ X, Y int32 }

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

func applyNoActivateStyle(hwnd windows.Handle) error {
	style, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), gwlExStyle)
	// WS_EX_TOOLWINDOW：不在任务栏与 Alt+Tab 出现（等价于 macOS 的 Accessory）
	// WS_EX_NOACTIVATE：点击不激活（docs/DESIGN.md §8 #5）
	want := style | wsExNoActivate | wsExToolWindow
	if want&wsExAppWindow != 0 {
		want &^= wsExAppWindow
	}
	if want == style {
		return nil
	}
	procSetWindowLongPtr.Call(uintptr(hwnd), gwlExStyle, want)
	// SetWindowLongPtr 的返回值不可靠（原值为 0 时返回 0 也可能成功），
	// 所以回读一次确认。
	if after, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), gwlExStyle); after != want {
		code, _, _ := procGetLastError.Call()
		return fmt.Errorf("panel: SetWindowLongPtrW(GWL_EXSTYLE) 失败：errno=%d", code)
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
		c.mu.Lock()
		c.hwnd = hwnd
		c.ready = true
		c.hotkeyCombo = cfg.Hotkey
		c.stop = make(chan struct{})
		c.mu.Unlock()

		if err := applyNoActivateStyle(hwnd); err != nil {
			return err
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

// Show 显示面板。
//
// 与 macOS 的**有意差异**：Win32 没有"拿键盘但不抢前台"的等价能力
// （WS_EX_NOACTIVATE 的窗口收不到键盘输入），所以这里明确抢一次前台，
// 由 writer.go 在粘贴完成后把前台还给原窗口。
func (c *winController) Show() error {
	c.mu.Lock()
	hwnd := c.hwnd
	c.mu.Unlock()
	if hwnd == 0 {
		return ErrNoWindow
	}
	procShowWindow.Call(uintptr(hwnd), swShowNoActivate)
	procSetWindowPos.Call(uintptr(hwnd), hwndTopmost, 0, 0, 0, 0,
		swpNoActivate|swpShowWindow|swpNoSize|swpNoMove)
	procSetForeground.Call(uintptr(hwnd))
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
func (c *winController) Diag() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf(`{"platform":"windows","windowClass":%q,"hwnd":%d,"tray":%t}`,
		wailsDefaultWindowClass, uint64(c.hwnd), c.tray)
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
