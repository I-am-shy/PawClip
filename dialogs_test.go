package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
)

// 本文件钉住"选目录 / 选备份文件"这条链路的两个不变量：
//
//  1. macOS 必须走**原生对话框**（sheet 挂面板窗口），不能回 Wails——
//     Wails 把 sheet 挂在被掏空的宿主窗口上，macOS 弹 sheet 会把父窗口
//     强制显示到屏幕上，用户看到一块半透明占位窗口（2026-09-18 真机截图）。
//  2. 平台没有原生实现时（ErrUnsupported）必须回退 Wails，而不是报错——
//     Windows 的宿主窗口是正常窗口，回退是安全的。
//
// 原生对话框本身没法在 CI 里跑（会弹真面板），所以测的是参数折算与路由。

// withDialogFakes 换掉两条对话框出口，测试结束复原。
func withDialogFakes(t *testing.T,
	native func(panel.DialogOptions) (string, error),
	wails func(*App, panel.DialogOptions) (string, error)) {
	t.Helper()
	oldN, oldW := openNativeDialog, openWailsDialog
	openNativeDialog, openWailsDialog = native, wails
	t.Cleanup(func() { openNativeDialog, openWailsDialog = oldN, oldW })
}

func TestNativeExts_FoldsFilterPatterns(t *testing.T) {
	cases := []struct {
		name string
		in   []wailsrt.FileFilter
		want []string
	}{
		{"单一扩展名", []wailsrt.FileFilter{{Pattern: "*.clipbak"}}, []string{"clipbak"}},
		{"分号多值", []wailsrt.FileFilter{{Pattern: "*.png; *.jpg"}}, []string{"png", "jpg"}},
		{"含所有文件档则不限类型", []wailsrt.FileFilter{
			{Pattern: "*.clipbak"}, {Pattern: "*"}}, nil},
		{"空过滤器", nil, nil},
		{"裸点前缀", []wailsrt.FileFilter{{Pattern: ".clipbak"}}, []string{"clipbak"}},
	}
	for _, c := range cases {
		got := nativeExts(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: nativeExts=%v want=%v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: nativeExts[%d]=%q want=%q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestPickExportDir_UsesNativeDialogWithDefaults(t *testing.T) {
	a, _ := newTrayApp(t, true, "zh-CN")
	a.ctx = context.Background()

	// 挂一个临时库：默认目录折算依赖 db.Dir()（数据目录下的 exports）。
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "pawclip.db")})
	if err != nil {
		t.Fatalf("打开临时库: %v", err)
	}
	defer db.Close()
	a.initMu.Lock()
	a.db = db
	a.initMu.Unlock()

	var got panel.DialogOptions
	withDialogFakes(t,
		func(opts panel.DialogOptions) (string, error) { got = opts; return "/tmp/chosen", nil },
		func(*App, panel.DialogOptions) (string, error) {
			t.Error("darwin 上不许回退 Wails（会弹被掏空的宿主窗口）")
			return "", nil
		})

	p, err := a.PickExportDir()
	if err != nil {
		t.Fatalf("PickExportDir: %v", err)
	}
	if p != "/tmp/chosen" {
		t.Errorf("返回 %q， want /tmp/chosen", p)
	}
	if !got.CanDirs || !got.CanCreate || got.CanFiles {
		t.Errorf("选项折算错误： %+v", got)
	}
	if !strings.HasSuffix(got.Dir, "exports") {
		t.Errorf("初始目录应默认落在 exports 下，得到 %q", got.Dir)
	}
}

func TestPickBackupFile_AllFilesFilterMeansUnrestricted(t *testing.T) {
	a, _ := newTrayApp(t, true, "zh-CN")
	a.ctx = context.Background()

	var got panel.DialogOptions
	withDialogFakes(t,
		func(opts panel.DialogOptions) (string, error) { got = opts; return "", nil },
		func(*App, panel.DialogOptions) (string, error) { return "", nil })

	// 返回空串 = 取消；这里只关心传下去的选项。
	if _, err := a.PickBackupFile(); err != nil {
		t.Fatalf("PickBackupFile: %v", err)
	}
	// 过滤器里带"所有文件"档：macOS 面板没有格式下拉，必须放宽为不限类型，
	// 否则用户把 .clipbak 改名成 .zip（导出的包本就是 ZIP）就选不中了。
	if got.Extensions != nil {
		t.Errorf("含所有文件档时 Extensions 应为 nil（不限类型），得到 %v", got.Extensions)
	}
	if !got.CanFiles || got.CanDirs {
		t.Errorf("选项折算错误： %+v", got)
	}
}

