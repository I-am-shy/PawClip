// Package panel 管住三件"必须和操作系统打交道"的界面能力：
//
//	① 免抢焦点面板   —— 热键呼出时不抢走前台 App 的键盘焦点（docs/DESIGN.md §0.2）
//	② 全局热键       —— macOS Carbon RegisterEventHotKey / Windows RegisterHotKey
//	③ 托盘菜单 + 自动粘贴（模拟 ⌘/Ctrl+V）
//
// 它**不做**剪贴板读写（那是 clipboard 包的事），也不碰数据库。
//
// 平台差异被收敛到 Controller 接口后面，与 clipboard.Backend 同一套路子：
// 上层（app.go）完全平台无关，只有本包内部按 build tag 分叉。
package panel

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported 表示当前平台没有面板实现（Linux 只留骨架）。
var ErrUnsupported = errors.New("panel: not supported on this platform")

// ErrNoWindow 表示 Wails 的窗口还没建出来，无法接管。
//
// 它不是错误路径而是**时序点**：Wails 在 startup 回调返回之后才创建窗口，
// 所以接管必须重试等到窗口出现（见 Controller.Attach 的文档）。
var ErrNoWindow = errors.New("panel: no window to adopt yet")

// ErrHotkeyTaken 表示热键已被别的程序注册。
//
// 这不是致命错误：面板、托盘、捕获都还能用，只是不能靠热键呼出。
// 上层应该把它显示给用户并建议换一个组合。
var ErrHotkeyTaken = errors.New("panel: hotkey is already registered by another application")

// Action 是托盘 / 面板菜单里的动作。
//
// 用不透明的整数而不是函数，是为了让它能安全地跨 cgo 边界传回 Go
// （函数指针跨语言传是能炸的那种"聪明写法"）。
type Action int

const (
	// ActionShow 呼出面板。
	ActionShow Action = iota
	// ActionToggleCapture 暂停 / 恢复记录。
	ActionToggleCapture
	// ActionSettings 打开设置窗口。
	ActionSettings
	// ActionStats 打开统计。
	ActionStats
	// ActionBackup 打开导出 / 导入。
	ActionBackup
	// ActionAbout 关于。
	ActionAbout
	// ActionQuit 退出。
	ActionQuit
	// actionMax 是合法动作的上界（用于校验来自原生的回调）。
	actionMax
)

