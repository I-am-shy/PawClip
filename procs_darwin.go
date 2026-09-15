//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// runCmd 跑一个短命令并返回 stdout（去掉尾部换行）。
//
// 统一放在这里而不是各调用点自己 exec：这些命令都是"读一个数字"，
// 必须有超时——`ps` 卡住会把 PanelLifecycle 这个绑定方法一起拖住。
func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// webContentProcessCount 数系统上 WebKit 的网页内容进程。
//
// ⚠️ 它是**全系统**计数，不是"本 App 的"。原因：WebKit 的内容进程
// （com.apple.WebKit.WebContent）是 launchd 托管的 XPC 服务，**不挂在我们的
// 进程树下**，没法按父子关系过滤。所以这个数字只在前后对比时有用
// （收起面板前后各测一次，看它有没有下降）。
//
// 这一点必须写在类型注释里，否则后来的人会把它当成"我们的 WebView 进程数"
// 直接拿去断言，而那是个错误的前提。
func webContentProcessCount() int {
	out, err := runCmd("pgrep", "-f", "com.apple.WebKit.WebContent")
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}