func TestPickExportDir_FallsBackToWailsWhenUnsupported(t *testing.T) {
	a, _ := newTrayApp(t, true, "zh-CN")
	a.ctx = context.Background()

	var wailsGot panel.DialogOptions
	withDialogFakes(t,
		func(panel.DialogOptions) (string, error) {
			return "", panel.ErrUnsupported
		},
		func(_ *App, opts panel.DialogOptions) (string, error) {
			wailsGot = opts
			return "/tmp/wails", nil
		})

	p, err := a.PickExportDir()
	if err != nil {
		t.Fatalf("回退路径不该报错: %v", err)
	}
	if p != "/tmp/wails" {
		t.Errorf("返回 %q， want /tmp/wails", p)
	}
	if wailsGot.Title == "" || !wailsGot.CanDirs {
		t.Errorf("回退时选项丢失： %+v", wailsGot)
	}
}

func TestPickExportDir_ErrorsBeforeUIReady(t *testing.T) {
	a, _ := newTrayApp(t, true, "zh-CN")
	// ctx 为 nil（Wails 还没起来）：不许碰对话框出口，直接返回"界面没准备好"。
	withDialogFakes(t,
		func(panel.DialogOptions) (string, error) {
			t.Error("UI 未就绪时不许弹对话框")
			return "", nil
		},
		func(*App, panel.DialogOptions) (string, error) {
			t.Error("UI 未就绪时不许回退 Wails")
			return "", nil
		})
	if _, err := a.PickExportDir(); err == nil {
		t.Error("ctx 为 nil 时应返回错误")
	} else if errors.Is(err, panel.ErrUnsupported) {
		t.Errorf("不应把 ErrUnsupported 泄漏给上层: %v", err)
	}
}

// TestOpenableURL 钉住"什么地址可以交给系统去打开"这道白名单。
//
// 为什么值得单独测：这是"用户内容 → 系统执行"的那条通道，一旦放宽，
// 出问题的地方在**应用之外**（浏览器/访达被拉起来），应用里没有任何迹象。
// 所以这里刻意用**白名单**（黑名单挡不住 `JaVaScRiPt:`、`file:`、
// 以及各种注册 scheme 的大小写变体），并配一组"看起来像地址但不是"的输入。
//
// 变异验证：把 openableURL 改成 `return s != ""`，下面 deny 组会红。
func TestOpenableURL(t *testing.T) {
	allow := []string{
		"http://a.com",
		"https://a.com/x?y=1#z",
		"HTTPS://A.COM/x",   // 协议大小写不敏感
		"  https://a.com  ", // 前后空白由 TrimSpace 吃掉
		"mailto:a@b.com",
	}
	for _, s := range allow {
		if !openableURL(s) {
			t.Errorf("openableURL(%q) = false，应当放行", s)
		}
	}
	deny := []string{
		"",
		"   ",
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		"java\tscript:alert(1)", // 内部制表符不会被 TrimSpace 去掉
		"file:///etc/passwd",
		"data:text/html,<b>x</b>",
		"ftp://a.com",
		"blob/9f/2a/x.png", // 草稿自己的图片：相对地址，没有"外部"可言
		"www.a.com",        // 不带协议的不算（补协议是前端识别裸 URL 的活）
	}
	for _, s := range deny {
		if openableURL(s) {
			t.Errorf("openableURL(%q) = true，应当拒绝", s)
		}
	}
}

// TestOpenURL_RefusesBeforeReachingTheSystem 钉住"被拒的地址一步都不往外走"。
//
// 只断言返回错误是不够的：真正的危害是**系统那一侧已经被启动了**，
// 所以这里替换掉 openURLInSystem 看它有没有被调到。
func TestOpenURL_RefusesBeforeReachingTheSystem(t *testing.T) {
	var called []string
	old := openURLInSystem
	openURLInSystem = func(u string) error {
		called = append(called, u)
		return nil
	}
	t.Cleanup(func() { openURLInSystem = old })

	a := &App{}
	for _, bad := range []string{"", "javascript:alert(1)", "blob/9f/2a/x.png"} {
		if err := a.OpenURL(bad); err == nil {
			t.Errorf("OpenURL(%q) 应当报错", bad)
		}
	}
	if len(called) != 0 {
		t.Fatalf("被拒的地址不该走到系统那一步，实际调了 %v", called)
	}

	// 放行的要原样递出去，但**去掉前后空白**（空白会让 exec 的参数与
	// 用户看到的不一致，`open " https://a.com"` 在有些平台上会当路径找）。
	if err := a.OpenURL("  https://a.com/x  "); err != nil {
		t.Fatalf("OpenURL(https) 报错：%v", err)
	}
	if len(called) != 1 || called[0] != "https://a.com/x" {
		t.Fatalf("递给系统的地址不对：%v", called)
	}
}
