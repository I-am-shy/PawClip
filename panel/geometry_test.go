package panel

import "testing"

// TestRecenterIn 钉住"把面板放到工作区顶部居中"的取值。
//
// 它存在的理由：这段计算决定了**热键呼出后面板出现在哪**，而它错了的表现
// 是"面板在另一块显示器上"或者"面板贴着屏幕底边、列表被切掉一半"——
// 都是只有真机才看得见的错，且看起来像窗口系统的锅。Windows 侧尤其贵：
// 那个平台没有开发机，只能靠这张表。
func TestRecenterIn(t *testing.T) {
	// 主显示器：1920×1080 工作区，窗口 560×760。
	// 期望 y：1080 * 0.12 = 129.6 → 129（截断，不是四舍五入）。
	main1080 := Rect{X: 0, Y: 0, W: 1920, H: 1080}

	cases := []struct {
		name  string
		work  Rect
		w, h  int
		wantX int
		wantY int
	}{
		{"主屏：水平居中、顶部留 12%", main1080, 560, 760, 680, 129},
		{"宽窗口：居中后左右边距相等", main1080, 760, 760, 580, 129},
		{
			// 左副屏：工作区原点是负数。这是多显示器上最容易写错的一格——
			// 把 work.X 当成 0 会让面板跑到主屏去。
			name: "原点为负的左副屏", work: Rect{X: -1920, Y: 0, W: 1920, H: 1080},
			w: 560, h: 760, wantX: -1240, wantY: 129,
		},
		{
			// 任务栏在上方时工作区原点不为 0，12% 要相对工作区算。
			name: "工作区上边界不为 0", work: Rect{X: 0, Y: 40, W: 1920, H: 1040},
			w: 560, h: 760, wantX: 680, wantY: 40 + 124,
		},
		{
			// 窗口比工作区还高：底部 clamp 之后会算到工作区上方，
			// 必须再夹回工作区顶（见 RecenterIn 里那条有意偏差）。
			name: "窗口高于工作区：贴住工作区顶部", work: main1080,
			w: 560, h: 1200, wantX: 680, wantY: 0,
		},
		{
			name: "窗口高于工作区且上边界非 0", work: Rect{X: 0, Y: 300, W: 1280, H: 720},
			w: 560, h: 900, wantX: 360, wantY: 300,
		},
		{
			// 面板高度恰好等于工作区高度：底部 clamp 生效，贴住工作区顶部。
			name: "窗口与工作区等高", work: main1080, w: 560, h: 1080, wantX: 680, wantY: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, y := RecenterIn(c.work, c.w, c.h)
			if x != c.wantX || y != c.wantY {
				t.Fatalf("RecenterIn(%+v, %d, %d) = (%d,%d)，期望 (%d,%d)",
					c.work, c.w, c.h, x, y, c.wantX, c.wantY)
			}
		})
	}
}

// TestRecenterInSitsAboveCenter 钉住"面板偏上"这件事本身。
//
// 12% 的留白如果被改成垂直居中（50%），上面那张表里的具体数字会同步变，
// 表驱动用例只会跟着一起改绿——但产品形态已经变了。所以这条判据看的是
// **关系**而不是数字：面板顶边必须明显高于垂直居中时的位置。
func TestRecenterInSitsAboveCenter(t *testing.T) {
	work := Rect{X: 0, Y: 0, W: 1920, H: 1080}
	const winW, winH = 560, 760

	_, y := RecenterIn(work, winW, winH)
	centered := work.Y + (work.H-winH)/2

	if y >= centered {
		t.Fatalf("面板顶边 y=%d 不在垂直居中(%d)之上：12%% 的留白被改坏了", y, centered)
	}
	if y <= work.Y {
		t.Fatalf("面板顶边 y=%d 贴住了工作区顶边：顶部留白没了", y)
	}
}

