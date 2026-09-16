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
	return revealPath(dir)
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
	return revealPath(p)
}

// dbHandle 是一个小助手：取当前数据库句柄（可能为 nil）。
func (a *App) dbHandle() *store.DB {
	a.initMu.Lock()
	db := a.db
	a.initMu.Unlock()
	return db
}
