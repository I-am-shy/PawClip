package main

import (
	"runtime"
)

// 开机自启（§11 P0 的最后一项）。
//
// ⚠️ 一条纪律：**只在"用户明确改了开关"时写系统**，绝不在启动时按设置
// "顺手纠正"一次。理由是误判代价不对称：
//
//   - 设置 enabled=false、系统里其实还留着自启项 → 启动时"纠正"成删除，
//     看起来对；但检测逻辑一旦误判（macOS 那边是读 launchctl 的输出，
//     可能因本地化/权限失败），就会把用户自己建的自启项删掉。
//   - 反过来把 enabled 纠正成 true 更糟：用户的机器上凭空多出一个自启项。
//
// 所以策略是"设置记意图、系统是事实，两者不一致只在 UI 上提示"，
// 且 UI 开关的初始值读的是**系统状态**（IsAutoStart），不是设置——
// 这样开关永远显示事实，不会骗人。

// AutoStartSupported 报告当前平台是否支持开机自启。
func AutoStartSupported() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		return false
	}
}

// IsAutoStart 读**系统里**的真实自启状态。
func (a *App) IsAutoStart() (bool, error) {
	if !AutoStartSupported() {
		return false, nil
	}
	return autoStartEnabled()
}

// SetAutoStart 写系统自启状态，并把意图同步进设置。
//
// 顺序是**先改系统、后写设置**：反过来的话，系统那步失败会留下一个
// "设置说已开启、实际没开"的假象；而 UI 读的是系统状态，于是用户会看到
// 开关自己跳回去，且没有任何解释。
func (a *App) SetAutoStart(enabled bool) error {
	if !AutoStartSupported() {
		return msgf(msgErrAutoStartNo, nil)
	}
	if err := setAutoStart(enabled); err != nil {
		return err
	}
	// 设置只是记录意图，写失败不该让整个操作报错（系统状态已经改好了）。
	if err := a.SetSetting("autostart.enabled", boolJSON(enabled)); err != nil {
		a.log.Warn("自启已改，但设置写入失败", "enabled", enabled, "err", err)
	}
	return nil
}

// AutoStartDiag 给 UI 与验收用的诊断串（当前落地形态）。
func (a *App) AutoStartDiag() string {
	if !AutoStartSupported() {
		return "unsupported"
	}
	return autoStartDiag()
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// op 收的是**动作名的文案键**（msgActXxx），不是字符串。
//
// 为什么不收已翻好的字符串：这个函数在平台层（autostart_darwin.go /
// autostart_windows.go）被调用，那里是包级函数、拿不到 a.T()，
// 根本翻不了。收键则两个问题一起解决——平台层只声明"是哪一步失败了"，
// 措辞交给渲染时决定。
//
// detail 是底层命令的原始输出，作为**附加诊断**拼在后面。
// 它会原样出现在界面上，这是刻意的：launchctl / reg 的报错虽然难懂，
// 但删掉它会让"失败了"变成一句无法追查的话。
func autoStartErr(op msgKey, detail string, err error) error {
	if detail == "" {
		return msgf(msgErrAutoStartFail, err, op, err)
	}
	return msgf(msgErrAutoStartFail, err, op, err.Error()+"；"+detail)
}
