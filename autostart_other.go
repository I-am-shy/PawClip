//go:build !darwin && !windows

package main

// Linux（以及其它非 macOS/Windows 平台）没有自启实现。
//
// ⚠️ 这里返回**错误**而不是 no-op 成功，是有意的：AutoStartSupported()
// 已经在上层挡住了这些平台，所以本函数的错误只会出现在"有人绕过那个检查
// 直接调"的时候。那时静默返回 nil 会让调用方以为自启真的开上了，
// 而实际什么也没发生——正是最难查的那类 bug。
//
// 返回错误则是一个立刻可见的失败。
func autoStartEnabled() (bool, error) { return false, errAutoStartUnsupported }

func setAutoStart(bool) error { return errAutoStartUnsupported }

func autoStartDiag() string { return "unsupported" }

var errAutoStartUnsupported = msgf(msgErrAutoStartNo, nil)
