package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// fakePanel 是 panel.Controller 的测试替身。
//
// 它有双重身份：
//
//  1. **接口契约的可执行文档**：真机上跑不到的分支（热键被占用、没有
//     辅助功能授权、平台不支持）在这里都能被触发。
//  2. 让"托盘菜单内容"和"空闲收起"这两件事可以脱离 AppKit 断言 ——
//     它们在真机上只能靠肉眼，而"勾选态反了"这种错肉眼很容易放过。
type fakePanel struct {
	mu sync.Mutex

	attached bool
	visible  bool
	traySet  bool
	trayIcon int
	menu     []panel.TrayItem

	hotkey       string
	hotkeyErr    error
	hotkeyCalls  int
	unregistered int

	canAutoPaste bool
	autoPasteErr error
	autoPastes   int

	showErr error
	diag    string

	closed    bool
	accessory int
}

func (f *fakePanel) Attach(panel.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = true
	return nil
}

func (f *fakePanel) Show() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.showErr != nil {
		return f.showErr
	}
	f.visible = true
	return nil
}

func (f *fakePanel) Hide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = false
}

func (f *fakePanel) Visible() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visible
}

// Drag 是无操作：测试替身没有窗口可拖。
func (f *fakePanel) Drag() {}

func (f *fakePanel) RegisterHotkey(combo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hotkeyCalls++
	if f.hotkeyErr != nil {
		// 真实实现的契约是"先卸后装"：失败之后旧热键也没了。
		f.hotkey = ""
		return f.hotkeyErr
	}
	f.hotkey = combo
	return nil
}

func (f *fakePanel) UnregisterHotkey() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered++
	f.hotkey = ""
}

func (f *fakePanel) SetTray(iconPNG []byte, items []panel.TrayItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traySet = true
	f.trayIcon = len(iconPNG)
	f.menu = append([]panel.TrayItem(nil), items...)
	return nil
}

func (f *fakePanel) UpdateTrayMenu(items []panel.TrayItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.menu = append([]panel.TrayItem(nil), items...)
	return nil
}

func (f *fakePanel) RemoveTray() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traySet = false
}

func (f *fakePanel) AutoPaste() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoPastes++
	return f.autoPasteErr
}

func (f *fakePanel) CanAutoPaste() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canAutoPaste
}

func (f *fakePanel) RequestAutoPaste() {}

func (f *fakePanel) SetActivationPolicyAccessory() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessory++
	return nil
}

func (f *fakePanel) Diag() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diag == "" {
		return "{}"
	}
	return f.diag
}

func (f *fakePanel) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakePanel) menuOf(t *testing.T, action panel.Action) panel.TrayItem {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.menu {
		if !it.Separator && it.Action == action {
			return it
		}
	}
	t.Fatalf("托盘菜单里找不到动作 %v（菜单：%+v）", action, f.menu)
	return panel.TrayItem{}
}

// newTrayApp 造一个只装好设置与面板替身的 App（不碰磁盘、不碰 Wails）。
func newTrayApp(t *testing.T, enabled bool, lang string) (*App, *fakePanel) {
	t.Helper()
	fp := &fakePanel{}
	s := store.DefaultSettings()
	s.Capture.Enabled = enabled
	s.UI.Language = lang

	a := &App{log: discardLogger(), settings: s}
	a.ctrl = fp
	return a, fp
}

