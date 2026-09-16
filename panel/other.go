//go:build !darwin && !windows

package panel

// Linux 只留骨架（docs/DESIGN.md §0.1：暂不做 Linux，只预留后端骨架）。
//
// 这个文件的存在有两个意义：
//
//  1. 让 `GOOS=linux go build ./panel/` 能过——没有它，panel 包在
//     Linux 上会因为"没有任何实现文件"而编译失败；
//  2. 让上层不必到处写 `if runtime.GOOS == ...`：拿到的是一个
//     每个方法都返回 ErrUnsupported 的正常对象，启动流程照常走完，
//     只是没有面板 / 热键 / 托盘。
//
// 将来补 Linux 时，替换的是这个文件里的四个动作：
// 快捷键用 XGrabKey，托盘用 StatusNotifierItem（D-Bus），
// 自动粘贴用 XTEST / wtype，免抢焦点靠 override-redirect 窗口。
type unsupportedController struct{}

// New 返回一个"什么都不做"的控制器。
func New(h Handler) Controller {
	_ = h
	return unsupportedController{}
}

func (unsupportedController) Attach(Config) error { return ErrUnsupported }
func (unsupportedController) Show() error         { return ErrUnsupported }
func (unsupportedController) Hide()               {}
func (unsupportedController) Visible() bool       { return false }
func (unsupportedController) Drag()               {}

func (unsupportedController) RegisterHotkey(string) error { return ErrUnsupported }
func (unsupportedController) UnregisterHotkey()           {}

func (unsupportedController) SetTray([]byte, []TrayItem) error { return ErrUnsupported }
func (unsupportedController) UpdateTrayMenu([]TrayItem) error  { return ErrUnsupported }
func (unsupportedController) RemoveTray()                      {}

func (unsupportedController) AutoPaste() error                    { return ErrUnsupported }
func (unsupportedController) CanAutoPaste() bool                  { return false }
func (unsupportedController) RequestAutoPaste()                   {}
func (unsupportedController) SetActivationPolicyAccessory() error { return ErrUnsupported }
func (unsupportedController) Diag() string                        { return "{}" }
func (unsupportedController) Close()                              {}
