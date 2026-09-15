package main

import "runtime"

// platformName 是写进备份清单 `platform` 字段的名字。
//
// 刻意不用 runtime.GOOS 直接写（那是 "darwin"/"windows"）：清单是**给人看也
// 给跨平台迁移判断用**的，用 "macos" 比 "darwin" 清楚。两边不一致的风险是
// "导入到另一平台上时显示了个谁都不认识的平台名"，所以集中在这里定义、
// 只此一处。
func platformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

// isMacOS 用于决定若干"只有 macOS 才有"的行为（辅助功能授权、LSUIElement）。
func isMacOS() bool { return runtime.GOOS == "darwin" }