// TestTrayItems_PauseStateIsCheckedNotRenamed 钉住托盘"暂停记录"的表示方式。
//
// 设计选择：**勾选态承载状态，而不是改文案**。理由是文案会随状态变化，
// 用户下次找同一项时要重新读一遍；勾选态的位置是固定的。
//
// 这条测试防的是"把语义写反"——勾上代表已暂停，而不是代表"记录中"。
// 反了的话，用户看到勾就是以为在记录，实际是暂停，然后发现"东西丢了"。
func TestTrayItems_PauseStateIsCheckedNotRenamed(t *testing.T) {
	// ① 记录中：菜单项文案是"暂停记录"，未勾选。
	a, fp := newTrayApp(t, true, "zh-CN")
	a.installTray()

	item := fp.menuOf(t, panel.ActionToggleCapture)
	if item.Checked {
		t.Error("正在记录时，菜单项不该是勾选态（勾上代表已暂停）")
	}
	if item.Label != a.T(msgTrayPause) {
		t.Errorf("记录中的菜单文案 = %q，想要 %q", item.Label, a.T(msgTrayPause))
	}

	// ② 已暂停：文案变成"继续记录"，且是勾选态。
	b, fp2 := newTrayApp(t, false, "zh-CN")
	b.installTray()

	item2 := fp2.menuOf(t, panel.ActionToggleCapture)
	if !item2.Checked {
		t.Error("已暂停时，菜单项必须是勾选态")
	}
	if item2.Label != b.T(msgTrayResume) {
		t.Errorf("已暂停的菜单文案 = %q，想要 %q", item2.Label, b.T(msgTrayResume))
	}
}

// TestTrayItems_HasQuitAndSeparators 检查菜单结构完整性。
//
// 退出一项漏掉是很有可能的（写完"暂停/设置/统计"就收工了），
// 而用户在没有 Dock 图标（Accessory 策略）的前提下**唯一能退出应用的入口
// 就是托盘菜单**。漏了它 = 用户只能去活动监视器强杀。
func TestTrayItems_HasQuitAndSeparators(t *testing.T) {
	a, fp := newTrayApp(t, true, "zh-CN")
	a.installTray()

	fp.mu.Lock()
	defer fp.mu.Unlock()

	var hasQuit, hasShow bool
	seps := 0
	for _, it := range fp.menu {
		if it.Separator {
			seps++
			continue
		}
		switch it.Action {
		case panel.ActionQuit:
			hasQuit = true
		case panel.ActionShow:
			hasShow = true
		}
	}
	if !hasShow {
		t.Error("托盘菜单缺少「显示」项——没有 Dock 图标时这是唯一的呼出入口之一")
	}
	if !hasQuit {
		t.Error("托盘菜单缺少「退出」项——Accessory 策略下这是用户唯一能正常退出的入口")
	}
	if seps == 0 {
		t.Error("托盘菜单里没有任何分隔线，所有项挤成一坨")
	}
}

// TestInstallTray_ShipsNonEmptyIcon 验证托盘图标不是空的。
//
// 空图标在 macOS 上表现为"菜单栏里有一块看不见但能点的区域"——
// 用户找不到托盘，而代码不报任何错。所以这里断言真的取到了字节。
func TestInstallTray_ShipsNonEmptyIcon(t *testing.T) {
	if b := trayTemplatePNG(); len(b) == 0 {
		t.Fatal("取不到托盘模板图（assets/icon/dist/tray/trayTemplate@2x.png 是否还在？）")
	}
	a, fp := newTrayApp(t, true, "zh-CN")
	a.installTray()
	if fp.trayIcon == 0 {
		t.Error("SetTray 收到的图标是空的")
	}
}

// TestEventsQueue_DrainsAndCaps 验证事件队列。
//
// 队列有上界是必须的：这些事件由原生线程推入、由前端轮询取走。
// 如果前端因为某种原因（页面崩了、在后台被节流）长时间不取，
// 无上界的队列会一直涨——一个"用户不用它就不占内存"的组件不该有这种形态。
func TestEventsQueue_DrainsAndCaps(t *testing.T) {
	eventMu.Lock()
	eventQ = nil
	eventMu.Unlock()

	a := &App{}
	if got := a.TakeEvents(); got != nil {
		t.Errorf("空队列应返回 nil，得到 %+v", got)
	}

	for i := 0; i < eventMax+10; i++ {
		pushEvent(EventShow, "")
	}
	got := a.TakeEvents()
	if len(got) != eventMax {
		t.Errorf("队列长度 = %d，想要 %d（应有上界）", len(got), eventMax)
	}
	// 取走即清空
	if again := a.TakeEvents(); again != nil {
		t.Errorf("取走后队列应为空，得到 %d 条", len(again))
	}
}

