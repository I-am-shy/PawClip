package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zego/pawclip/capture"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/retention"
	"github.com/zego/pawclip/store"
)

// 本文件把 App 手里那些"启动之后才存在"的资源集中起来，并实现面板回调、
// 托盘刷新与面板的空闲生命周期。
//
// 单独一个文件的原因：app.go 已经很长了，而这些东西是**装配与回调**，
// 不是生命周期本身。混在一起会让"谁在什么时刻被创建"变得难读。

// ── 资源读取器 ──────────────────────────────────────────────────
//
// 一律走 initMu，因为 startup 是在另一个 goroutine 里跑的，绑定方法可能
// 在它还没跑完时就被前端调用了（Wails 不等 startup 返回就允许 JS 调绑定）。

func (a *App) blobStore() *store.BlobStore {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	return a.blobs
}

// blobsReady 报告 blob 存储是否可用。
func (a *App) blobsReady() bool { return a.blobStore() != nil }

// blobAbs 把相对路径转成绝对路径；未就绪时返回空串。
func (a *App) blobAbs(rel string) string {
	bs := a.blobStore()
	if bs == nil || rel == "" {
		return ""
	}
	return bs.Abs(rel)
}

// writeback 返回回写器。
func (a *App) writeback() (*Writeback, error) {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	if a.wb == nil {
		return nil, msgf(msgErrNotReady, nil)
	}
	return a.wb, nil
}

// panelController 返回面板控制器；未就绪时返回 nil（调用方要判空）。
//
// 为什么不返回 error：面板在 Linux 上根本不存在（只有骨架实现），
// 而"没有面板"不该是一串错误——绑定方法返回 ErrUnsupported 就够表达了。
func (a *App) panelController() panel.Controller {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	return a.ctrl
}

func (a *App) gcEngine() *retention.GC {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	return a.gc
}

func (a *App) capturePipeline() *capture.Capture {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	return a.cap
}

// uiSettings 是给回写器用的实时设置读取器。
//
// 用函数而不是快照：回写是低频动作，每次读一次锁可以忽略；而快照会让
// "刚在设置里关掉恢复原剪贴板，下一次回写却还在恢复"。
func (a *App) uiSettings() store.UISettings {
	if s := a.Settings(); s != nil {
		return s.UI
	}
	return store.DefaultSettings().UI
}

// retentionSettings 同理，给 GC 用。
func (a *App) retentionSettings() store.RetentionSettings {
	if s := a.Settings(); s != nil {
		return s.Retention
	}
	return store.DefaultSettings().Retention
}

// gcConfig 把设置投影成 GC 的配置。GC 每轮开头调它一次。
func (a *App) gcConfig() retention.Config {
	r := a.retentionSettings()
	return retention.Config{
		DefaultTTLSec:     r.DefaultTTLSec,
		OnExpire:          r.OnExpire,
		TrashTTLSec:       r.TrashTTLSec,
		MaxItems:          r.MaxItems,
		MaxDiskBytes:      r.MaxDiskBytes,
		GCIntervalSec:     r.GCIntervalSec,
		VacuumAfterDelete: true,
	}
}

// ── 面板回调（实现 panel.Handler）───────────────────────────────
//
// ⚠️ 这两个方法都在**非主线程**上被调用（热键在 Carbon 事件处理里、
// 托盘在 AppKit 菜单动作里）。所以它们**绝不能直接碰 WebView**：
// 跨线程调 Wails 的运行时方法是未定义行为（轻则丢消息，重则崩溃）。
//
// 做法是"记事件 + 让前端来取"：事件进队列，前端在轮询里拿走。
// 这比从原生线程直接 eval JS 啰嗦，但它是唯一不会炸的做法。

type panelHandler struct{ app *App }

func (h *panelHandler) OnAction(act panel.Action) { h.app.handlePanelAction(act) }
func (h *panelHandler) OnHotkey()                 { h.app.togglePanel() }
func (h *panelHandler) OnPanelBlur()              { h.app.onPanelBlur() }

// PanelEvent 是给前端的原生侧事件。
type PanelEvent struct {
	Type string `json:"type"`
	At   int64  `json:"at"`
	Note string `json:"note,omitempty"`
}

