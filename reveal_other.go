//go:build !darwin && !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// openPathInFileManager 在 Linux 上尽力而为。
//
// 先用 xdg-open 打开**所在目录**：xdg-open 没有"选中文件"的通用写法
// （各桌面环境的 D-Bus 接口不一致），所以文件与目录都退化成"打开所在目录"。
// 失败再试 gio。两个都没有时如实报错，而不是假装打开成功
// ——用户会对着一个"什么都没发生"发懵。
func openPathInFileManager(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return msgf(msgErrPathMissing, err, err)
	}
	target := p
	if !fi.IsDir() {
		target = filepath.Dir(p)
	}
	if _, err := exec.LookPath("xdg-open"); err == nil {
		if err := exec.Command("xdg-open", target).Run(); err == nil {
			return nil
		}
	}
	if _, err := exec.LookPath("gio"); err == nil {
		if err := exec.Command("gio", "open", target).Run(); err == nil {
			return nil
		}
	}
	return msgf(msgErrNoFileManager, nil)
}

// revealPath 是跨平台入口（见 dialogs.go）。
func revealPath(p string) error { return openPathInFileManager(p) }
