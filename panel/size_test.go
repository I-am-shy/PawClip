package panel

import "testing"

// TestClampPanelSize 钉住尺寸夹取的行为。
//
// 它存在的理由：这个函数站在"库里存的旧值 / 人手改过的值"与"窗口尺寸"之间，
// 错一档的后果是**静默的**——面板要么小到布局挤碎、要么大到超出屏幕，
// 而这两种都只在真机上才看得出来。
func TestClampPanelSize(t *testing.T) {
	cases := []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"正常值原样通过", 560, 760, 560, 760},
		{"下界原样通过", MinPanelWidth, MinPanelHeight, MinPanelWidth, MinPanelHeight},
		{"上界原样通过", MaxPanelWidth, MaxPanelHeight, MaxPanelWidth, MaxPanelHeight},
		{"过小抬到下限", 100, 100, MinPanelWidth, MinPanelHeight},
		{"过大压到上限", 5000, 5000, MaxPanelWidth, MaxPanelHeight},
		{"宽合法高过小：各自独立夹取", 600, 10, 600, MinPanelHeight},
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

// TestClampPanelSizeBoundsSane 钉住上下界本身的关系——写反了（下界 > 上界）
// 会让夹取产出不可能满足的值，而表驱动用例仍会全绿（它只是拿常量跟自己比）。
func TestClampPanelSizeBoundsSane(t *testing.T) {
	if MinPanelWidth >= MaxPanelWidth {
		t.Fatalf("宽度下界 %d 不小于上界 %d", MinPanelWidth, MaxPanelWidth)
	}
	if MinPanelHeight >= MaxPanelHeight {
		t.Fatalf("高度下界 %d 不小于上界 %d", MinPanelHeight, MaxPanelHeight)
	}
}