const (
	// EventShow 面板被呼出（前端据此刷新列表、聚焦搜索框）。
	EventShow = "show"
	// EventHide 面板被收起。
	EventHide = "hide"
	// EventSettings 用户从托盘/菜单点了设置。
	EventSettings = "settings"
	// EventStats 用户从托盘/菜单点了统计。
	EventStats = "stats"
	// EventBackup 用户从托盘/菜单点了导出/导入。
	EventBackup = "backup"
	// EventAbout 关于。
	EventAbout = "about"
	// EventCaptureToggled 记录开关被切换（前端据此提示并刷新）。
	EventCaptureToggled = "captureToggled"
	// EventIdleHidden 面板因空闲被收起。
	EventIdleHidden = "idleHidden"
	// EventNotice 是一条"纯提示"。
	//
	// 与其它事件的区别：它不改变视图，只让前端把 Note 弹成一句 toast。
	// 存在的理由是那些**后端才知道的话**——导出/导入的结果报告、
	// 连续粘贴还剩几条。它们由后端渲染（要跟着语言走），
	// 前端拿到的是已经翻好的一句话，所以不叫 "error"/"report"
	// 而是一个中性的 notice。
	EventNotice = "notice"
)

var (
	eventMu  sync.Mutex
	eventQ   []PanelEvent
	eventMax = 32
)

func pushEvent(t string, note string) {
	eventMu.Lock()
	defer eventMu.Unlock()
	if len(eventQ) >= eventMax {
		// 队列满就丢最老的。这些事件是"提示前端做点什么"，
		// 丢老的比把最新的挡在外面要好。
		eventQ = eventQ[1:]
	}
	eventQ = append(eventQ, PanelEvent{Type: t, At: time.Now().Unix(), Note: note})
}

// TakeEvents 让前端取走待处理事件（取走即清空）。
//
// 前端在 interval 里调它。用轮询而不是 Wails 的 EventsEmit：后者从非主线程
// 调用并不安全，而这个队列只有几十个字节，1 秒轮一次的开销可以忽略。
func (a *App) TakeEvents() []PanelEvent {
	eventMu.Lock()
	defer eventMu.Unlock()
	if len(eventQ) == 0 {
		return nil
	}
	out := eventQ
	eventQ = nil
	return out
}

// handlePanelAction 处理托盘 / 菜单动作。
func (a *App) handlePanelAction(act panel.Action) {
	// 任何一次交互都算"用户还在"，把空闲计时重置。
	a.touchActivity()

	switch act {
	case panel.ActionShow:
		a.showPanel()
	case panel.ActionToggleCapture:
		a.toggleCapture()
	case panel.ActionQuit:
		// Quit 必须在主线程语义上执行；Wails 的 Quit 自己会切。
		if a.ctx != nil {
			wailsrt.Quit(a.ctx)
		} else {
			os.Exit(0)
		}
	case panel.ActionSettings:
		pushEvent(EventSettings, "")
		a.showPanel()
	case panel.ActionStats:
		pushEvent(EventStats, "")
		a.showPanel()
	case panel.ActionBackup:
		pushEvent(EventBackup, "")
		a.showPanel()
	case panel.ActionAbout:
		pushEvent(EventAbout, "")
		a.showPanel()
	}
}

// togglePanel 是热键的语义：可见就收起，不可见就呼出。
//
// 不做"总是呼出"：热键最常见的误按场景是"面板已经开着、我又按了一次"，
// 那时用户的意图是收起。
func (a *App) togglePanel() {
	a.touchActivity()
	ctrl := a.panelController()
	if ctrl == nil {
		return
	}
	if ctrl.Visible() {
		a.HidePanel() // 唯一出口：顺带把面板尺寸落盘
		pushEvent(EventHide, "")
		return
	}
	a.showPanel()
}

func (a *App) showPanel() {
	ctrl := a.panelController()
	if ctrl == nil {
		return
	}
	if err := ctrl.Show(); err != nil {
		// 面板已经显示了，只是可能收不到键盘。把这个事实告诉前端，
		// 让它在自己身上放一个可见的提示，而不是静默地"能看不能敲"。
		a.log.Warn("面板显示时未能取得键盘焦点", "err", err)
		pushEvent(EventShow, err.Error())
		return
	}
	pushEvent(EventShow, "")
}