// String 让日志与测试断言可读。
func (a Action) String() string {
	switch a {
	case ActionShow:
		return "show"
	case ActionToggleCapture:
		return "toggleCapture"
	case ActionSettings:
		return "settings"
	case ActionStats:
		return "stats"
	case ActionBackup:
		return "backup"
	case ActionAbout:
		return "about"
	case ActionQuit:
		return "quit"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Valid 校验一个来自原生的动作值。
//
// 必须校验：cgo 边界上传来的是裸 int，越界访问会 panic。
func (a Action) Valid() bool { return a >= 0 && a < actionMax }

// Handler 是面板回调的出口。实现者是 app.go。
//
// 两个方法都会在**非主线程**被调用（热键回调在 Carbon 事件处理里，
// 托盘在 AppKit 菜单动作里），实现者必须自己保证线程安全——
// 尤其是别在里面直接碰 WebView。
type Handler interface {
	// OnAction 收到一个菜单 / 托盘动作。
	OnAction(a Action)
	// OnHotkey 热键被按下。
	OnHotkey()
}

// TrayItem 是托盘菜单的一项。
//
// Separator 为真时其余字段被忽略。
type TrayItem struct {
	Label     string
	Action    Action
	Separator bool
	// Checked 用于"暂停记录"这类可勾选项。
	Checked bool
	// Disabled 为真时该项灰掉不可点。
	Disabled bool
}

// Separator 返回一个分隔线的便捷构造。
func Separator() TrayItem { return TrayItem{Separator: true} }

// Item 返回一个普通菜单项。
func Item(label string, a Action) TrayItem { return TrayItem{Label: label, Action: a} }

// Config 是构造面板时的调参。
type Config struct {
	// Hotkey 是全局热键，形如 "CmdOrCtrl+Shift+V"（docs/DESIGN.md §9 的 ui.hotkey）。
	Hotkey string
	// Width / Height 是面板尺寸（逻辑点）。
	Width, Height int
	// Tooltip 是托盘图标的悬浮提示。
	Tooltip string
}

// Hotkey 是**平台无关**的热键描述。
//
// 刻意停在"哪个键 + 哪几个修饰键"这一层：macOS 要的是 kVK_* 虚拟键码、
// Windows 要的是 VK_*，两套编号毫无关系。把映射留给各平台，
// 解析（也就是真正容易出错的那部分）就能在 mac 上被单测直接钉住。
type Hotkey struct {
	Key string // 已归一化为大写：A-Z / 0-9 / F1-F12 / 具名键

	// CmdOrCtrl 是**平台相关**的那个修饰键：macOS 上是 ⌘、Windows 上是 Ctrl。
	//
	// 它单独一个字段而不是"把 Cmd 与 Ctrl 都置上"，是因为两者都要用：
	// 置上两个位的话，macOS 会注册出一个 ⌘+Ctrl+Shift+V，用户按 ⌘⇧V 反而不响。
	CmdOrCtrl bool

	// Cmd 是**明确指定**的 ⌘ 键（macOS 专用）。在 Windows 上退化为 Win 键。
	Cmd   bool
	Ctrl  bool
	Shift bool
	Alt   bool
}

// String 还原成一个人能读的形式（用于 UI 展示与日志）。
func (h Hotkey) String() string {
	var b strings.Builder
	if h.CmdOrCtrl {
		b.WriteString("CmdOrCtrl+")
	}
	if h.Cmd {
		b.WriteString("Cmd+")
	}
	if h.Ctrl {
		b.WriteString("Ctrl+")
	}
	if h.Alt {
		b.WriteString("Alt+")
	}
	if h.Shift {
		b.WriteString("Shift+")
	}
	b.WriteString(h.Key)
	return b.String()
}

// namedKeys 是"非单字符"的合法键名。
var namedKeys = map[string]bool{
	"SPACE": true, "TAB": true, "ENTER": true, "RETURN": true,
	"ESC": true, "ESCAPE": true, "BACKSPACE": true, "DELETE": true,
	"UP": true, "DOWN": true, "LEFT": true, "RIGHT": true,
	"HOME": true, "END": true, "PAGEUP": true, "PAGEDOWN": true,
	"F1": true, "F2": true, "F3": true, "F4": true, "F5": true, "F6": true,
	"F7": true, "F8": true, "F9": true, "F10": true, "F11": true, "F12": true,
}

// ParseHotkey 解析 §9 的 ui.hotkey 字符串。
//
// 接受的修饰键别名：
//
//	CmdOrCtrl / CommandOrControl / Cmd / Command / Super / Meta  → Cmd
//	Ctrl / Control                                               → Ctrl
//	Shift                                                        → Shift
//	Alt / Option / Opt                                           → Alt
//
// `CmdOrCtrl` 是跨平台写法（docs/DESIGN.md §9 的默认值就是它）：
// 在 macOS 上等价于 ⌘，在 Windows 上等价于 Ctrl。这里把两种修饰位都置上，
// 由各平台自己决定忽略哪一个——这样解析结果与平台无关，也就能被
// 平台无关的单测覆盖。
func ParseHotkey(s string) (Hotkey, error) {
	var h Hotkey
	s = strings.TrimSpace(s)
	if s == "" {
		return h, errors.New("panel: 空的热键")
	}

	parts := strings.Split(s, "+")
	// 至少要有"一个修饰键 + 一个主键"，否则会注册出一个裸键的全局热键，
	// 那等于把用户键盘上那个键吃掉（§9 对 ⌘⇧V 的警告是同一个道理）。
	if len(parts) < 2 {
		return h, fmt.Errorf("panel: 热键 %q 至少需要一个修饰键，例如 CmdOrCtrl+Shift+V", s)
	}

	for i, raw := range parts {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			return Hotkey{}, fmt.Errorf("panel: 热键 %q 里有空的分段", s)
		}
		up := strings.ToUpper(tok)
		isLast := i == len(parts)-1

		switch up {
		case "CMDORCTRL", "COMMANDORCONTROL":
			h.CmdOrCtrl = true
			continue
		case "CMD", "COMMAND", "SUPER", "META":
			h.Cmd = true
			continue
		case "CTRL", "CONTROL":
			h.Ctrl = true
			continue
		case "SHIFT":
			h.Shift = true
			continue
		case "ALT", "OPTION", "OPT":
			h.Alt = true
			continue
		}

		if !isLast {
			return Hotkey{}, fmt.Errorf("panel: 热键 %q 里的 %q 出现在了修饰键的位置", s, tok)
		}
		key, err := normalizeKey(up)
		if err != nil {
			return Hotkey{}, fmt.Errorf("panel: 热键 %q：%w", s, err)
		}
		h.Key = key
	}

	if h.Key == "" {
		return Hotkey{}, fmt.Errorf("panel: 热键 %q 没有主键", s)
	}
	if !h.CmdOrCtrl && !h.Cmd && !h.Ctrl && !h.Shift && !h.Alt {
		return Hotkey{}, fmt.Errorf("panel: 热键 %q 没有任何修饰键", s)
	}
	return h, nil
}

func normalizeKey(up string) (string, error) {
	switch {
	case len(up) == 1 && up[0] >= 'A' && up[0] <= 'Z':
		return up, nil
	case len(up) == 1 && up[0] >= '0' && up[0] <= '9':
		return up, nil
	case namedKeys[up]:
		if up == "RETURN" {
			return "ENTER", nil
		}
		if up == "ESCAPE" {
			return "ESC", nil
		}
		return up, nil
	default:
		return "", fmt.Errorf("不认识的键名 %q", up)
	}
}

// Controller 是面板的跨平台接口。
//
// 关于 Attach 的时序：Wails 在 OnStartup 回调**返回之后**才创建窗口，
// 所以 startup 里立刻取 NSApp.windows 一定是空的。Attach 内部自己轮询等待。
type Controller interface {
	// Attach 等到 Wails 窗口出现，把它的 WebView 搬进免抢焦点面板；
	// 若 cfg.Hotkey 非空则一并注册全局热键。
	//
	// 热键被别的程序占用时返回包装了 ErrHotkeyTaken 的错误——
	// 这不是致命错误，上层应展示给用户而不是中止启动。
	Attach(cfg Config) error

	// Show 免抢焦点显示面板并取得键盘。返回 error 时面板仍可显示，
	// 只是可能收不到键盘（例如激活策略不是 Accessory）。
	Show() error
	// Hide 隐藏面板（不销毁）。
	Hide()
	// Visible 报告面板是否可见。
	Visible() bool

	// RegisterHotkey 注册 / 换绑全局热键。
	RegisterHotkey(combo string) error
	// UnregisterHotkey 注销全局热键。
	UnregisterHotkey()

	// SetTray 安装托盘图标与菜单。
	SetTray(iconPNG []byte, items []TrayItem) error
	// UpdateTrayMenu 只换菜单（图标不变）。用于刷新"暂停记录"的勾选态。
	UpdateTrayMenu(items []TrayItem) error
	// RemoveTray 移除托盘图标。
	RemoveTray()

	// AutoPaste 模拟一次 ⌘/Ctrl+V（macOS 需辅助功能授权）。
	AutoPaste() error
	// CanAutoPaste 报告自动粘贴是否可用；false 时 UI 应提示降级为"只复制"。
	CanAutoPaste() bool
	// RequestAutoPaste 打开系统的辅助功能授权引导。
	RequestAutoPaste()

	// SetActivationPolicyAccessory 把 App 切到"无 Dock 图标"模式。
	//
	// ⚠️ 它不只是美观需求：docs/DESIGN.md §0.2 实测证明 Accessory 是
	// 免抢焦点面板的**必要条件**，与"真 NSPanel"缺一不可。
	// 首选做法是 build/darwin/Info.plist 里的 LSUIElement（进程启动前生效），
	// 这个方法是在窗口已经建出来之后的兜底。
	SetActivationPolicyAccessory() error

	// Diag 返回一次诊断快照（JSON），用于验收取证。
	// 非 darwin 平台返回 "{}"。
	Diag() string

	// Close 收摊：注销热键、移除托盘。不销毁 WebView（它归 Wails 所有）。
	Close()
}
