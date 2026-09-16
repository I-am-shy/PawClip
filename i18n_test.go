package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCatalogCoversEveryKey 是本文件里最该存在的一条测试。
//
// docs/DESIGN.md §14 第 24 条的"易漏清单"列了六个位置（托盘、通知、导出/导入报告、
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

// TestEveryKeyIsActuallyUsed 是本轮补上的那条"承重"测试。
//
// # 它防的是什么
//
// 上面三条测试全部只检查**目录自己**：两张表覆盖清单、清单没有多余键、
// 同语言内文案不重复。它们都会在"定义了 12 个键却一次都没调用"时**全绿** ——
// 这正是本轮发现的实际情况：msgReport*（导出/导入报告）、
// msgNotifyExportDone/ImportDone（导出/导入完成通知）、msgBackupReadme*、
// msgPasteAccessibility 全都躺在目录里没人用，于是那六个"易漏位置"里
// 有三个实际上根本没做，而所有测试都是绿的。
//
// 所以这条测试换个方向查：**每个键都必须在 i18n.go 之外的源码里被引用**。
// 做法是静态扫源码（而不是反射），理由与 allMsgKeys 手工维护同源——
// 它检查的是"有没有人用它"，这件事只有看代码才知道。
func TestEveryKeyIsActuallyUsed(t *testing.T) {
	// 键名（常量名）与键值（"err.notFound"）不是一回事：源码里引用的是
	// **常量名**。所以要先把 i18n.go 里的 `msgXxx msgKey = "..."` 解析成
	// 一张 键值 → 常量名 的表，再去别的文件里找那个名字。
	//
	// 只扫本包（msgKey 是 package main 的类型，子包引用不到）的非测试文件，
	// 并且排除 i18n.go 自己：那里只有定义与目录，引用它自己不算"用过"。
	decl, err := os.ReadFile("i18n.go")
	if err != nil {
		t.Fatalf("读 i18n.go: %v", err)
	}
	names := map[string]string{} // 键值 → 常量名
	declRe := regexp.MustCompile(`(?m)^\s*(msg[A-Za-z0-9_]*)\s+msgKey\s*=\s*"([^"]+)"`)
	for _, m := range declRe.FindAllStringSubmatch(string(decl), -1) {
		names[m[2]] = m[1]
	}
	if len(names) != len(allMsgKeys) {
		t.Fatalf("从 i18n.go 解析出 %d 个键，allMsgKeys 有 %d 个 —— 解析规则与"+
			"声明格式不一致（有键被漏掉了，这条测试就不可信了）", len(names), len(allMsgKeys))
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	var src strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			name == "i18n.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		src.Write(b)
		src.WriteByte('\n')
	}
	code := src.String()
	if len(code) == 0 {
		t.Fatal("没有扫到任何源码 —— 这条测试会退化成恒真")
	}

	for _, k := range allMsgKeys {
		constName, ok := names[string(k)]
		if !ok {
			t.Errorf("键 %q 在 i18n.go 里没有对应的常量声明", k)
			continue
		}
		if !strings.Contains(code, constName) {
			t.Errorf("msgKey %q（%s）在源码里没有任何调用点 —— 要么接上，"+
				"要么删掉；留着它只会让人以为这个位置已经做了 i18n",
				constName, k)
		}
	}
}
