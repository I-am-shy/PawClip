//go:build windows

package panel

import (
	"errors"
	"testing"
)

// TestRegisterUnregisterHotkey_WakesThread 钉住"清空/换绑热键真的会注销"。
//
// 这是 2026-10-31 用户报障的回归用例：设置里把快捷键清空后，按原来的组合
// 仍然能呼出面板。根因是热键线程卡在 GetMessage 里，close(stop) 叫不醒它，
// 系统里的 RegisterHotKey 一直留着；只有向那个线程 PostThreadMessage(WM_QUIT)
// 才会走到 defer 里的 UnregisterHotKey。
//
// 判据不是"看日志"，而是"注销后立刻重新注册同一个组合能否成功"：
// 旧线程如果还活着，第二次注册会撞 ERROR_HOTKEY_ALREADY_REGISTERED。
func TestRegisterUnregisterHotkey_WakesThread(t *testing.T) {
	const combo = "CmdOrCtrl+Alt+Shift+F9"

	c := &winController{stop: make(chan struct{})}
	t.Cleanup(c.UnregisterHotkey)

	if err := c.RegisterHotkey(combo); err != nil {
		if errors.Is(err, ErrHotkeyTaken) {
			t.Skipf("测试组合 %s 已被别的程序占用，跳过", combo)
		}
		t.Fatalf("第一次注册 %s 失败: %v", combo, err)
	}
	c.UnregisterHotkey()

	// 旧线程若没被叫醒，这里会返回 ErrHotkeyTaken。
	if err := c.RegisterHotkey(combo); err != nil {
		t.Fatalf("注销后重新注册同一组合失败（热键线程没被叫醒？）: %v", err)
	}
	c.UnregisterHotkey()
}
