// 草稿本侧边栏（目录树）那条设置路径的测试。
//
// 为什么值得单独一个文件：它是"前端读一个 bool、写一个 bool"的最小形态，
// 而正是这种最小形态最容易被极性搞反 —— 反了以后**两边各自都自洽**，
// 谁也不报错、日志里什么都没有，只在用户那里表现为"这个开关记不住"。
//
// 所以这里从**线路那一侧**钉它：真的起一个 App（真库、noPlatformUI），
// 写设置、读 DraftList、看序列化给前端的 JSON 里那一位叫什么、再重启一次
// 看它还在不在。前端读的就是这份 JSON，所以这就是它的契约。

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zego/pawclip/store"
)

// bootDraftsApp 在 dir 上起一个 App（真库；noPlatformUI 跳过面板接管）。
//
// 不注册 t.Cleanup：这几个用例要**关掉再开**来验证"活过重启"，
// 由调用方显式 Shutdown。
func bootDraftsApp(t *testing.T, dir string) *App {
	t.Helper()
	boot := DefaultBootstrap()
	boot.Database.Path = filepath.Join(dir, "paw.db")

	a := NewApp(boot, filepath.Join(dir, "config.toml"), discardLogger())
	a.noPlatformUI = true
	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return a
}

// TestDraftsSidebar_DefaultsOpenAndWireNameIsTocOpen 钉住两件事：
// 默认**开着**（产品要求"默认打开侧边栏"），以及前端读的那一位叫
// `tocOpen`（极性 = 是否展开）。
//
// 名字也是契约：前端 `setTocOpen(l.tocOpen)` 把它直接交给 useState，
// 后端换成 `tocCollapsed`（哪怕值本身是对的）就会让前端把 true 读成收起。
func TestDraftsSidebar_DefaultsOpenAndWireNameIsTocOpen(t *testing.T) {
	dir := t.TempDir()
	a := bootDraftsApp(t, dir)
	defer a.Shutdown()

	l, err := a.Drafts()
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}
	if !l.TocOpen {
		t.Error("全新库的 TocOpen = false，期望 true（默认打开侧边栏）")
	}

	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal DraftList: %v", err)
	}
	if !strings.Contains(string(raw), `"tocOpen":true`) {
		t.Errorf("DraftList 序列化后没有 \"tocOpen\":true：%s", firstN(raw, 400))
	}
	if strings.Contains(string(raw), "tocCollapsed") {
		t.Error("DraftList 里仍然出现 tocCollapsed —— 名字与极性一起被误读的机会又回来了")
	}
}

// TestDraftsSidebar_ToggleSurvivesRestart 就是用户报障的那条：
// 拨过开关 → 重启 → 还是上次的姿态。
//
// 必须真的重启（关掉 App、在同一个目录上再开一个）：只读一次设置验证不了
// "记不住"，因为这个 bug 恰恰是"写进去的值"与"读出来的解释"不一致。
func TestDraftsSidebar_ToggleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	a := bootDraftsApp(t, dir)
	if err := a.SetSetting(store.KeyUIDraftTOCOpen, "false"); err != nil {
		t.Fatalf("SetSetting(%s): %v", store.KeyUIDraftTOCOpen, err)
	}
	if l, err := a.Drafts(); err != nil {
		t.Fatalf("Drafts: %v", err)
	} else if l.TocOpen {
		t.Error("收起之后同一次运行里 TocOpen 仍然 = true")
	}
	a.Shutdown()

	b := bootDraftsApp(t, dir)
	defer b.Shutdown()
	l, err := b.Drafts()
	if err != nil {
		t.Fatalf("Drafts（重启后）: %v", err)
	}
	if l.TocOpen {
		t.Error("重启后 TocOpen = true，期望 false（用户上次是收起的）—— 开关没记住")
	}
}

// TestDraftsSidebar_LegacyKeyIsCarriedOverVerbatim：老库只有旧键
// （ui.draftTocCollapsed）时，值**原样**沿用 —— 不取反。
//
// 旧键的**名字**说"收起"，但老版本写进去的是**展开**状态（写入端一直是按
// 展开写的），所以 true 必须读成"展开"。这一条若写成取反，用户升级上来第一眼
// 看到的就是反的，而这次修复的全部意义就是不让他再看到反的。
func TestDraftsSidebar_LegacyKeyIsCarriedOverVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"旧值 true（上次是展开的）", "true", true},
		{"旧值 false（上次是收起的）", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// 先造出"老版本留下的库"：只有旧键、没有新键。
			//
			// ⚠️ 这一步必须在**新版本第一次启动之前**完成：新版本一启动就会
			// 补上缺席的新键（默认 true），那之后旧值就再也不会被搬了 ——
			// 所以不能"先起一个 App 再往里写旧键"（那正是本用例第一版写错
			// 的地方，它把要测的场景悄悄换成了"两个键都在"）。
			db, err := store.Open(store.Options{
				Path:   filepath.Join(dir, "paw.db"),
				Logger: discardLogger(),
			})
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			if err := db.SetRaw(context.Background(), store.KeyUIDraftTOCCollapsedLegacy, tc.raw); err != nil {
				t.Fatalf("SetRaw(legacy): %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			a := bootDraftsApp(t, dir)
			defer a.Shutdown()
			l, err := a.Drafts()
			if err != nil {
				t.Fatalf("Drafts: %v", err)
			}
			if l.TocOpen != tc.want {
				t.Errorf("沿用旧键后 TocOpen = %v，期望 %v（旧键的值原样搬、不取反）",
					l.TocOpen, tc.want)
			}
		})
	}
}

// firstN 截断过长的 JSON，让失败信息还能读。
func firstN(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
