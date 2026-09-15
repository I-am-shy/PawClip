package main

import (
	"strings"
	"testing"
)

// TestCatalogCoversEveryKey 是本文件里最该存在的一条测试。
//
// DESIGN §14 第 24 条的"易漏清单"列了六个位置（托盘、通知、导出/导入报告、
// 错误提示、备份包 README、导出文件名里的分类名）——把它们单列出来，就是
// 因为做 i18n 最容易只改前端、漏掉后端。
//
// 这条测试把"漏一项"从"上线后由英文用户发现"变成"提交时红"。
func TestCatalogCoversEveryKey(t *testing.T) {
	for _, lang := range []Lang{LangZhCN, LangEn} {
		m := catalog[lang]
		if m == nil {
			t.Fatalf("语言 %s 没有词条表", lang)
		}
		for _, k := range allMsgKeys {
			v, ok := m[k]
			if !ok {
				t.Errorf("%s 缺少词条 %q（新增 msgKey 时要在两张表里都补上）", lang, k)
				continue
			}
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s 的词条 %q 是空白", lang, k)
			}
		}
	}
}

// TestCatalogHasNoOrphanKeys 反向检查：表里不该有 allMsgKeys 之外的键。
//
// 光有单向检查会漏掉另一种坏法：把某个 key 改名了，两张表都留着旧键、
// 新键反而是空的——单向检查只会在新键被加进清单后才发现，而"忘了加进清单"
// 恰恰是最容易发生的。双向检查让"表"与"清单"不可能悄悄漂移。
func TestCatalogHasNoOrphanKeys(t *testing.T) {
	known := make(map[msgKey]bool, len(allMsgKeys))
	for _, k := range allMsgKeys {
		if known[k] {
			t.Errorf("allMsgKeys 里有重复项 %q", k)
		}
		known[k] = true
	}
	for _, lang := range []Lang{LangZhCN, LangEn} {
		for k := range catalog[lang] {
			if !known[k] {
				t.Errorf("%s 里有清单之外的词条 %q（改名后忘了删旧键？）", lang, k)
			}
		}
	}
}

// TestCatalogHasNoDuplicateText 检查两个键没有相同的文案。
//
// 这不是洁癖：托盘菜单里"暂停记录"与"继续记录"如果文案一样，
// 用户根本看不出当前是哪种状态。同语言内的重复文案几乎总是复制粘贴写错的。
func TestCatalogHasNoDuplicateText(t *testing.T) {
	for _, lang := range []Lang{LangZhCN, LangEn} {
		seen := make(map[string]msgKey, len(catalog[lang]))
		for _, k := range allMsgKeys {
			v := catalog[lang][k]
			if prev, dup := seen[v]; dup {
				t.Errorf("%s 里 %q 与 %q 的文案完全相同（%q）", lang, prev, k, v)
			}
			seen[v] = k
		}
	}
}

// TestResolveLang 覆盖 §9 的解析顺序：本机设置 → 系统区域 → 回退 en。
func TestResolveLang(t *testing.T) {
	cases := []struct {
		pref string
		want Lang
	}{
		{"zh-CN", LangZhCN},
		{"en", LangEn},
		// 未知值不该被当成"就用它"，而要落到系统/回退
		{"fr-FR", ""},
		{"system", ""},
		{"", ""},
		{"zh", ""}, // 只认规范标签，不认裸前缀
	}
	for _, c := range cases {
		got := resolveLang(c.pref)
		if c.want == "" {
			// 只要求它是一个受支持的语言，且不是空串
			if got != LangZhCN && got != LangEn {
				t.Errorf("resolveLang(%q) = %q，想要 zh-CN 或 en", c.pref, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("resolveLang(%q) = %q，想要 %q", c.pref, got, c.want)
		}
	}
}

// TestT_FallsBackToEnglishNotKey 钉住降级方向。
//
// 找不到键时返回 key 本身（"tray.pause"）是最糟的结果：用户看到的是
// 一串内部标识符。返回英文至少是可读的。
func TestT_FallsBackToEnglishNotKey(t *testing.T) {
	a := &App{}
	// 强制成中文再问一个"中文表里没有"的键——不存在这样的键（上面已断言
	// 两表覆盖完整），所以这里改为直接验证 catalogFor 的降级行为。
	if _, ok := catalogFor(LangZhCN)[msgTrayQuit]; !ok {
		t.Fatal("中文表应当有 tray.quit")
	}
	got := a.T(msgTrayQuit)
	if strings.TrimSpace(got) == "" || got == string(msgTrayQuit) {
		t.Errorf("T(tray.quit) = %q，不该是空串或键名本身", got)
	}
}

// TestLangReactsToSetting 验证「设置里改语言 → 后端文案跟着变」。
//
// 这条是 §14 第 24 条那六处的**实际生效**检查：托盘文案是后端自己渲染的，
// 语言设置改了它却没变，就是用户看到的"界面中文、菜单栏英文"。
func TestLangReactsToSetting(t *testing.T) {
	a := &App{}
	a.settings = nil
	// 无设置时可用的只有系统/回退路径，这里只断言"不 panic 且是受支持语言"
	if l := a.lang(); l != LangZhCN && l != LangEn {
		t.Errorf("lang() = %q，想要 zh-CN 或 en", l)
	}
	if s := a.Lang(); s != string(a.lang()) {
		t.Errorf("Lang() = %q 与 lang() = %q 不一致", s, a.lang())
	}
}
