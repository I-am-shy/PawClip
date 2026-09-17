// 全局热键这条设置路径的测试。
//
// 为什么它值得单独一个文件：热键是**唯一会失败的设置项**——别的键写进库就
// 一定生效，而热键要先在系统里占住一个组合，占不上就是失败。三条路径
// （清空 / 不合法 / 被占用）各有一个"看不出来但很致命"的错法：
//
//	清空  → 掉进 RegisterHotkey("") 被报成"被别的应用占用"（老实现就是这样）
//	不合法 → 坏值落库，下次启动静默没有热键
//	被占用 → 库里写着新组合，系统里一个都没装（设置与事实脱钩）
//
// 这三种都不会崩、不会报错日志，只会让用户"按了没反应"，所以必须钉住。
//
// 文件下半部分是另一半故事：**让出 / 收回**。设置页的热键控件在"输入态"里
// 要读键盘，而系统级热键会在事件分发之前把按键吃掉，所以那几秒必须先把热键
// 从系统里摘下来——真机上"这个控件输入不了"就是这么来的。

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// newHotkeyApp 造一个真的有库、但面板换成替身的 App。
//
// noPlatformUI 让 initialize 跳过真正的面板接管（测试进程里等不到 Wails
// 的窗口，Attach 会白等 15 秒），然后手动把替身塞进去——热键这条路径
// 本来就是"App ↔ Controller"之间的事，替身足够。
func newHotkeyApp(t *testing.T) (*App, *fakePanel) {
	t.Helper()
	dir := t.TempDir()
	boot := DefaultBootstrap()
	boot.Database.Path = filepath.Join(dir, "paw.db")

	a := NewApp(boot, filepath.Join(dir, "config.toml"), discardLogger())
	a.noPlatformUI = true
	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(a.Shutdown)

	fp := &fakePanel{}
	a.ctrl = fp
	return a, fp
}

// TestSetHotkey_ClearIsALegalState 钉住"清空热键"这条路径。
//
// 它是一个**合法状态**：用户就是不想要全局热键了（比如和别的软件冲突），
// 设置里清空即可，托盘图标仍然能呼出面板。
//
// 老实现会把它当成一次普通的注册：RegisterHotkey("") → ParseHotkey 报
// "空的热键" → 前端收到"全局热键已被其他应用占用"。用户做的事和看到的
// 提示完全不沾边，而且会以为设置没保存。
func TestSetHotkey_ClearIsALegalState(t *testing.T) {
	a, fp := newHotkeyApp(t)

	const before = "Ctrl+Alt+P"
	if err := a.SetSetting(store.KeyUIHotkey, `"Ctrl+Alt+P"`); err != nil {
		t.Fatalf("设置热键失败：%v", err)
	}
	if fp.hotkey != before {
		t.Fatalf("注册到的热键 = %q，想要 %q", fp.hotkey, before)
	}

	fp.unregistered = 0
	if err := a.SetSetting(store.KeyUIHotkey, `""`); err != nil {
		t.Fatalf("清空热键应当成功（那是一个合法状态），却报错：%v", err)
	}
	if fp.unregistered == 0 {
		t.Error("清空热键应当把全局热键注销掉")
	}
	if fp.hotkey != "" {
		t.Errorf("清空后系统里还留着热键 %q", fp.hotkey)
	}
	if got := a.Settings().UI.Hotkey; got != "" {
		t.Errorf("设置里的热键 = %q，想要空串", got)
	}
}

