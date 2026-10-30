//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// 单实例守卫（macOS/Linux）：对临时目录里的锁文件加 flock。
//
// flock 随进程退出自动释放，不会留下"程序崩了、锁还在"的脏状态；锁文件
// 本身留在临时目录里没关系（下一次打开会复用）。key 是按数据库路径算的，
// 所以不同 -config 仍然可以并存。
func acquireSingleInstance(key string) (release func(), already bool, err error) {
	p := filepath.Join(os.TempDir(), "pawclip-"+key+".lock")
	f, openErr := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if openErr != nil {
		return nil, false, openErr
	}
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, lockErr
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, false, nil
}

// notifyAlreadyRunning 在 macOS/Linux 上没有 Windows 那样的 MessageBox；
// 从终端启动时 stderr 能看到原因。GUI 双击启动的场景需要平台通知，先不做。
func notifyAlreadyRunning() {
	fmt.Fprintln(os.Stderr,
		"PawClip is already running. Use the tray icon or the global hotkey to open it.")
}
