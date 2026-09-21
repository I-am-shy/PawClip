//go:build darwin

package main

import "os/exec"

// openExternalURL 把一条 URL 交给系统默认程序打开（macOS 走 `open`）。
//
// 协议白名单**不在这里**做：本函数只负责"递出去"这一步，白名单在
// dialogs.go 的 openableURL 里（那个要被单测钉住，而这里一行也测不了——
// 真跑起来它会弹出浏览器）。两者的分工与 reveal_*.go 一样：跨平台文件只做
// "怎么调这个系统的程序"，策略在能测的地方声明。
func openExternalURL(u string) error {
	if err := exec.Command("open", u).Run(); err != nil {
		return msgf(msgErrOpenURLFail, err, err)
	}
	return nil
}