// TestSetHotkey_TakenDoesNotHarmTheOldBinding 钉住"换绑失败"的收尾。
//
// RegisterHotkey 的契约是**先卸后装**，所以换绑失败时旧热键也已经没了。
// 库里不能留下那个装不上的新组合（否则设置说的和实际生效的是两回事），
// 而且旧热键必须被装回去——不然用户为了换个键点一下，结果是两个都没有。
func TestSetHotkey_TakenDoesNotHarmTheOldBinding(t *testing.T) {
	a, fp := newHotkeyApp(t)

	const old = "CmdOrCtrl+Shift+V"
	if got := a.Settings().UI.Hotkey; got != old {
		t.Fatalf("默认热键 = %q，想要 %q", got, old)
	}

	// 只有新组合被"占用"：这正是真实世界的形状。
	const taken = "Ctrl+Alt+P"
	fp.hotkeyErr = panel.ErrHotkeyTaken
	fp.hotkeyErrFor = taken

	err := a.SetSetting(store.KeyUIHotkey, `"Ctrl+Alt+P"`)
	if err == nil {
		t.Fatal("热键被占用时 SetSetting 应当报错")
	}
	// 报错要是"被占用"那句（用户看得懂，也知道该去关哪个程序），
	// 而不是底层那句 "panel: hotkey is already registered by another application"。
	if !strings.Contains(err.Error(), a.T(msgNotifyHotkeyTaken)) {
		t.Errorf("报错信息 = %q，里面没有 %q", err.Error(), a.T(msgNotifyHotkeyTaken))
	}
	if got := a.Settings().UI.Hotkey; got != old {
		t.Errorf("换绑失败后设置变成了 %q —— 库里不该留下装不上的组合", got)
	}
	if fp.hotkey != old {
		t.Errorf("换绑失败后系统里的热键 = %q，想要旧值 %q（用户不该两个都没有）", fp.hotkey, old)
	}
}

// TestSetHotkey_InvalidNeverLands 钉住"不合法组合不落库"。
//
// 坏值落库的后果是**静默**的：下次启动 attachPanel 只能降级（不注册热键），
// 用户按了没反应，而设置页里还显示着他设过的那个组合。
// 所以校验必须在写库之前，而且不能走到注册那一步。
func TestSetHotkey_InvalidNeverLands(t *testing.T) {
	a, fp := newHotkeyApp(t)
	old := a.Settings().UI.Hotkey

	for _, bad := range []string{
		`"Ctrl+Shift+NotAKey"`, // 不认识的键名
		`"V"`,                  // 没有修饰键
		`"Ctrl"`,               // 只有修饰键
		`"Ctrl+Shift+"`,        // 尾随加号
	} {
		err := a.SetSetting(store.KeyUIHotkey, bad)
		if err == nil {
			t.Errorf("SetSetting(ui.hotkey, %s) 应当报错", bad)
			continue
		}
		// 报错里要有那个组合本身，用户才知道是哪一条不对。
		combo := strings.Trim(bad, `"`)
		if !strings.Contains(err.Error(), combo) {
			t.Errorf("报错信息 = %q，里面没有 %q", err.Error(), combo)
		}
	}

	if fp.hotkeyCalls != 0 {
		t.Errorf("不合法的组合不该走到注册那一步（注册被调了 %d 次）", fp.hotkeyCalls)
	}
	if got := a.Settings().UI.Hotkey; got != old {
		t.Errorf("不合法的组合落库了：%q（想要仍是 %q）", got, old)
	}
}

