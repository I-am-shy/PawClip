package clipboard

import (
	"strings"
	"testing"
	"time"
)

func newTestFilter(cfg FilterConfig, guard *SelfWriteGuard) *Filter {
	return NewFilter(cfg, guard)
}

func baseCfg() FilterConfig { return DefaultFilterConfig() }

func textRaw(s string, appID string) *Raw {
	return &Raw{
		RawTypes:      []string{"public.utf8-plain-text"},
		Text:          &s,
		SourceAppID:   appID,
		SourceAppName: "TestApp",
	}
}

func TestFilter_AcceptsPlainText(t *testing.T) {
	f := newTestFilter(baseCfg(), nil)
	r := textRaw("hello", "com.example.app")
	if d := f.Decide(r, r.Fingerprint(), time.Now()); d != Accept {
		t.Fatalf("d = %s, want accept", d)
	}
}

func TestFilter_Order_PrivateBeatsEverything(t *testing.T) {
	// 同时命中保密标记 + 应用黑名单 + 类型关闭，应该报最靠前的那一条
	cfg := baseCfg()
	cfg.Types = []string{} // 类型也不允许
	f := newTestFilter(cfg, nil)
	s := "密码"
	r := &Raw{
		RawTypes:    []string{"public.utf8-plain-text", "org.nspasteboard.ConcealedType"},
		Text:        &s,
		SourceAppID: "com.1password.mac",
	}
	if d := f.Decide(r, r.Fingerprint(), time.Now()); d != DropPrivate {
		t.Fatalf("d = %s, want %s", d, DropPrivate)
	}
}

func TestFilter_Order_AppBeatsType(t *testing.T) {
	cfg := baseCfg()
	cfg.Types = []string{} // 类型关闭
	f := newTestFilter(cfg, nil)
	r := textRaw("x", "com.agilebits.onepassword7")
	if d := f.Decide(r, r.Fingerprint(), time.Now()); d != DropAppExcluded {
		t.Fatalf("d = %s, want %s", d, DropAppExcluded)
	}
}

func TestFilter_Order_SelfWriteBeatsSize(t *testing.T) {
	guard := &SelfWriteGuard{}
	cfg := baseCfg()
	cfg.ImageMaxBytes = 1 // 任何图片都超限
	f := newTestFilter(cfg, guard)

	png := []byte{1, 2, 3, 4}
	r := &Raw{RawTypes: []string{"public.png"}, Image: &Image{PNG: png, Width: 1, Height: 1}}
	fp := r.Fingerprint()
	guard.Arm(fp)

	if d := f.Decide(r, fp, time.Now()); d != DropSelfWrite {
		t.Fatalf("d = %s, want %s（自写入守卫要排在尺寸上限之前）", d, DropSelfWrite)
	}
}

