//go:build darwin && cgo

package panel

/*
#include <stdlib.h>
#include "panel_darwin.h"
*/
import "C"

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// 原生文件对话框的 Go 出口（darwin 实现；其他平台见 dialog_other.go）。
//
// 为什么不肯用 Wails 的 OpenFile*Dialog：那些把 NSOpenPanel 以 sheet 挂在
// **Wails 宿主窗口**上（WailsContext.m: beginSheetModalForWindow:
// self.mainWindow），而宿主窗口的 contentView 早已被面板接管掏空——
// macOS 弹 sheet 时会把父窗口强制显示到屏幕上，用户看到的就是一块被
// 掏空的半透明窗口一直占位（2026-09-18 真机截图定位）。
//
// 这里把 sheet 挂到面板窗口自己身上：入口在面板内的备份页，面板此刻
// 必然可见，sheet 压暗的也是有内容的面板，观感与系统一致。顺带的收益：
// 失焦自动收起对 attachedSheet 已有豁免（panel_darwin.m 的
// PawClip_ReportBlur），两条逻辑在这里会合，互不惊动。

var dialogMu sync.Mutex

// RunOpenDialog 弹一个原生"打开"对话框，阻塞至关闭。
//
// 返回空串 = 用户取消（与 dialogs.go 的约定一致，取消不是错误）。
// 面板尚未建出来时同样返回空串——这个入口只在面板 UI 里出现，
// 到不了这里就说明 UI 根本没起来，按取消处理最不扰人。
//
// ⚠️ 不要在主线程调用：内部 dispatch_sync 到主队列（与 panel 包其余
// 方法同一条红线）。
func RunOpenDialog(opts DialogOptions) (string, error) {
	dialogMu.Lock()
	defer dialogMu.Unlock()

	ct := C.CString(opts.Title)
	defer C.free(unsafe.Pointer(ct))
	cd := C.CString(opts.Dir)
	defer C.free(unsafe.Pointer(cd))
	ce := C.CString(strings.Join(opts.Extensions, ","))
	defer C.free(unsafe.Pointer(ce))

	p := C.paw_open_dialog(ct, cd, cBool(opts.CanFiles), cBool(opts.CanDirs),
		cBool(opts.CanCreate), ce)
	if p == nil {
		return "", nil
	}
	defer C.paw_free(p)
	path := C.GoString(p)
	if path == "" {
		return "", fmt.Errorf("panel: 文件对话框返回了空路径")
	}
	return path, nil
}
