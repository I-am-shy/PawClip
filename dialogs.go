package main

import (
	"errors"
	"path/filepath"
	"strings"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zego/pawclip/panel"
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

// openNativeDialog 留成变量只为单测：真跑起来它会弹系统面板并阻塞。
//
// 为什么 macOS 必须走 panel.RunOpenDialog 而不是 Wails 的对话框：
// Wails 把 NSOpenPanel 以 sheet 挂在**宿主窗口**上，而宿主窗口的
// contentView 早已被面板接管掏空——macOS 弹 sheet 会把父窗口强制显示
// 到屏幕上，用户看到的就是一块被掏空的半透明窗口一直占位（备份页点
// "···"后弹访达、界面变透明，2026-09-18 真机截图定位）。原生实现把
// sheet 挂到面板窗口自己身上，压暗的是有内容的面板。
var openNativeDialog = panel.RunOpenDialog

// openWailsDialog 同样留成变量：回退路径也要能被单测钉住。
// 真实现要求 a.ctx 就绪（Wails 运行时没起来时对话框无处安放）。
var openWailsDialog = func(a *App, opts panel.DialogOptions) (string, error) {
	if a.ctx == nil {
		return "", msgf(msgErrUINotReady, nil)
	}
	if opts.CanDirs {
		return wailsrt.OpenDirectoryDialog(a.ctx, wailsrt.OpenDialogOptions{
			Title:            opts.Title,
			DefaultDirectory: opts.Dir,
		})
	}
	filters := make([]wailsrt.FileFilter, 0, len(opts.Extensions))
	for _, e := range opts.Extensions {
		filters = append(filters, wailsrt.FileFilter{DisplayName: e, Pattern: "*." + e})
	}
	return wailsrt.OpenFileDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title:            opts.Title,
		DefaultDirectory: opts.Dir,
		Filters:          filters,
	})
}

// PickExportDir 让用户选导出目录。返回空串 = 用户取消。
func (a *App) PickExportDir() (string, error) {
	if a.ctx == nil {
		return "", msgf(msgErrUINotReady, nil)
	}
	// 默认落在数据目录下的 exports（与 Export 的空 OutputDir 行为一致），
	// 这样"不选目录直接导出"和"选了默认目录"落到同一个地方。
	// 目录可能还不存在（首次导出），对话框会做存在性检查，所以先确保它在。
	def := ""
	if db := a.dbHandle(); db != nil {
		def = filepath.Join(db.Dir(), "exports")
		_ = ensureDir(def)
	}
	opts := panel.DialogOptions{
		Title:     a.T(msgPickExportDir),
		Dir:       def,
		CanDirs:   true,
		CanCreate: true,
	}
	p, err := openNativeDialog(opts)
	if errors.Is(err, panel.ErrUnsupported) {
		// 平台没有原生对话框：退回 Wails。那里的宿主窗口是正常窗口，
		// sheet 挂上去不会出现"被掏空的窗口"。
		return openWailsDialog(a, opts)
	}
	return p, err
}

// PickBackupFile 让用户选一个 .clipbak。返回空串 = 用户取消。
func (a *App) PickBackupFile() (string, error) {
	if a.ctx == nil {
		return "", msgf(msgErrUINotReady, nil)
	}
	def := a.lastBackupDir()
	opts := panel.DialogOptions{
		Title:    a.T(msgPickBackupFile),
		Dir:      def,
		CanFiles: true,
		// 同时给"只看 .clipbak"和"所有文件"两档的意图由 nativeExts 折算：
		// macOS 的 NSOpenPanel 没有"格式下拉"，含"所有文件"档时干脆不限
		// 类型——用户把包改名成 .zip 之后仍然需要能选中它（导出的包本就
		// 是 ZIP），限死扩展名等于把这条路堵掉。
		Extensions: nativeExts([]wailsrt.FileFilter{
			{DisplayName: a.T(msgDialogBackupFilter), Pattern: "*.clipbak"},
			{DisplayName: a.T(msgDialogAllFilesFilter), Pattern: "*"},
		}),
	}
	p, err := openNativeDialog(opts)
	if errors.Is(err, panel.ErrUnsupported) {
		return openWailsDialog(a, opts)
	}
	return p, err
}

// nativeExts 把 Wails 风格的过滤器模式折成原生对话框的裸扩展名列表。
//
// 规则：任何一档是 "*"（所有文件）就直接返回 nil（不限类型）——
// macOS 的打开面板没有格式下拉，两种意图无法并存，取宽不取窄，
// 理由见 PickBackupFile。模式形如 "*.clipbak" 或 "*.png;*.jpg"。
func nativeExts(fs []wailsrt.FileFilter) []string {
	var out []string
	for _, f := range fs {
		for _, p := range strings.Split(f.Pattern, ";") {
			p = strings.TrimSpace(p)
			switch {
			case p == "" || p == "*":
				return nil
			case strings.HasPrefix(p, "*."):
				if e := strings.TrimPrefix(p, "*."); e != "" {
					out = append(out, e)
				}
			case strings.HasPrefix(p, "."):
				out = append(out, strings.TrimPrefix(p, "."))
			}
		}
	}
	return out
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

// openableURL 判断一条地址能不能交给系统默认程序打开。
//
// 白名单只有三种协议，与前端 md.ts 的 safeHref 同源（两边都是**白名单不是
// 黑名单**）：黑名单挡不住 `JaVaScRiPt:`、`file:` 以及各种注册 scheme 的
// 大小写/空白变体，而这里递出去的东西是**系统会去执行**的——macOS 的 `open`
// 连一个 .app 都会替你启动。
//
// 不带协议的相对地址（草稿图片的 `blob/…`）也一律拒绝：它没有"外部"可言，
// 而放它过去只会得到一句"文件不存在"。
func openableURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, sc := range []string{"http:", "https:", "mailto:"} {
		if strings.HasPrefix(lower, sc) {
			return true
		}
	}
	return false
}

// openURLInSystem 与 revealInFileManager 一样，是"交给系统"这一步的间接层：
// 留成变量只为单测（真跑起来它会弹出浏览器）。唯一的替换者是测试。
var openURLInSystem = openExternalURL

// OpenURL 用系统默认程序打开一条外部链接（草稿正文里点一条链接）。
//
// # 为什么必须由后端打开
//
// 草稿正文里的 `<a href>` 不能让 WebView 自己导航：这个 WebView **就是应用
// 界面本身**，跟着链接走一趟等于把整个面板换成网页，用户回不来。所以前端
// 一律 preventDefault，把地址送到这里。
//
// # 为什么先收起面板
//
// 与 revealAndStepBack 同一个坑：浏览器被拉起来 → 本 App 失去活跃 → 面板
// （非不透明窗口）在"App 已经不活跃"的时刻被 orderOut，回来时就是一块透明
// 空壳。先收再交，让 orderOut 落在面板仍持有键盘的那一刻。
func (a *App) OpenURL(raw string) error {
	if !openableURL(raw) {
		return msgf(msgErrOpenURLBad, nil)
	}
	a.hideBeforeReveal()
	return openURLInSystem(strings.TrimSpace(raw))
}