// TestHandlePanelAction_QuitAndShow 检查动作分派。
func TestHandlePanelAction_QuitAndShow(t *testing.T) {
	a, fp := newTrayApp(t, true, "zh-CN")

	// Show 要真的把面板显示出来，并且留下一个 show 事件。
	a.handlePanelAction(panel.ActionShow)
	if !fp.Visible() {
		t.Fatal("ActionShow 之后面板应当可见")
	}
	evs := a.TakeEvents()
	if len(evs) == 0 || evs[len(evs)-1].Type != EventShow {
		t.Errorf("ActionShow 之后的事件 = %+v，想要最后一条是 %q", evs, EventShow)
	}

	// 设置/统计/导出/关于：都是"呼出面板 + 告诉前端切视图"，不在这里改库。
	for _, tc := range []struct {
		act  panel.Action
		want string
	}{
		{panel.ActionSettings, EventSettings},
		{panel.ActionStats, EventStats},
		{panel.ActionBackup, EventBackup},
		{panel.ActionAbout, EventAbout},
	} {
		fp.Hide()
		a.handlePanelAction(tc.act)
		evs := a.TakeEvents()
		if !hasEvent(evs, tc.want) {
			t.Errorf("动作 %v 的事件 = %+v，想要含 %q", tc.act, evs, tc.want)
		}
		if !fp.Visible() {
			t.Errorf("动作 %v 应当顺带呼出面板", tc.act)
		}
	}
}

// TestTogglePanel_HidesWhenVisible 钉住热键语义。
//
// 热键最常见的误按是"面板已经开着、我又按了一次"——那时用户想要的是收起。
// 做成"总是呼出"会让面板永远关不掉（只能等空闲超时）。
func TestTogglePanel_HidesWhenVisible(t *testing.T) {
	a, fp := newTrayApp(t, true, "zh-CN")

	a.togglePanel()
	if !fp.Visible() {
		t.Fatal("第一次按热键应当呼出面板")
	}
	a.TakeEvents()

	a.togglePanel()
	if fp.Visible() {
		t.Fatal("面板已可见时再按热键应当收起")
	}
	if !hasEvent(a.TakeEvents(), EventHide) {
		t.Error("收起时应当留下 hide 事件")
	}
}

// TestTogglePanel_ReportsKeyboardFailure 验证"显示成功但拿不到键盘"要被上报。
//
// 这是 M0 实测里那个坑的形态：面板看得见、但按键打到前台 App 去了。
// 静默吞掉它会让用户以为"PawClip 的搜索框坏了"。
func TestTogglePanel_ReportsKeyboardFailure(t *testing.T) {
	a, fp := newTrayApp(t, true, "zh-CN")
	fp.showErr = errors.New("activation policy is not accessory")

	a.togglePanel()
	evs := a.TakeEvents()
	if len(evs) != 1 || evs[0].Type != EventShow {
		t.Fatalf("事件 = %+v，想要一条 show", evs)
	}
	if !strings.Contains(evs[0].Note, "accessory") {
		t.Errorf("show 事件的 note = %q，应当带上失败原因（前端要展示它）", evs[0].Note)
	}
}

