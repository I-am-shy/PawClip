//go:build windows

package main

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// nativeSystemLang 走 GetUserDefaultLocaleName。
//
// ⚠️ 不能用 windows.GetUserDefaultLocaleName：x/sys/windows v0.48 **没有导出
// 这个函数**（我逐个查过该版本的导出表）。所以这里自己 LazyProc 调
// kernel32，与 panel/windows.go 里其它 Win32 调用同一套路子。
//
// 也不能读环境变量：Windows GUI 程序从 explorer 启动时同样拿不到 shell 的
// LANG；用户语言只存在系统设置里。
var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procGetUserDefaultLocaleName = kernel32.NewProc("GetUserDefaultLocaleName")
)

func nativeSystemLang() Lang {
	if err := procGetUserDefaultLocaleName.Find(); err != nil {
		return LangSystem
	}
	// LOCALE_NAME_MAX_LENGTH == 85（含结尾 NUL）。
	buf := make([]uint16, 85)
	n, _, _ := procGetUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if n == 0 {
		return LangSystem
	}
	name := windows.UTF16ToString(buf)
	// 形如 "zh-CN" / "zh-Hans-CN" / "en-US"。
	if strings.HasPrefix(strings.ToLower(name), "zh") {
		return LangZhCN
	}
	return LangEn
}
