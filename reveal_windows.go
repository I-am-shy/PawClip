//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

// openPathInExplorer 在资源管理器里"打开或选中"一个路径。
//
// 目录 → `explorer <dir>`：进去，看到内容
// 文件 → `explorer /select,<file>`：选中它
//
// ⚠️ `/select,` 后面的**逗号不能省、路径要作为整体传**：
// explorer 的命令行解析很粗糙，把 "/select," 与路径分开成两个参数时，
// 路径里的空格会让它把前半段当目标，结果打开"我的文档"而不是目标目录
// ——表现为"点了没反应"。拼成一个 token 就没这个问题。
//
// explorer 在"已经有资源管理器进程在跑"时会返回退出码 1，
// 而窗口其实已经弹出来了，所以 1 不当成失败。
func openPathInExplorer(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return msgf(msgErrPathMissing, err, err)
	}
	p = strings.TrimRight(p, `\`)
	target := p
	if !fi.IsDir() {
		target = "/select," + p
	}
	err = exec.Command("explorer", target).Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		return msgf(msgErrRevealFailed, err, err)
	}
	return nil
}

// revealPath 是跨平台入口（见 dialogs.go）。
func revealPath(p string) error { return openPathInExplorer(p) }
