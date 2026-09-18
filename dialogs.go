package main

import (
	"path/filepath"
	"strings"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zego/pawclip/store"
)

// 本文件收"必须弹系统对话框"的几个绑定。
//
// 为什么放在这里而不是让前端传路径：浏览器沙箱里前端**拿不到真实路径**
// （input[type=file] 只给 File 对象，没有绝对路径），这是刻意的安全设计。
// 所以"选一个目录/文件"这件事只能由 Go 侧弹原生对话框来做。
//
// 三个绑定都返回空串表示**用户取消了**，而不是报错：取消是正常操作，
// 前端据此静默返回即可，不该弹一个"操作失败"。

// PickExportDir 让用户选导出目录。返回空串 = 用户取消。
func (a *App) PickExportDir() (string, error) {
	if a.ctx == nil {
		return "", msgf(msgErrUINotReady, nil)
	}
	// 默认落在数据目录下的 exports（与 Export 的空 OutputDir 行为一致），
	// 这样"不选目录直接导出"和"选了默认目录"落到同一个地方。
	// 目录可能还不存在（首次导出），OpenDirectoryDialog 会做存在性检查，
	// 所以先确保它在。
	def := ""
	if db := a.dbHandle(); db != nil {
		def = filepath.Join(db.Dir(), "exports")
		_ = ensureDir(def)
	}
	return wailsrt.OpenDirectoryDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title:            a.T(msgPickExportDir),
		DefaultDirectory: def,
	})
}

// PickBackupFile 让用户选一个 .clipbak。返回空串 = 用户取消。
func (a *App) PickBackupFile() (string, error) {
	if a.ctx == nil {
		return "", msgf(msgErrUINotReady, nil)
	}
	def := a.lastBackupDir()
	return wailsrt.OpenFileDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title:            a.T(msgPickBackupFile),
		DefaultDirectory: def,
		Filters: []wailsrt.FileFilter{
			// 同时给"只看 .clipbak"和"所有文件"两档：
			// 用户把包改名成 .zip 之后仍然需要能选中它（导出的包本就是 ZIP）。
			{DisplayName: a.T(msgDialogBackupFilter), Pattern: "*.clipbak"},
			{DisplayName: a.T(msgDialogAllFilesFilter), Pattern: "*"},
		},
	})
}

// lastBackupDir 猜一个合适的"上次用过的地方"。
//
// 不做持久化记录：那要新增一个设置键、还要处理"目录已被删除"。
// 用导出目录当默认值已经覆盖了绝大多数情况（用户导出完就会想导回来）。
func (a *App) lastBackupDir() string {
	if db := a.dbHandle(); db != nil {
		return filepath.Join(db.Dir(), "exports")
	}
	return ""
}

// revealInFileManager 是"把路径交给系统文件管理器"这一步的间接层。
//
// 留成变量只为单测：真跑起来它会弹出访达/资源管理器，而这里要断言的恰恰是
// **弹出去之前面板已经收起了**（见 revealAndStepBack）。唯一的替换者是测试。
var revealInFileManager = revealPath

// revealAndStepBack 打开文件管理器之前的**唯一出口**：先收起面板，再交出去。
//
// # 为什么必须"先收，再交"
//
// 面板窗口是**非不透明窗口**（panel_darwin.m: setOpaque:NO + clearColor，
// 为的是让系统投影跟着圆角卡片走），它自己一个像素都不画，可见性全靠
// WKWebView 画内容。代价是：**内容没合成的那一刻，这个窗口就是一块透明的
// 空壳**——看起来就是"悬在别的东西上面的一个透明窗口"。
//
// "点开文件管理器"正好是最容易踩进那个状态的一条路：
//
//	① 面板此刻持有键盘（App 活跃），用户点了文件链接 / 打开数据目录；
//	② `open` 让访达（资源管理器）激活 → 本 App 失去活跃 → 面板丢 key；
//	③ "失焦自动收起"要等 80ms 后的复核才发现焦点没了，于是 orderOut
//	   落在了 **App 已经不活跃**的时刻。这时候窗口被重新呼出，WebKit
//	   未必马上重新合成内容，用户回头看到的就是那个透明空壳。
//
// 在这里主动收起，让 orderOut 发生在面板仍持有键盘、App 仍活跃的时刻，
// 从源头上不进那个状态；顺带也去掉了原来"访达都弹出来了、面板还赖着
// 100ms 才消失"的观感。
//
// 只在 ui.closeOnBlur 为真时收起：把这一项关掉的用户要的就是"面板钉在
// 屏幕上"，替他做决定等于让那个设置失效。
func (a *App) revealAndStepBack(target string) error {
	a.hideBeforeReveal()
	return revealInFileManager(target)
}

// hideBeforeReveal 是 revealAndStepBack 里"收起"的那一半（策略与副作用分开，
// 单测直接钉这一半）。面板本来就不可见时什么都不做。
func (a *App) hideBeforeReveal() {
	if !a.uiSettings().CloseOnBlur {
		return
	}
	ctrl := a.panelController()
	if ctrl == nil || !ctrl.Visible() {
		return
	}
	a.HidePanel()
	pushEvent(EventHide, "")
}

// RevealDataDir 在系统文件管理器里打开数据目录。
//
// 用途是设置页的"打开数据目录"：用户想自己备份 pawclip.db 或清理 blobs 时
// 不必去记 ~/Library/Application Support/PawClip 这种路径。
func (a *App) RevealDataDir() error {
	db := a.dbHandle()
	if db == nil {
		return msgf(msgErrNotReady, nil)
	}
	dir := db.Dir()
	if err := ensureDir(dir); err != nil {
		return err
	}
	return a.revealAndStepBack(dir)
}

// RevealPath 在文件管理器里定位一个任意路径（导出的包用得上）。
//
// 路径必须在数据目录内——这个绑定是前端可调的，不设限就等于给了一个
// "任意路径打开"的原语。约束在这里而不是注释里。
func (a *App) RevealPath(p string) error {
	db := a.dbHandle()
	if db == nil {
		return msgf(msgErrNotReady, nil)
	}
	p = filepath.Clean(strings.TrimSpace(p))
	if p == "" {
		return msgf(msgErrEmptyPath, nil)
	}
	root := filepath.Clean(db.Dir())
	// 用 Rel 判断包含关系而不是 strings.HasPrefix：
	// HasPrefix("/a/bc", "/a/b") 为真，会把边界外的路径放过去。
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return msgf(msgErrOutsideData, nil)
	}
	return a.revealAndStepBack(p)
}

// dbHandle 是一个小助手：取当前数据库句柄（可能为 nil）。
func (a *App) dbHandle() *store.DB {
	a.initMu.Lock()
	db := a.db
	a.initMu.Unlock()
	return db
}