// TestIdleTick_HidesOnlyWhenOverThreshold 检查空闲收起的边界。
//
// 三个必须成立的分支：
//   - 阈值 <= 0 时什么都不做（用户显式关掉了这个行为）
//   - 面板不可见时不做事
//   - 未到阈值不收起
func TestIdleTick_HidesOnlyWhenOverThreshold(t *testing.T) {
	// ① 阈值 0 → 不做事
	a, fp := newTrayApp(t, true, "zh-CN")
	fp.visible = true
	a.settings.UI.WindowIdleDestroySec = 0
	a.lastActivity.Store(time.Now().Add(-time.Hour).Unix())
	a.idleTick()
	if !fp.Visible() {
		t.Error("阈值为 0 时不该收起面板")
	}

	// ② 面板不可见 → 不做事（也不该累加计数）
	a.settings.UI.WindowIdleDestroySec = 1
	fp.visible = false
	a.lastActivity.Store(time.Now().Add(-time.Hour).Unix())
	before := idleHides.Load()
	a.idleTick()
	if idleHides.Load() != before {
		t.Error("面板不可见时空闲 tick 不该计数")
	}

	// ③ 未到阈值 → 不收起
	fp.visible = true
	a.lastActivity.Store(time.Now().Unix())
	a.idleTick()
	if !fp.Visible() {
		t.Error("刚有活动就不该收起面板")
	}

	// ④ 超过阈值 → 收起
	a.lastActivity.Store(time.Now().Add(-time.Hour).Unix())
	a.idleTick()
	if fp.Visible() {
		t.Error("超过阈值应当收起面板")
	}
	if !hasEvent(a.TakeEvents(), EventIdleHidden) {
		t.Error("空闲收起应当留下 idleHidden 事件")
	}
}

// TestPanelLifecycle_IsHonestAboutDestroy 钉住一件事：不能假装支持销毁。
//
// docs/DESIGN.md §14 第 10 条要求"验证销毁是真的"，而 Wails v2 没有窗口销毁 API。
// 与其写一个"看起来在销毁"的实现，不如让报告如实说明——
// 这条测试保证没人会顺手把 DestroySupported 改成 true 而没真的实现它。
func TestPanelLifecycle_IsHonestAboutDestroy(t *testing.T) {
	a, fp := newTrayApp(t, true, "zh-CN")
	a.settings.UI.WindowIdleDestroySec = 300
	fp.visible = true
	a.touchActivity()

	r := a.PanelLifecycle()
	if r.DestroySupported {
		t.Error("DestroySupported 必须是 false：Wails v2.16.0 的 runtime 只导出 " +
			"Show/Hide/Quit，没有窗口销毁与重建 API。改成 true 之前先真的实现它")
	}
	if r.IdleDestroySec != 300 {
		t.Errorf("IdleDestroySec = %d，想要 300", r.IdleDestroySec)
	}
	if !r.Visible {
		t.Error("Visible 应为 true")
	}
	if !strings.Contains(r.Note, "Wails") {
		t.Errorf("Note = %q，应当说明为什么不是真销毁", r.Note)
	}
	// RSS 读不到时应为 0（前端显示"未知"），而不是编一个数。
	if r.RSSBytes < 0 {
		t.Errorf("RSSBytes = %d，读不到应当是 0 而不是负数", r.RSSBytes)
	}
}

// TestProcessRSSBytes_ReadsOwnProcess 确认我们能真的读到自己的内存。
//
// 这条是"内存验收"的地基：如果 processRSSBytes 永远返回 0，
// 那么 PanelLifecycle 的 rssBytes 字段就只是装饰，验收会变成空话。
//
// 注意断言的是**自己**这个进程：别的进程要走 `ps`，而 `ps` 在某些环境里
// 是不可用的（本项目开发沙箱就是，实测 "operation not permitted: ps"）。
// 所以"读不到别的进程返回 0"也是契约的一部分，两个方向都要测。
func TestProcessRSSBytes_ReadsOwnProcess(t *testing.T) {
	rss := processRSSBytes(os.Getpid())
	if rss <= 0 {
		t.Fatalf("processRSSBytes(自己) = %d，读不到就说明这条链路是坏的"+
			"（macOS 上应当走 Mach task_info，见 rss_darwin_cgo.go）", rss)
	}
	// 一个跑着 Go 测试的进程不可能只有几 KB 常驻内存；这个下界能抓住
	// "把字段读错位"这类错误（例如把 rss 当成了别的字段）。
	if rss < 1<<20 {
		t.Errorf("自己的 RSS = %d 字节（< 1MB），这个数字明显不对", rss)
	}

	// 不存在的 pid：返回 0（= 未知），而不是编一个数。
	if got := processRSSBytes(1 << 30); got != 0 {
		t.Errorf("对不存在的 pid 返回 %d，想要 0", got)
	}
}

func hasEvent(evs []PanelEvent, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}
