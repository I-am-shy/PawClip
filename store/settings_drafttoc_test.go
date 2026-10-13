package store

import (
	"context"
	"encoding/json"
	"testing"
)

// ── 草稿本侧边栏的开合状态（ui.draftTocOpen）──────────────────────
//
// 这个键曾经叫 ui.draftTocCollapsed，含义写着"收起"，可**写入端写的是展开**
// （frontend/src/views/Drafts.tsx 的 toggleToc）。键名、默认值、读取端三者都按
// "收起"解释，只有写入端按"展开"写 —— 每一处单独看都自洽，合起来是"用户每次
// 重启看到的都是上一次的反面"。
//
// 修法不是在中间补一次取反（那只是让两处错误互相抵消，下一个读这个键的人照样
// 踩），而是把键改成说它真正含义的名字（ui.draftTocOpen），写入端不再取反，
// 默认值也就是"打开"——这正是用户要的"默认打开侧边栏"。
//
// 这里钉三件事：新库默认开着、旧键的值被搬过来且**不取反**、两个键都在时以
// 新键为准。搬运本身在 store/settings_legacy.go，调用时机在生产路径上
// （App.initialize：搬运 → 补默认值 → 读设置），所以下面的 helper 照抄那个顺序。

// seedAndLoad 复现生产启动顺序：搬旧键 → 补默认值 → 读设置。
func seedAndLoad(t *testing.T, db *DB) *Settings {
	t.Helper()
	ctx := context.Background()
	if err := CarryOverLegacySettings(ctx, db); err != nil {
		t.Fatalf("CarryOverLegacySettings: %v", err)
	}
	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatalf("SeedSettings: %v", err)
	}
	s, err := LoadSettings(ctx, db)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	return s
}

func TestSettings_DraftTOCOpenDefaultsToOpen(t *testing.T) {
	db := newTestDB(t)
	if s := seedAndLoad(t, db); !s.UI.DraftTOCOpen {
		t.Error("全新库的 DraftTOCOpen = false，期望 true（草稿本默认打开侧边栏）")
	}
}

// TestSettings_DraftTOCOpenHonoursLegacyKey：老库里只有旧键。
//
// 旧键的**名字**说"收起"，但它的**值**是写入端按展开写进去的，所以要原样搬
// —— 不取反。取反会把用户上次的姿态正好弄反（那正是这个 bug 的形态）。
func TestSettings_DraftTOCOpenHonoursLegacyKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"旧值 true 表示展开", "true", true},
		{"旧值 false 表示收起", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			if err := db.SetRaw(context.Background(), KeyUIDraftTOCCollapsedLegacy, tc.raw); err != nil {
				t.Fatalf("SetRaw(legacy): %v", err)
			}
			if s := seedAndLoad(t, db); s.UI.DraftTOCOpen != tc.want {
				t.Errorf("DraftTOCOpen = %v，期望 %v（旧键的值原样搬过来）", s.UI.DraftTOCOpen, tc.want)
			}
		})
	}
}

// TestSettings_DraftTOCOpenWinsOverLegacyKey：两个键都在时以新键为准 ——
// 用户在新版本里拨过一次之后，不能再被库里那行旧值盖回去。
func TestSettings_DraftTOCOpenWinsOverLegacyKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := db.SetRaw(ctx, KeyUIDraftTOCCollapsedLegacy, "false"); err != nil {
		t.Fatalf("SetRaw(legacy): %v", err)
	}
	if err := db.SetRaw(ctx, KeyUIDraftTOCOpen, "true"); err != nil {
		t.Fatalf("SetRaw(new): %v", err)
	}
	if s := seedAndLoad(t, db); !s.UI.DraftTOCOpen {
		t.Error("两个键都在时 DraftTOCOpen = false，期望以新键（true）为准")
	}
}

// TestSettings_DraftTOCOpenLegacyBadValueFallsBackToDefault：旧值不是合法布尔
// （手改过库）时不搬，让它走默认值 —— 而不是把垃圾抄进新键。
func TestSettings_DraftTOCOpenLegacyBadValueFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := db.SetRaw(ctx, KeyUIDraftTOCCollapsedLegacy, `"yes"`); err != nil {
		t.Fatalf("SetRaw(legacy): %v", err)
	}
	if s := seedAndLoad(t, db); !s.UI.DraftTOCOpen {
		t.Error("旧值是坏值时 DraftTOCOpen = false，期望回落到默认的 true")
	}
}

// TestSettings_DraftTOCOpenIsWrittenAsJSONBool：写入端写的是 `true` 这种 JSON
// 字面量（前端 SetSetting 传 String(bool)），读取侧靠 json.Unmarshal 解码。
// 两边一旦有一边改成 `"true"`（带引号的字符串）就会静默失败，失败方向还是
// "用默认值" —— 表现为"设置改不动"，很难查。
func TestSettings_DraftTOCOpenIsWrittenAsJSONBool(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := db.SetRaw(ctx, KeyUIDraftTOCOpen, "false"); err != nil {
		t.Fatalf("SetRaw: %v", err)
	}
	raw, ok, err := db.GetRaw(ctx, KeyUIDraftTOCOpen)
	if err != nil || !ok {
		t.Fatalf("GetRaw = (%q, %v, %v)", raw, ok, err)
	}
	var open bool
	if err := json.Unmarshal([]byte(raw), &open); err != nil {
		t.Fatalf("库里的值 %q 不是合法 JSON 布尔: %v", raw, err)
	}
	if s := seedAndLoad(t, db); s.UI.DraftTOCOpen {
		t.Errorf("库里写了 false，读回来却是 %v", s.UI.DraftTOCOpen)
	}
}

// TestSettings_DraftTOCOpenIsAManagedKey：这个键必须在受管键表里，否则前端
// SetSetting 会被 app.go 的白名单挡下来 —— 表现是"拨了开关，重启又回去了"，
// 且前端把错误吞掉（.catch(() => {})），一声不响。
func TestSettings_DraftTOCOpenIsAManagedKey(t *testing.T) {
	for _, k := range SettingKeys() {
		if k == KeyUIDraftTOCOpen {
			return
		}
	}
	t.Errorf("SettingKeys() 里没有 %s（前端写它会被白名单拒绝）", KeyUIDraftTOCOpen)
}
