package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// 老设置键的搬运（一次性兼容）。
//
// 目前只有一条：草稿本侧边栏的开合状态在 0.1.3 及更早叫
// `ui.draftTocCollapsed`，而那个键名与它的内容**是相反的** —— 写入端一直按
// "展开"往里写（见 frontend/src/views/Drafts.tsx 的 toggleToc），键名、默认值
// 与读取端却都按"收起"解释。0.1.4 改名成 `ui.draftTocOpen`（默认 true = 展开），
// 读写两侧同一极性。
//
// 改名之后老库里的旧值不能不管：
//
//   - **必须搬**，不能只靠新键的默认值：旧键里的值是用户上次的真实姿态，用默认
//     值覆盖等于替他改了一次设置 —— 对一个"布局必需"的开关（§4.4.5）来说，
//     那会让窄面板下的排版在他升级后第一眼就变样。
//   - **原样搬，不取反**：旧键的名字说"收起"，可里面装的确实是展开状态；取反
//     只会让两处错误互相抵消，下一个读它的人照样踩（这次修的就是这个）。
//   - **只在新键缺席时搬**：用户在新版本里拨过一次之后，新键就是唯一真源，
//     不能再被库里那行旧值盖回去。
//   - 值不是合法布尔（手改过库）时不搬：让他走默认值，而不是把垃圾抄进新键。
//
// 调用时机在 App.initialize 里，且**必须在 SeedSettings 之前**：那一步会给缺席
// 的新键写默认值，一旦写下去，旧值就永远读不到了。
func CarryOverLegacySettings(ctx context.Context, d *DB) error {
	all, err := d.AllRaw(ctx)
	if err != nil {
		return err
	}
	if _, ok := all[KeyUIDraftTOCOpen]; ok {
		return nil
	}
	raw, ok := all[KeyUIDraftTOCCollapsedLegacy]
	if !ok {
		return nil
	}
	var open bool
	if err := json.Unmarshal([]byte(raw), &open); err != nil {
		// 坏值不搬：新键缺席 → SeedSettings 会补默认值（展开）。
		return nil
	}
	if err := d.SetRaw(ctx, KeyUIDraftTOCOpen, raw); err != nil {
		return fmt.Errorf("store: carry over %s: %w", KeyUIDraftTOCOpen, err)
	}
	return nil
}