// TestSetHotkey_AppliesToRunningPanel 确认换绑真的作用到了**正在运行**的面板上。
//
// 只写进库不算生效——用户按下去要有反应。这条测试盯着那次 RegisterHotkey 调用。
func TestSetHotkey_AppliesToRunningPanel(t *testing.T) {
	a, fp := newHotkeyApp(t)

	// 换到一个和默认值不同的组合：必须真的走到注册。
	if err := a.SetSetting(store.KeyUIHotkey, `"Ctrl+Alt+P"`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if fp.hotkeyCalls != 1 {
		t.Errorf("注册被调用 %d 次，想要 1 次", fp.hotkeyCalls)
	}
	if fp.hotkey != "Ctrl+Alt+P" {
		t.Errorf("注册到的热键 = %q", fp.hotkey)
	}
	if got := a.Settings().UI.Hotkey; got != "Ctrl+Alt+P" {
		t.Errorf("设置里的热键 = %q，想要 Ctrl+Alt+P", got)
	}
}

// TestAttachPanel_BadStoredHotkeyDegradesInsteadOfSkippingTray 钉住启动期的降级。
//
// 库里可能躺着一个装不上的组合（老版本会写进去，用户也可能自己改库）。
// attachPanel 若把它原样交给 Attach，ParseHotkey 报的是**普通错误**，
// 而那条 default 分支会直接 return —— 连 Accessory 策略和托盘都不装了。
// 托盘是"没有 Dock 图标"时唯一的退出入口，不能因为一条坏掉的设置整体失效。
func TestAttachPanel_BadStoredHotkeyDegradesInsteadOfSkippingTray(t *testing.T) {
	a, fp := newHotkeyApp(t)

	s := a.Settings()
	s.UI.Hotkey = "Ctrl+Shift+NotAKey" // 坏值：解析不过
	a.attachPanel(fp, s)

	fp.mu.Lock()
	cfg, accessory, traySet := fp.attachCfg, fp.accessory, fp.traySet
	fp.mu.Unlock()

	if cfg.Hotkey != "" {
		t.Errorf("解析不过的热键不该交给面板，得到 %q", cfg.Hotkey)
	}
	if accessory == 0 {
		t.Error("Accessory 激活策略仍要设置（它同时决定 Dock 里有没有图标）")
	}
	if !traySet {
		t.Error("托盘必须照装：它是没有 Dock 图标时唯一的退出入口")
	}
}

// TestHotkeyContract_FrontendTokensAreParsable 是**跨语言契约**检查。
//
// 设置页现在是"读键盘"的控件：前端把 Keydown 翻成一个规范组合串
// （frontend/src/hotkey.ts 的 CODE_TO_TOKEN / KEY_TO_TOKEN），写进 ui.hotkey，
// 再由后端 panel.ParseHotkey 解析。两边必须认识同一套键名。
//
// 这条测试像 test/check-i18n.mjs 一样静态扫源码：把前端那两张表里的
// **规范记号**抠出来，逐个交给后端的解析器。前端多写一个后端不认的键名
// （比如 'PAGE_UP'、'RETURN'），这里立刻红 —— 否则表现是"用户按下去
// 什么都没发生"，那种问题没人查得出来。
func TestHotkeyContract_FrontendTokensAreParsable(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("frontend", "src", "hotkey.ts"))
	if err != nil {
		t.Fatalf("读前端的 hotkey.ts：%v", err)
	}

	// 只认"标识符: '全大写记号'"这种行：它恰好就是两张映射表里的值
	// （MAC_KEY_LABEL 那些 '↩' / 'Space' 不会被匹配到，因为它们不是全大写）。
	tokenRe := regexp.MustCompile(`(?m)^\s*[A-Za-z0-9_]+:\s*'([A-Z0-9]+)',`)
	seen := map[string]bool{}
	for _, m := range tokenRe.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	if len(seen) < 10 {
		t.Fatalf("只从前端抠出 %d 个键名 —— 正则与写法对不上了，这条测试会退化成恒真", len(seen))
	}

	for tok := range seen {
		// 加一个修饰键，把话题收在"这个键名认不认"上。
		if _, err := panel.ParseHotkey("Ctrl+" + tok); err != nil {
			t.Errorf("前端会产出键名 %q，而后端解析不了：%v", tok, err)
		}
	}
}

// ── 输入态：让出 / 收回热键 ──────────────────────────────────────
//
// 设置页的热键控件是"先输入、再确认"的：进入输入态时要读键盘，而**系统级
// 热键在事件分发之前就把按键吃掉了**（Carbon 的 RegisterEventHotKey、
// Win32 的 RegisterHotKey 都收在窗口消息之前），WebView 根本收不到那一次
// keydown。真机上的表现就是"这个控件输入不了"。
//
// 所以输入态开始让出、结束收回。这三条测试分别钉住：让出是真的、
// 收回是按设置值装的、以及**收起面板时不依赖前端**也要收回。

