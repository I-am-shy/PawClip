//go:build darwin && cgo

package panel

/*
#include "panel_darwin.h"
*/
import "C"

// 本文件是 cgo 的**回调出口**：C 侧（panel_darwin.m 的 Carbon 热键处理与
// AppKit 菜单动作）通过这两个函数把事件交回 Go。
//
// 它们跑在 App 的主线程上，所以只做一件事：把事件丢进 channel
// （见 dispatch.go 里的 emitHotkey / emitAction）。回调必须立刻返回。

//export pawGoHotkey
func pawGoHotkey() { emitHotkey() }

//export pawGoTrayAction
func pawGoTrayAction(action C.int) { emitAction(Action(action)) }
