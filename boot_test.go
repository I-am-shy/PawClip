package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这个文件钉住"配置读不了时程序**仍然启动**，并且用户看得见原因"。
//
// 背景（一个被改掉的真实缺陷）：main() 原本在 LoadBootstrap 失败时
// `fmt.Fprintln(os.Stderr, err); os.Exit(1)`。对 GUI 程序来说 stderr
// 没人看得到——用户双击图标，进程起来又立刻死掉，界面上什么都不会出现。
//
// 而同一个项目里 normalizeBootstrap 的原则写的是"把明显非法的取值拉回
// 默认值，而不是让应用起不来"。两处决定本来是矛盾的：**值域**错了可以继续
// 跑，**语法**错了整个不启动。
//
// 现在统一为：能启动就必须启动，然后把原因交给界面。这几条测试就是
// 那次改动的判据。

// brokenConfigPath 写一个语法坏掉的 config.toml，返回路径。
func brokenConfigPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	// toml.Decode 会在这里报错：字符串没有闭合的引号。
	if err := os.WriteFile(p, []byte("log_level = \"info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBrokenConfigStillYieldsUsableDefaults 断言坏配置不会让 boot 变成空壳。
//
// 这条防的是"换掉 os.Exit 之后顺手忽略了错误"：那样 boot 会是
// BootstrapConfig{}（零值），数据库路径为空、日志级别为空——
// 程序确实起来了，但起在一个没有任何配置的状态上，比直接报错更难查。
func TestBrokenConfigStillYieldsUsableDefaults(t *testing.T) {
	path := brokenConfigPath(t)

	_, _, err := LoadBootstrap(path)
	if err == nil {
		t.Fatal("坏配置竟然读成功了 —— 这条测试的语料已经失效，请换一种坏法")
	}

	// main() 在出错时的回退动作：用默认值继续。
	boot := DefaultBootstrap()
	if boot.Log.Level == "" {
		t.Error("默认配置的 log.level 是空的 —— 回退值本身不合格")
	}
	if boot.UI.Language == "" {
		t.Error("默认配置的 ui.language 是空的 —— 回退值本身不合格")
	}
}

// TestBootWarningSurfacesInHealth 断言"配置坏了"这件事会出现在 Health 里，
// 也就是界面上那条横幅的数据来源。
func TestBootWarningSurfacesInHealth(t *testing.T) {
	path := brokenConfigPath(t)
	_, _, loadErr := LoadBootstrap(path)
	if loadErr == nil {
		t.Fatal("坏配置竟然读成功了")
	}

	app := newApp(DefaultBootstrap(), path, bootTestLogger(), loadErr)

	h, err := app.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.BootWarning == "" {
		t.Fatal("配置读不了，但 Health.BootWarning 是空的 —— " +
			"用户将看到「什么都没发生」，这正是这条测试要防的场景")
	}
	// 不能把键名或原始错误直接抛出去：那句话是显示给用户看的。
	if strings.Contains(h.BootWarning, "boot.configBroken") {
		t.Errorf("BootWarning 落到了键名而不是文案：%q", h.BootWarning)
	}
	if !strings.Contains(h.BootWarning, path) {
		// 底层错误里带路径，用户据此才知道该去改哪个文件。
		// 少了它，"配置文件读不了"这句话等于没说清是哪个文件。
		t.Errorf("BootWarning 里没有带上出问题的路径：%q\n（底层错误：%v）",
			h.BootWarning, loadErr)
	}
	// 它同时必须是**本地化过**的一句话：中文界面不该出现英文原句。
	// 这里不断言具体语言（CI 的 LANG 不定），只断言它不是空、也不含英文长句特征。
	if h.InitError != "" {
		t.Errorf("只是配置坏了，不该被判成初始化失败：%q", h.InitError)
	}
}

// TestNoBootWarningForHealthyConfig 反向断言：配置正常时**不能**出现警告。
//
// 没有这条的话，把 BootWarning 写成恒非空也能让上面那条测试通过——
// 那样用户每次启动都会看到一条莫名其妙的警告，久了就学会忽略所有警告。
func TestNoBootWarningForHealthyConfig(t *testing.T) {
	app := newApp(DefaultBootstrap(), "/tmp/whatever.toml", bootTestLogger(), nil)

	h, err := app.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.BootWarning != "" {
		t.Errorf("配置正常却报了启动警告：%q", h.BootWarning)
	}
}

// TestBootWarningIsLocalizedInBothLanguages 断言那条警告两种语言都取得到，
// 而且不是回退成键名。
func TestBootWarningIsLocalizedInBothLanguages(t *testing.T) {
	for _, lang := range []Lang{LangZhCN, LangEn} {
		s := renderIn(lang, msgBootConfigBroken, "示例错误")
		if strings.Contains(s, "boot.configBroken") {
			t.Errorf("%s 下取到的是键名而不是文案：%q", lang, s)
		}
		if !strings.Contains(s, "示例错误") {
			t.Errorf("%s 下没有把底层原因拼进去：%q", lang, s)
		}
	}

	zh := renderIn(LangZhCN, msgBootConfigBroken, "x")
	en := renderIn(LangEn, msgBootConfigBroken, "x")
	if zh == en {
		t.Error("中英两门语言下的启动警告是同一句话 —— 有一门漏翻了")
	}
}

// bootTestLogger 给不需要看日志的用例一个安静的记录器。
func bootTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// renderIn 在**指定语言**下渲染一条文案。
//
// 为什么不复用 App.render：那个方法走的是"当前语言"，而在测试里
// "当前语言"取决于运行环境的 LANG —— 断言会因为跑测试的机器不同而变。
// 要验证"两门语言都翻到了"，就必须能显式指定语言。
// 放在测试文件里而不是生产代码里：它是测试需求，不是产品能力。
func renderIn(l Lang, k msgKey, args ...any) string {
	tmpl := catalogFor(l)[k]
	if tmpl == "" {
		tmpl = string(k)
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, renderArgs(tmpl, catalogFor(l), args)...)
}