// onPanelBlur 处理"面板丢掉了键盘焦点"（用户点了面板以外的任何地方）。
//
// 这是 §9 的 ui.closeOnBlur：把面板当成 Spotlight 那样的临时浮层——
// 视线/鼠标移开就意味着"不用了"，让它自己退场，而不是留在屏幕上挡着。
//
// 三个不该收起的时刻：
//
//	· 面板本来就不可见。原生已经过滤了一道，但从"上报"到"执行"之间有
//	  一段跨线程的路（原生 → channel → 这个 goroutine），而收起面板本身
//	  也会让窗口 resign key，所以这里必须再判一次。
//	· 用户在设置里关掉了这个行为（有人就喜欢面板钉在屏幕上）。
//	· 没有面板可用（Linux 骨架）——panelController 为 nil。
//
// 收起走的是 HidePanel（= 前端 ✕ 按钮那条路）：先落盘尺寸再收起。
// 用户完全可能把面板拖大、点一下别处让它自己消失，尺寸不能因此丢掉。
func (a *App) onPanelBlur() {
	ctrl := a.panelController()
	if ctrl == nil || !ctrl.Visible() {
		return
	}
	if !a.uiSettings().CloseOnBlur {
		return
	}
	a.log.Debug("面板失去焦点，自动收起", "note", "ui.closeOnBlur")
	a.HidePanel()
	pushEvent(EventHide, "")
}

// toggleCapture 切换"暂停记录"并刷新托盘勾选态。
func (a *App) toggleCapture() {
	s := a.Settings()
	if s == nil {
		return
	}
	next := !s.Capture.Enabled
	if err := a.SetSetting("capture.enabled", boolJSON(next)); err != nil {
		a.log.Error("切换记录开关失败", "enabled", next, "err", err)
		return
	}
	// 设置改完要**立刻**作用到捕获链路：Capture 的过滤器是启动时固化的，
	// 所以这里显式推一次。漏掉这一步的表现是"开关动了、却还在记录"。
	if cap := a.capturePipeline(); cap != nil {
		if err := cap.ApplyFilter(filterConfigFrom(a.Settings())); err != nil {
			a.log.Warn("把新的过滤设置推给捕获链路失败", "err", err)
		}
	}
	a.refreshTray()
	note := a.T(msgNotifyCaptureOn)
	if !next {
		note = a.T(msgNotifyCaptureOff)
	}
	a.notify(a.T(msgTrayTooltip), note)
	pushEvent(EventCaptureToggled, note)
}

// ── 空闲生命周期 ────────────────────────────────────────────────
//
// ⚠️ 这里是本实现与 docs/DESIGN.md §14 第 10 条之间**没有完全对齐**的地方，
// 必须写清楚，不能靠注释含糊过去：
//
// DESIGN 要求"面板闲置超时即**销毁**，热键按下时**重建**"，目的是让
// WebView 子进程退出，把常驻内存压到 30 MB 以内。
//
// 但 Wails **v2.16.0 的运行时只导出 Show / Hide / Quit 三个窗口方法**
// （我逐个查过 pkg/runtime/runtime.go 的导出表），没有 WindowClose，
// 也没有"再建一个窗口"的能力——v2 是单窗口模型，v3 才有多窗口。
// 也就是说：把 WebView 所在的窗口真销毁之后，**没有任何 API 能把它建回来**，
// 面板就永久消失了。这不是"实现偷懒"，是被依赖的框架堵死的。
//
// 所以本实现的行为是：空闲到时间就把面板**收起**（把键盘还给前台 App），
// 并把实测的 RSS 与 WebView 子进程数报出来——也就是 §14 第 10 条要求的
// "验证销毁是真的"。测出来的数字如果没达标，那是事实，不该用一个
// 无法重建的"销毁"去换一个好看的内存曲线。
//
// 想真正达标有两条路，都超出 M2 范围：
//   - 升级到 Wails v3（多窗口，支持销毁重建）；
//   - 自己托管 WKWebView / WebView2，不走 Wails 的窗口（等于放弃 Wails 的
//     资源服务与绑定生成，成本很高）。
//
// 这两条都记在收尾报告的"未解风险"里。

