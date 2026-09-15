//go:build !darwin

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// runCmd 见 procs_darwin.go 的说明（两边必须行为一致）。
func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// webContentProcessCount 在非 macOS 上恒为 0。
//
// Windows 的 WebView2 是 msedgewebview2.exe，逻辑上可以数，但它的进程名与
// 计数方式与 macOS 完全不同，而且 Windows 侧本实现的空闲策略是
// TrySuspend 而非销毁。为了不让一个"看起来能比"的数字误导验收，
// 这里如实返回 0，由调用方展示"不适用"。
func webContentProcessCount() int { return 0 }
