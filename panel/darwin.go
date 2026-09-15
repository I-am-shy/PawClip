//go:build darwin && cgo

package panel

/*
#cgo LDFLAGS: -framework Cocoa -framework Carbon -framework ApplicationServices
#include <stdlib.h>
#include "panel_darwin.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// macOS 面板实现。
//
// 全部原生调用都在 App 的主线程上执行（panel_darwin.m 里的 PawClip_OnMain
// 负责 dispatch_sync）——AppKit 不是线程安全的，而且 dispatch_sync 到主队列
// 同时给了我们一个内存屏障。
//
// ⚠️ 有一条**不能违反**的约束：绝不能在主线程上调用本包的任何方法。
// 主线程是 AppKit 事件循环，在里面 dispatch_sync 到主队列会立刻死锁。
// 实际上 Wails 的 OnStartup 回调跑在它自己的 goroutine 上，
// 热键回调也只在原生侧被转交给 Go goroutine（见 export_darwin.go），
// 所以这条天然满足；但如果你将来在主线程上加调用点，记得先 go 出去。

// 热键注册结果。
type darwinController struct {
	mu       sync.Mutex
	handler  Handler
	attached bool
	hotkey   string
}

func newController(h Handler) Controller {
	bindHandler(h)
	return &darwinController{handler: h}
}

// ── 热键键码表（kVK_ANSI_* / kVK_*）──────────────────────────────
//
// 这张表是"平台相关"的那一半：panel.go 的 ParseHotkey 已经把用户输入
// 归一化成平台无关的 Hotkey，这里只负责翻译成 Carbon 的虚拟键码。
var darwinKeycodes = map[string]uint32{
	"A": 0x00, "S": 0x01, "D": 0x02, "F": 0x03, "H": 0x04, "G": 0x05,
	"Z": 0x06, "X": 0x07, "C": 0x08, "V": 0x09, "B": 0x0B, "Q": 0x0C,
	"W": 0x0D, "E": 0x0E, "R": 0x0F, "Y": 0x10, "T": 0x11,
	"1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15, "6": 0x16, "5": 0x17,
	"9": 0x19, "7": 0x1A, "8": 0x1C, "0": 0x1D,
	"O": 0x1F, "U": 0x20, "I": 0x22, "P": 0x23,
	"L": 0x25, "J": 0x26, "K": 0x28,
	"N": 0x2D, "M": 0x2E,

	"ENTER": 0x24, "TAB": 0x30, "SPACE": 0x31, "BACKSPACE": 0x33, "ESC": 0x35,
	"DELETE": 0x75,
	"HOME":   0x73, "END": 0x77, "PAGEUP": 0x74, "PAGEDOWN": 0x79,
	"LEFT": 0x7B, "RIGHT": 0x7C, "DOWN": 0x7D, "UP": 0x7E,

	"F1": 0x7A, "F2": 0x78, "F3": 0x63, "F4": 0x76, "F5": 0x60, "F6": 0x61,
	"F7": 0x62, "F8": 0x64, "F9": 0x65, "F10": 0x6D, "F11": 0x67, "F12": 0x6F,
}

// Carbon 修饰键位。
const (
	carbonCmdKey     = 0x0100
	carbonShiftKey   = 0x0200
	carbonOptionKey  = 0x0800
	carbonControlKey = 0x1000
)

// carbonHotkey 把平台无关的 Hotkey 翻成 (keycode, modifiers)。
//
// CmdOrCtrl 在这里落成 ⌘ —— 这是 DESIGN §9 那个默认值
// "CmdOrCtrl+Shift+V" 在 macOS 上的正确含义。
func carbonHotkey(h Hotkey) (uint32, uint32, error) {
	code, ok := darwinKeycodes[h.Key]
	if !ok {
		return 0, 0, fmt.Errorf("panel: macOS 上没有为键 %q 定义键码", h.Key)
	}
	var mods uint32
	if h.CmdOrCtrl || h.Cmd {
		mods |= carbonCmdKey
	}
	if h.Ctrl {
		mods |= carbonControlKey
	}
	if h.Shift {
		mods |= carbonShiftKey
	}
	if h.Alt {
		mods |= carbonOptionKey
	}
	return code, mods, nil
}

// Attach 等 Wails 的窗口出现，接管它并注册热键。
func (c *darwinController) Attach(cfg Config) error {
	// Accessory 激活策略要在窗口出现之前就位（DESIGN §7 第 7 条）。
	// 我们用 build/darwin/Info.plist 的 LSUIElement 做到了"进程启动前生效"，
	// 这里再调一次是幂等的兜底——两者缺一不可的条件里它是后者。
	if err := c.SetActivationPolicyAccessory(); err != nil {
		return err
	}

	w, h := cfg.Width, cfg.Height
	if w <= 0 {
		w = 420
	}
	if h <= 0 {
		h = 560
	}

	// Wails 在 OnStartup 返回之后才创建窗口，所以这里必须等。
	// 不变量：paw_attach 幂等，重复调用无害。
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if C.paw_has_window() == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err := c.attachOnce(w, h); err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		c.mu.Lock()
		c.attached = true
		c.mu.Unlock()

		if cfg.Hotkey != "" {
			if err := c.RegisterHotkey(cfg.Hotkey); err != nil {
				// 热键被占用不是致命错误：面板、托盘、捕获都还能用。
				// 上层负责把 ErrHotkeyTaken 展示给用户。
				return err
			}
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("panel: 接管窗口超时：%w", lastErr)
	}
	return fmt.Errorf("panel: 等了 15s 也没等到 Wails 的窗口：%w", ErrNoWindow)
}

func (c *darwinController) attachOnce(w, h int) error {
	var errBuf [512]C.char
	rc := C.paw_attach(C.int(w), C.int(h), &errBuf[0], C.int(len(errBuf)))
	if rc != 0 {
		return fmt.Errorf("panel: paw_attach 失败(rc=%d)：%s", int(rc), C.GoString(&errBuf[0]))
	}
	return nil
}

// RegisterHotkey 注册全局热键。
func (c *darwinController) RegisterHotkey(combo string) error {
	hk, err := ParseHotkey(combo)
	if err != nil {
		return err
	}
	code, mods, err := carbonHotkey(hk)
	if err != nil {
		return err
	}
	var errBuf [512]C.char
	rc := C.paw_register_hotkey(C.uint(code), C.uint(mods), &errBuf[0], C.int(len(errBuf)))
	if rc != 0 {
		msg := C.GoString(&errBuf[0])
		if rc == 2 {
			return fmt.Errorf("%w：%s", ErrHotkeyTaken, msg)
		}
		return fmt.Errorf("panel: 注册热键失败：%s", msg)
	}
	c.mu.Lock()
	c.hotkey = hk.String()
	c.mu.Unlock()
	return nil
}

// UnregisterHotkey 注销热键。
func (c *darwinController) UnregisterHotkey() {
	C.paw_unregister_hotkey()
}

// Show 免抢焦点显示面板并取键盘。
func (c *darwinController) Show() error {
	C.paw_panel_recenter()
	if C.paw_show() == 0 {
		return fmt.Errorf("panel: 面板显示了但没拿到键盘（激活策略可能不是 Accessory）")
	}
	return nil
}

// Hide 隐藏面板。
func (c *darwinController) Hide() { C.paw_hide() }

// Visible 报告面板可见性。
func (c *darwinController) Visible() bool { return C.paw_visible() != 0 }

// SetTray 安装托盘图标与菜单。
func (c *darwinController) SetTray(iconPNG []byte, items []TrayItem) error {
	var errBuf [512]C.char
	var ptr unsafe.Pointer
	if len(iconPNG) > 0 {
		ptr = unsafe.Pointer(&iconPNG[0])
	}
	tip := C.CString("PawClip")
	defer C.free(unsafe.Pointer(tip))

	rc := C.paw_tray_install(ptr, C.int(len(iconPNG)), tip, &errBuf[0], C.int(len(errBuf)))
	if rc != 0 {
		return fmt.Errorf("panel: 安装托盘失败：%s", C.GoString(&errBuf[0]))
	}
	return c.UpdateTrayMenu(items)
}

// UpdateTrayMenu 只换菜单。
func (c *darwinController) UpdateTrayMenu(items []TrayItem) error {
	C.paw_tray_clear()
	for _, it := range items {
		if it.Separator {
			C.paw_tray_separator()
			continue
		}
		label := C.CString(it.Label)
		C.paw_tray_add(label, C.int(it.Action), cBool(it.Checked), cBool(it.Disabled))
		C.free(unsafe.Pointer(label))
	}
	C.paw_tray_commit()
	return nil
}

// RemoveTray 移除托盘。
func (c *darwinController) RemoveTray() { C.paw_tray_remove() }

// AutoPaste 模拟一次 ⌘V。
func (c *darwinController) AutoPaste() error {
	var errBuf [512]C.char
	if rc := C.paw_autopaste(&errBuf[0], C.int(len(errBuf))); rc != 0 {
		return fmt.Errorf("panel: 自动粘贴失败：%s", C.GoString(&errBuf[0]))
	}
	return nil
}

// CanAutoPaste 报告辅助功能授权状态。
func (c *darwinController) CanAutoPaste() bool { return C.paw_is_trusted() != 0 }

// RequestAutoPaste 打开系统授权引导。
func (c *darwinController) RequestAutoPaste() { C.paw_request_trust() }

// SetActivationPolicyAccessory 切到无 Dock 图标模式。
func (c *darwinController) SetActivationPolicyAccessory() error {
	if C.paw_set_accessory() != 0 {
		return fmt.Errorf("panel: setActivationPolicy:Accessory 失败")
	}
	return nil
}

// Close 收摊：注销热键、移除托盘。
//
// **故意不销毁面板与 WebView**：WebView 归 Wails 所有，
// 我们从它那里借来的 contentView 必须还回去，否则 Wails 关闭时会崩。
func (c *darwinController) Close() {
	C.paw_unregister_hotkey()
	C.paw_tray_remove()
}

// Diag 返回一次诊断快照（JSON）。
//
// 用途是**验收取证**：DESIGN §0.2 强调判断"有没有抢焦点"的正确判据是
// frontmostApplication 有没有变成自己，而不是 NSApp.isActive。
func (c *darwinController) Diag() string {
	p := C.paw_diag_json()
	if p == nil {
		return "{}"
	}
	defer C.paw_free(p)
	return C.GoString(p)
}

func cBool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// New 构造本平台的面板控制器。handler 的两个回调都会在**非主线程**上被调用。
func New(h Handler) Controller { return newController(h) }