// PanelLifecycleReport 是面板生命周期的实测快照。
type PanelLifecycleReport struct {
	// IdleDestroySec 是设置里的空闲阈值（秒）。
	IdleDestroySec int `json:"idleDestroySec"`
	// IdleSec 是当前已空闲的秒数。
	IdleSec int64 `json:"idleSec"`
	// Visible 面板当前是否可见。
	Visible bool `json:"visible"`
	// IdleHides 累计因空闲收起过几次。
	IdleHides int64 `json:"idleHides"`
	// Toggles 累计因空闲被收起后又被呼出过几次。
	Toggles int64 `json:"toggles"`
	// RSSBytes 是本进程的常驻内存（实测，不是估算）。
	RSSBytes int64 `json:"rssBytes"`
	// ChildProcs 是本进程的直接子进程数。
	ChildProcs int `json:"childProcs"`
	// WebContentProcs 是系统上 WebKit 的网页内容进程数（macOS）。
	//
	// 这是"销毁是不是假象"的判据（§14 第 10 条）：真正的销毁应当让
	// 这个数字下降。它是**全系统**计数而不是"本 App 的"——因为
	// WebKit 的内容进程不挂在我们的进程树下（是 launchd 的 XPC 服务），
	// 没法按父子关系过滤。所以这个数字只适合前后对比。
	WebContentProcs int `json:"webContentProcs"`
	// DestroySupported 恒为 false，见上面那段说明。
	DestroySupported bool `json:"destroySupported"`
	// Note 是给用户看的说明。
	Note string `json:"note"`
}

func (a *App) idleDestroySec() int {
	s := a.Settings()
	if s == nil {
		return 0
	}
	return s.UI.WindowIdleDestroySec
}

// touchActivity 记录一次用户交互。
func (a *App) touchActivity() {
	a.lastActivity.Store(time.Now().Unix())
}

var idleHides, idleToggles atomic.Int64

// startIdleWatcher 起一个每秒检查一次的 watcher。
//
// 1 秒的粒度对 300 秒的阈值完全够用，而这个 tick 本身几乎不耗电
// （一次原子读 + 一次整数比较），比把定时器做得更精细要划算。
func (a *App) startIdleWatcher(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.idleTick()
		}
	}
}

func (a *App) idleTick() {
	sec := a.idleDestroySec()
	if sec <= 0 {
		// 0 或负数表示"不因空闲做任何事"——这是个合法的用户选择
		// （有人宁可多占点内存也不要面板自己消失）。
		return
	}
	ctrl := a.panelController()
	if ctrl == nil || !ctrl.Visible() {
		return
	}
	idle := time.Now().Unix() - a.lastActivity.Load()
	if idle < int64(sec) {
		return
	}
	a.HidePanel() // 唯一出口：顺带把面板尺寸落盘
	pushEvent(EventIdleHidden, "")
	idleHides.Add(1)
	a.log.Info("面板已按空闲阈值收起",
		"idleSec", idle, "thresholdSec", sec,
		"note", "Wails v2 无窗口销毁 API，故为收起而非销毁；见 PanelLifecycleReport")
}

// PanelLifecycle 返回面板生命周期的实测快照。
func (a *App) PanelLifecycle() *PanelLifecycleReport {
	r := &PanelLifecycleReport{
		IdleDestroySec:   a.idleDestroySec(),
		IdleHides:        idleHides.Load(),
		Toggles:          idleToggles.Load(),
		DestroySupported: false,
	}
	if ctrl := a.panelController(); ctrl != nil {
		r.Visible = ctrl.Visible()
	}
	if v := a.lastActivity.Load(); v > 0 {
		r.IdleSec = time.Now().Unix() - v
	}
	r.RSSBytes = processRSSBytes(os.Getpid())
	r.ChildProcs = childProcessCount(os.Getpid())
	r.WebContentProcs = webContentProcessCount()
	if r.DestroySupported {
		r.Note = ""
	} else {
		r.Note = "Wails v2.16.0 只导出 Show/Hide/Quit，无窗口销毁与重建 API（单窗口模型）；" +
			"本实现改为按空闲阈值收起面板并实测内存，而不是做一个销毁后无法恢复的操作。"
	}
	return r
}

