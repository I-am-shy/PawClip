//go:build darwin

package main

import (
	"os"
	"os/exec"
)

// openPathInFinder 在 Finder 里"打开或选中"一个路径。
//
// 两种模式，按路径是不是目录自动选：
//
//	目录 → `open <dir>`   ：进去，看到内容（"打开数据目录"就是这个诉求）
//	文件 → `open -R <file>`：选中它（用 `open` 会拿默认程序去打开，
//	                       对 .clipbak 只会得到"无法打开"的报错）
//
// 这个区分不能省：对目录用 -R 会把**父目录**打开并选中它（用户多一层），
// 对文件不用 -R 则是想打又打不开。两种错法都很难自我诊断。
func openPathInFinder(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return msgf(msgErrPathMissing, err, err)
	}
	args := []string{p}
	if !fi.IsDir() {
		// -R = reveal，选中该项而非打开它。
		args = []string{"-R", p}
	}
	if err := exec.Command("open", args...).Run(); err != nil {
		return msgf(msgErrRevealFailed, err, err)
	}
	return nil
}

// revealPath 是跨平台入口（见 dialogs.go）。
func revealPath(p string) error { return openPathInFinder(p) }
