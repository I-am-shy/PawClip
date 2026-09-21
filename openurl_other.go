//go:build !darwin && !windows

package main

import "os/exec"

// openExternalURL 把一条 URL 交给系统默认程序打开（Linux 等走 xdg-open）。
//
// 找不到 xdg-open 时**如实报错**，不静默返回 nil：用户点了链接什么都没发生、
// 又没有任何提示，只能自己去猜是应用坏了还是这台机器没装。
func openExternalURL(u string) error {
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return msgf(msgErrOpenURLFail, err, err)
	}
	if err := exec.Command("xdg-open", u).Run(); err != nil {
		return msgf(msgErrOpenURLFail, err, err)
	}
	return nil
}
