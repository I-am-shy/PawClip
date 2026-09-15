//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// nativeSystemLang 用系统 API 探测语言。
//
// 为什么需要它：macOS 的 GUI 程序（双击 .app 启动的那种）**拿不到 shell 的
// LANG/LC_ALL** —— launchd 给的环境里没有。所以只读环境变量会把一个中文
// 用户的界面判成英文，而这正是最常见的使用方式（用户平时不会从终端启动）。
//
// `defaults read -g AppleLocale` 读的是登录用户的语言区域（形如 "zh_CN"），
// 对 GUI 进程可用，不依赖环境变量。
func nativeSystemLang() Lang {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "defaults", "read", "-g", "AppleLocale").Output()
	if err != nil {
		// 读不到不猜：返回 LangSystem 让上层继续降级到 en。
		// 这里**不能**返回 LangEn，那会短路掉"上层还有别的探测手段"的可能。
		return LangSystem
	}
	v := strings.ToLower(strings.TrimSpace(string(out)))
	if strings.HasPrefix(v, "zh") {
		return LangZhCN
	}
	return LangEn
}