func TestFilter_DropReasons(t *testing.T) {
	now := time.Now()

	t.Run("总开关关闭", func(t *testing.T) {
		cfg := baseCfg()
		cfg.Enabled = false
		f := newTestFilter(cfg, nil)
		r := textRaw("x", "app")
		if d := f.Decide(r, r.Fingerprint(), now); d != DropCaptureDisabled {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("内容为空", func(t *testing.T) {
		f := newTestFilter(baseCfg(), nil)
		if d := f.Decide(nil, "", now); d != DropEmpty {
			t.Fatalf("nil raw d = %s", d)
		}
		if d := f.Decide(&Raw{RawTypes: []string{"public.utf8-plain-text"}}, "", now); d != DropEmpty {
			t.Fatalf("空快照 d = %s", d)
		}
	})

	t.Run("保密标记", func(t *testing.T) {
		f := newTestFilter(baseCfg(), nil)
		r := textRaw("x", "app")
		r.RawTypes = append(r.RawTypes, "org.nspasteboard.TransientType")
		if d := f.Decide(r, r.Fingerprint(), now); d != DropPrivate {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("保密检查可关闭", func(t *testing.T) {
		cfg := baseCfg()
		cfg.ExcludePrivateTypes = false
		f := newTestFilter(cfg, nil)
		r := textRaw("x", "app")
		r.RawTypes = append(r.RawTypes, "org.nspasteboard.TransientType")
		if d := f.Decide(r, r.Fingerprint(), now); d != Accept {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("应用黑名单支持通配", func(t *testing.T) {
		f := newTestFilter(baseCfg(), nil)
		r := textRaw("x", "com.1password.mac")
		if d := f.Decide(r, r.Fingerprint(), now); d != DropAppExcluded {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("类型开关", func(t *testing.T) {
		cfg := baseCfg()
		cfg.Types = []string{"text"}
		f := newTestFilter(cfg, nil)
		r := &Raw{RawTypes: []string{"public.png"}, Image: &Image{PNG: []byte{1}}}
		if d := f.Decide(r, r.Fingerprint(), now); d != DropTypeDisabled {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("自写入守卫", func(t *testing.T) {
		guard := &SelfWriteGuard{}
		f := newTestFilter(baseCfg(), guard)
		r := textRaw("回贴内容", "app")
		fp := r.Fingerprint()
		guard.Arm(fp)
		if d := f.Decide(r, fp, now); d != DropSelfWrite {
			t.Fatalf("d = %s", d)
		}
	})

	t.Run("去抖", func(t *testing.T) {
		f := newTestFilter(baseCfg(), nil)
		r := textRaw("同一段内容", "app")
		fp := r.Fingerprint()
		// 第一次不是 Accept 才怪；注意必须避开守卫与类型判断
		d1 := f.Decide(r, fp, now)
		if d1 == DropDebounced {
			t.Fatalf("第一次不应触发去抖：%s", d1)
		}
		if d := f.Decide(r, fp, now.Add(10*time.Millisecond)); d != DropDebounced {
			t.Fatalf("窗口内重复 d = %s, want %s", d, DropDebounced)
		}
		if d := f.Decide(r, fp, now.Add(500*time.Millisecond)); d == DropDebounced {
			t.Fatalf("超出窗口不应再去抖：%s", d)
		}
	})

	t.Run("尺寸上限", func(t *testing.T) {
		cfg := baseCfg()
		cfg.ImageMaxBytes = 100
		f := newTestFilter(cfg, nil)
		big := make([]byte, 101)
		r := &Raw{RawTypes: []string{"public.png"}, Image: &Image{PNG: big, Width: 1, Height: 1}}
		if d := f.Decide(r, r.Fingerprint(), now); d != DropTooLarge {
			t.Fatalf("d = %s", d)
		}
		// 恰好等于上限应放行（"> " 而不是 ">= "）
		cfg.ImageMaxBytes = 101
		f2 := newTestFilter(cfg, nil)
		r2 := &Raw{RawTypes: []string{"public.png"}, Image: &Image{PNG: big, Width: 1, Height: 1}}
		if d := f2.Decide(r2, r2.Fingerprint(), now); d != Accept {
			t.Fatalf("恰好等于上限的图片应放行，d = %s", d)
		}
	})
}

// 这是最容易翻车的点：浏览器复制的每个条目都同时带 text 与 html，
// 而默认 capture.types 只有 ["text","image","files"]。
// 如果按 items.kind 判定，"html" 不在列表里就会把网页复制全部丢掉。
func TestFilter_BrowserCopyIsAcceptedUnderDefaultTypes(t *testing.T) {
	f := newTestFilter(baseCfg(), nil)
	s := "网页里的纯文本孪生体"
	h := "<p>网页里的纯文本孪生体</p>"
	r := &Raw{
		RawTypes:    []string{"public.utf8-plain-text", "public.html"},
		Text:        &s,
		HTML:        &h,
		SourceAppID: "com.google.Chrome",
	}
	if d := f.Decide(r, r.Fingerprint(), time.Now()); d != Accept {
		t.Fatalf("浏览器复制被丢掉了：%s", d)
	}
	if kind := r.Content().Kind(); kind != KindHTML {
		t.Fatalf("浏览器复制的 kind = %q, want %q", kind, KindHTML)
	}
}

func TestFilter_Stats(t *testing.T) {
	f := newTestFilter(baseCfg(), nil)
	r := textRaw("x", "com.1password.mac")
	f.Decide(r, r.Fingerprint(), time.Now())
	f.Decide(r, r.Fingerprint(), time.Now())
	st := f.Stats()
	if st[DropAppExcluded.String()] != 2 {
		t.Fatalf("stats = %v", st)
	}
}

func TestDecisionString(t *testing.T) {
	if Accept.String() != "accept" {
		t.Fatal(Accept.String())
	}
	if !DropPrivate.Dropped() || DropPrivate.String() != "drop:private_type" {
		t.Fatalf("DropPrivate: %s", DropPrivate.String())
	}
	if DropTooLarge.Dropped() != true {
		t.Fatal("DropTooLarge 应是丢弃")
	}
	if !strings.HasPrefix(Decision(999).String(), "drop:") {
		t.Fatal("未知决策值应有稳定字符串")
	}
}