// TestRecenterInInvalidInput 钉住拿不到可靠输入时的行为。
//
// 它不能返回错误（签名里没有），所以只能返回保守值；这里钉住的是
// "保守"具体是哪两个数——给 (0,0) 会在非主显示器上把面板甩到主屏，
// 而调用方拿不到任何信号。
func TestRecenterInInvalidInput(t *testing.T) {
	work := Rect{X: -100, Y: 50, W: 1280, H: 720}
	for _, c := range []struct {
		name string
		w, h int
	}{
		{"宽度为 0", 0, 760},
		{"高度为 0", 560, 0},
		{"宽度为负", -1, 760},
		{"高度为负", 560, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			x, y := RecenterIn(work, c.w, c.h)
			if x != work.X || y != work.Y {
				t.Fatalf("非法尺寸 (%d,%d) 返回 (%d,%d)，期望工作区原点 (%d,%d)",
					c.w, c.h, x, y, work.X, work.Y)
			}
		})
	}
}

// TestRecenterInMatchesDarwin 钉住 Go 侧与 ObjC 侧是**同一个公式**。
//
// panel_darwin.m 的 paw_panel_recenter 用左下角原点，本包的 RecenterIn 用
// 左上角原点，两边没有共享代码——这种"两套实现、同一意图"的地方，
// 最危险的失效方式是其中一边被单独改掉（比如有人觉得 12% 太靠下，
// 在 mac 上改成了 8%），而表面上两边都"功能正常"，只是面板出现的位置
// 不一样，而且只有同时用两个平台的人才会发现。
//
// 做法：把 ObjC 那份逐行转写成 bl 坐标版本，再换算到 tl 坐标与
// RecenterIn 对比。只有"窗口高于工作区"那一格允许不同（那是 RecenterIn
// 里记录的有意偏差）。
func TestRecenterInMatchesDarwin(t *testing.T) {
	// pawPanelRecenterBL 是 paw_panel_recenter 的逐行转写（左下角原点）：
	//	x        = vis.x + (vis.w - winW)/2
	//	bottomY  = vis.y + vis.h - winH - vis.h*0.12；小于 vis.y 时抬到 vis.y
	pawPanelRecenterBL := func(vis Rect, winW, winH int) (int, int) {
		x := vis.X + (vis.W-winW)/2
		bottomY := vis.Y + vis.H - winH - int(float64(vis.H)*recenterTopRatio)
		if bottomY < vis.Y {
			bottomY = vis.Y
		}
		return x, bottomY
	}

	works := []Rect{
		{X: 0, Y: 0, W: 1920, H: 1080},
		{X: -1920, Y: 0, W: 1920, H: 1080},
		{X: 0, Y: 40, W: 2560, H: 1400},
		{X: 1920, Y: -200, W: 1440, H: 900},
	}
	sizes := [][2]int{{560, 760}, {380, 480}, {760, 1100}}

	for _, vis := range works {
		for _, s := range sizes {
			winW, winH := s[0], s[1]

			// bl → tl 的换算：
			//   工作区顶边在 bl 里是 vis.Y+vis.H，在 tl 里是 vis.Y；
			//   窗口顶边在 bl 里是 bottomY+winH；
			//   两者之差（往下为正）加上 tl 的工作区上边界，就是绝对 tl y。
			// ⚠️ 漏掉 `vis.Y +` 这一步时，只有"工作区上边界恰为 0"的用例
			//   还能对上——而多数屏幕就是那个情况，于是这个错会一直藏着。
			bx, bottomY := pawPanelRecenterBL(vis, winW, winH)
			blTop := vis.Y + vis.H
			wantYTL := vis.Y + (blTop - (bottomY + winH))

			gotX, gotY := RecenterIn(vis, winW, winH)

			if gotX != bx {
				t.Fatalf("vis=%+v win=%dx%d：x 与 mac 不一致，Go=%d ObjC=%d", vis, winW, winH, gotX, bx)
			}

			// 窗口不高于工作区时，两条 clamp 是同一件事，必须**逐位相等**。
			if winH <= vis.H && gotY != wantYTL {
				t.Fatalf("vis=%+v win=%dx%d：y 与 mac 不一致，Go=%d ObjC(换算后)=%d",
					vis, winW, winH, gotY, wantYTL)
			}

			// 窗口高于工作区时只允许那一处已记录的偏差：mac 会把顶端推出
			// 工作区，我们夹在工作区顶。
			if winH > vis.H && gotY != vis.Y {
				t.Fatalf("vis=%+v win=%dx%d：窗口高于工作区时应贴住工作区顶部 %d，实得 %d",
					vis, winW, winH, vis.Y, gotY)
			}
		}
	}
}
