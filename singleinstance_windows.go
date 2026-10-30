//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 单实例守卫（Windows）：命名互斥量。
//
// 用 Local\ 而不是 Global\：同一台机器上不同登录用户各有各的 PawClip 数据
// （%AppData%\PawClip），互斥量按 session 隔离正合适。名字里带数据库路径的
// 哈希，见 singleinstance.go。
//
// 互斥量的生命周期就是进程：第一个实例拿到 handle 并持有到退出；第二个
// CreateMutexW 返回 ERROR_ALREADY_EXISTS。崩溃时句柄由系统关闭，不会留下
// 需要手工清理的锁文件。
var (
	singleInstanceKernel32 = syscall.NewLazyDLL("kernel32.dll")
	singleInstanceUser32   = syscall.NewLazyDLL("user32.dll")

	procCreateMutexW = singleInstanceKernel32.NewProc("CreateMutexW")
	procCloseHandle  = singleInstanceKernel32.NewProc("CloseHandle")
	procMessageBoxW  = singleInstanceUser32.NewProc("MessageBoxW")
)

const errorAlreadyExists = syscall.Errno(183)

// acquireSingleInstance 尝试拿锁。already=true 时 release 为 nil（对方的锁
// 不归我们释放）。
func acquireSingleInstance(key string) (release func(), already bool, err error) {
	name := "Local\\PawClip-" + key
	namePtr, convErr := syscall.UTF16PtrFromString(name)
	if convErr != nil {
		return nil, false, convErr
	}

	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		// diag: 单实例守卫的底层 Win32 错误，只写 stderr，不会出现在界面上。
		return nil, false, fmt.Errorf("panel: CreateMutexW(%s) 失败：%v", name, callErr)
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		// 必须关掉刚拿到的这个 handle，否则会替第一个实例多持有一份引用。
		procCloseHandle.Call(h)
		return nil, true, nil
	}

	return func() { procCloseHandle.Call(h) }, false, nil
}

// notifyAlreadyRunning 在第二份实例上弹一只说明框，然后由 main 直接退出。
// 文案中英并列：此时 settings 还没读，拿不到用户语言。
func notifyAlreadyRunning() {
	text, _ := syscall.UTF16PtrFromString(
		"PawClip 已经在运行。\n\n" +
			"PawClip is already running.\n\n" +
			"请从托盘图标或热键打开已有窗口。\n" +
			"Use the tray icon or the global hotkey to open the running window.")
	title, _ := syscall.UTF16PtrFromString("PawClip")

	const (
		mbOK              = 0x00000000
		mbIconInformation = 0x00000040
		mbSetForeground   = 0x00010000
		mbTopmost         = 0x00040000
	)
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		mbOK|mbIconInformation|mbSetForeground|mbTopmost)
}
