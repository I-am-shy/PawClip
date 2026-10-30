package panel

import "testing"

// TestClampPanelSize 钉住尺寸夹取的行为。
//
// 它存在的理由：这个函数站在"库里存的旧值 / 人手改过的值"与"窗口尺寸"之间，
// 错一档的后果是**静默的**——面板要么小到布局挤碎、要么被莫名其妙截断。
// 2026-10-31 起只有下限，没有上限：大尺寸必须原样通过。
func TestClampPanelSize(t *testing.T) {
	cases := []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"正常值原样通过", 700, 760, 700, 760},
		{"下界原样通过", MinPanelWidth, MinPanelHeight, MinPanelWidth, MinPanelHeight},
		{"过小抬到下限", 100, 100, MinPanelWidth, MinPanelHeight},
		{"大尺寸不再截断", 5000, 5000, 5000, 5000},
		{"宽合法高过小：各自独立夹取", 700, 10, 700, MinPanelHeight},
		{"宽度为 0 视为没值", 0, 760, 0, 0},
		{"高度为 0 视为没值", 560, 0, 0, 0},
		{"负值视为没值", -1, -1, 0, 0},
		{"一边为负整组作废", 560, -5, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := ClampPanelSize(c.w, c.h)
			if w != c.wantW || h != c.wantH {
				t.Fatalf("ClampPanelSize(%d,%d) = (%d,%d)，期望 (%d,%d)",
					c.w, c.h, w, h, c.wantW, c.wantH)
			}
			// (0,0) 的语义必须**恰好**是"没值"：调用方靠它决定"用默认尺寸"
			// 还是"照这个尺寸建窗口"，混进一个"0 宽 + 合法高"就会写出半个尺寸。
			invalid := c.w <= 0 || c.h <= 0
			isZero := w == 0 && h == 0
			if isZero != invalid {
				t.Fatalf("ClampPanelSize(%d,%d) = (%d,%d)：零值语义与输入是否非法不一致",
					c.w, c.h, w, h)
			}
		})
	}
}

// TestClampPanelSizeMinimumSane 只钉"下限本身是正数"这一条。
//
// 没有上限了，所以不再有"下界 > 上界"这种写反的可能；下限仍必须是正数，
// 否则 0 会被当成"没值"，和零值语义撞车。
func TestClampPanelSizeMinimumSane(t *testing.T) {
	if MinPanelWidth <= 0 || MinPanelHeight <= 0 {
		t.Fatalf("下限必须是正数：MinPanelWidth=%d MinPanelHeight=%d",
			MinPanelWidth, MinPanelHeight)
	}
}