// processRSSBytes 读一个进程的常驻内存。
//
// 分两条路：
//
//   - **自己的进程**走 Mach 的 task_info（rss_darwin_cgo.go），不走子进程。
//     这条是内存验收的主路径，必须可靠。
//   - 别的进程退回 `ps`。这条只是诊断用（我们从不测别的进程的内存），
//     读不到就返回 0——沙箱里 `ps` 是不可用的（实测 "operation not
//     permitted: ps"），所以**不能**把它的成功当作前提。
//
// 用 ps/Mach 而不是 runtime.MemStats：后者只统计 Go 堆，而这里要回答的是
// "整个进程（含 cgo 侧的 AppKit、SQLite 页缓存）占了多少"。
func processRSSBytes(pid int) int64 {
	if pid == os.Getpid() {
		if n := ownRSSBytes(); n >= 0 {
			return n
		}
		// Mach 也失败时继续往下走，用 ps 兜一次（少见，但没必要因此放弃）。
	}
	out, err := runCmd("ps", "-o", "rss=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return n * 1024 // ps 的单位是 KiB
}

// childProcessCount 数本进程的直接子进程。
//
// macOS 上 WebKit 的内容进程**不是**我们的子进程（它是 launchd 托管的
// XPC 服务），所以这个数字通常接近 0；它主要用来发现"有没有留下意料之外
// 的帮手进程"。
func childProcessCount(pid int) int {
	out, err := runCmd("pgrep", "-P", strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// ── 通知 ────────────────────────────────────────────────────────

// notify 发一个系统通知。
//
// 通知是"锦上添花"，任何失败都只记日志：它的失败绝不能让主流程失败
// （用户按暂停记录，不该因为通知发不出去而看到报错）。
func (a *App) notify(title, body string) {
	if a.ctx == nil {
		return
	}
	wailsrt.EventsEmit(a.ctx, "pawclip:notify", map[string]string{
		"title": title,
		"body":  body,
	})
}

// ── 托盘 ────────────────────────────────────────────────────────

// installTray 装托盘图标与菜单。
func (a *App) installTray() {
	ctrl := a.panelController()
	if ctrl == nil {
		return
	}
	icon := trayTemplatePNG()
	if err := ctrl.SetTray(icon, a.trayItems()); err != nil {
		// 托盘失败不影响主链路：用户还有热键。
		a.log.Warn("托盘安装失败", "err", err)
	}
}

// refreshTray 只换菜单（勾选态变了）。
func (a *App) refreshTray() {
	ctrl := a.panelController()
	if ctrl == nil {
		return
	}
	if err := ctrl.UpdateTrayMenu(a.trayItems()); err != nil {
		a.log.Warn("刷新托盘菜单失败", "err", err)
	}
}

// trayItems 按当前设置生成托盘菜单。
func (a *App) trayItems() []panel.TrayItem {
	paused := true
	if s := a.Settings(); s != nil {
		paused = !s.Capture.Enabled
	}

	// "暂停记录"的语义：**勾上代表已暂停**。
	// 用勾选态而不是改文案，是因为菜单项的文字会随状态变化，
	// 用户下次找同一项时要重新读一遍；勾选态的位置是固定的。
	pauseLabel := a.T(msgTrayPause)
	if paused {
		pauseLabel = a.T(msgTrayResume)
	}

	return []panel.TrayItem{
		panel.Item(a.T(msgTrayShow), panel.ActionShow),
		{Label: pauseLabel, Action: panel.ActionToggleCapture, Checked: paused},
		panel.Separator(),
		panel.Item(a.T(msgTraySettings), panel.ActionSettings),
		panel.Item(a.T(msgTrayStats), panel.ActionStats),
		panel.Item(a.T(msgTrayBackup), panel.ActionBackup),
		panel.Separator(),
		panel.Item(a.T(msgTrayAbout), panel.ActionAbout),
		panel.Item(a.T(msgTrayQuit), panel.ActionQuit),
	}
}

// ── 小工具 ──────────────────────────────────────────────────────

// dataFileAbs 把数据目录下的相对路径转成绝对路径。
func (a *App) dataFileAbs(rel string) string {
	dir := a.DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, filepath.FromSlash(rel))
}
