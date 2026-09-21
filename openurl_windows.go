//go:build windows

package main

import "os/exec"

// openExternalURL 把一条 URL 交给系统默认程序打开。
//
// 用 `rundll32 url.dll,FileProtocolHandler` 而不是 `cmd /c start`，两条理由：
//
//  1. cmd 会闪过一个控制台黑框（用 /B 也只是没有窗口，不是没有代价）；
//  2. 更要紧的是 URL 得经过 cmd 的解析——里面的 `&` 会被当成命令分隔符，
//     那是一条典型的命令注入面。exec.Command 不经 shell，URL 作为**单个
//     参数**递给 rundll32，`&` 就只是普通字符。
//
// ⚠️ 与 reveal_windows.go 一样，本文件**只在 CI 构建、未在真机验证**
// （§12 里那条"Windows 未真机验证"）。
func openExternalURL(u string) error {
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Run(); err != nil {
		return msgf(msgErrOpenURLFail, err, err)
	}
	return nil
}