// TestSuspendHotkey_HoldsTheHotkeyOpenWhileInputting 钉住让出与收回。
func TestSuspendHotkey_HoldsTheHotkeyOpenWhileInputting(t *testing.T) {
	a, fp := newHotkeyApp(t)

	const combo = "Ctrl+Alt+P"
	if err := a.SetSetting(store.KeyUIHotkey, `"Ctrl+Alt+P"`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	fp.unregistered = 0

	a.SuspendHotkey()
	if fp.hotkey != "" {
		t.Errorf("让出之后系统里还留着热键 %q", fp.hotkey)
	}
	if fp.unregistered != 1 {
		t.Errorf("注销被调用 %d 次，想要 1 次", fp.unregistered)
	}

	// 幂等：输入态的 effect 重挂时可能连着调两次，不该反复注销。
	a.SuspendHotkey()
	if fp.unregistered != 1 {
		t.Errorf("重复让出又注销了一次（共 %d 次）", fp.unregistered)
	}

	if err := a.ResumeHotkey(); err != nil {
		t.Fatalf("ResumeHotkey: %v", err)
	}
	if fp.hotkey != combo {
		t.Errorf("收回之后系统里的热键 = %q，想要 %q", fp.hotkey, combo)
	}
	// 让出/收回只动**注册**，不动设置：用户什么都没提交，库里那行原样不动。
	if got := a.Settings().UI.Hotkey; got != combo {
		t.Errorf("让出/收回改动了设置里的热键：%q", got)
	}
}

// TestResumeHotkey_IsIdempotentAndRespectsEmptySetting 钉住收回的两条边界。
func TestResumeHotkey_IsIdempotentAndRespectsEmptySetting(t *testing.T) {
	a, fp := newHotkeyApp(t)

	// 没让出过就收回：什么都不该发生。设置页每挂载一次都会调一次收回，
	// 若无条件注册，用户每次进设置页都会重装一遍热键。
	before := fp.hotkeyCalls
	if err := a.ResumeHotkey(); err != nil {
		t.Fatalf("ResumeHotkey: %v", err)
	}
	if fp.hotkeyCalls != before {
		t.Errorf("没让出过也注册了热键（调用 %d 次）", fp.hotkeyCalls)
	}

	// 用户本来就没设热键（清空是合法状态）：收回之后仍然没有，且不该报错。
	if err := a.SetSetting(store.KeyUIHotkey, `""`); err != nil {
		t.Fatalf("清空热键：%v", err)
	}
	a.SuspendHotkey()
	if err := a.ResumeHotkey(); err != nil {
		t.Fatalf("没有热键时收回应当无害，却报错：%v", err)
	}
	if fp.hotkey != "" {
		t.Errorf("用户已经清空热键，收回却又装上了 %q", fp.hotkey)
	}
}

// TestHidePanel_ResumesHotkeyWithoutHelpFromFrontend 钉住不依赖前端的兜底。
//
// 输入态里点一下面板外面（ui.closeOnBlur）或等空闲超时，面板就收起了，
// 而**设置页组件不会卸载**——前端那个"卸载时收回"的兜底不会跑。
// 收起这条路径不兜住的话，用户的全局热键会一直停在"已让出"状态：
// 热键没了，而设置页里还显示着那个组合，他完全看不出来。
func TestHidePanel_ResumesHotkeyWithoutHelpFromFrontend(t *testing.T) {
	a, fp := newHotkeyApp(t)

	const combo = "Ctrl+Alt+P"
	if err := a.SetSetting(store.KeyUIHotkey, `"Ctrl+Alt+P"`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	a.SuspendHotkey()
	if fp.hotkey != "" {
		t.Fatalf("前置条件不成立：让出之后系统里还有 %q", fp.hotkey)
	}

	a.HidePanel()
	if fp.hotkey != combo {
		t.Errorf("收起面板后系统里的热键 = %q，想要 %q（热键被留在让出态了）", fp.hotkey, combo)
	}
}
