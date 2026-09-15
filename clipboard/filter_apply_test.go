package clipboard

import (
	"sync"
	"testing"
	"time"
)

// TestFilterApply_SwapsRules 验证热重载真的换了判定规则。
//
// 场景就是用户做的事：设置界面里关掉"图片"，然后复制一张图。
func TestFilterApply_SwapsRules(t *testing.T) {
	guard := NewSelfWriteGuard()
	textOnly := FilterConfig{
		Enabled:             true,
		Types:               []string{KindText},
		ExcludePrivateTypes: true,
		ImageMaxBytes:       1 << 20,
		DebounceMs:          10,
	}
	f := NewFilter(textOnly, guard)

	now := time.Now()
	img := &Raw{RawTypes: []string{"public.png"}, Image: &Image{PNG: make([]byte, 16)}}

	// ① 只允许 text 时，图片该被"类型开关"挡下。
	if d := f.Decide(img, "fp-img", now); d != DropTypeDisabled {
		t.Fatalf("图片在 types=[text] 下结论 = %v，想要 %v", d, DropTypeDisabled)
	}

	// ② 热重载加上 image。
	withImage := textOnly
	withImage.Types = []string{KindText, KindImage}
	f.Apply(withImage)

	// 换规则会 Reset 去抖，所以同一指纹+同一时刻现在应当被放行。
	if d := f.Decide(img, "fp-img", now); d != Accept {
		t.Fatalf("重载后图片结论 = %v，想要 Accept（新规则允许 image）", d)
	}

	// ③ 再关掉总开关，连文本都不该过。
	off := withImage
	off.Enabled = false
	f.Apply(off)
	txt := &Raw{RawTypes: []string{"public.utf8-plain-text"}, Text: strptr("hello")}
	if d := f.Decide(txt, "fp-txt", now); d != DropCaptureDisabled {
		t.Fatalf("总开关关闭后结论 = %v，想要 %v", d, DropCaptureDisabled)
	}
}

// TestFilterApply_KeepsCounters 验证计数器不被重置。
//
// 这条防的是一个很隐蔽的退化：如果 Apply 顺手把 counts 清零，
// 那么"用户在设置里改一次东西"就会让验收证据（"累计丢弃 N 条"）倒退回去，
// 而所有功能看起来都是好的。
func TestFilterApply_KeepsCounters(t *testing.T) {
	f := NewFilter(DefaultFilterConfig(), nil)
	now := time.Now()
	priv := &Raw{RawTypes: []string{"org.nspasteboard.ConcealedType"}, Text: strptr("secret")}

	for i := 0; i < 3; i++ {
		if d := f.Decide(priv, "fp", now); d != DropPrivate {
			t.Fatalf("第 %d 次结论 = %v，想要 %v", i+1, d, DropPrivate)
		}
	}
	before := f.Stats()[DropPrivate.String()]
	if before != 3 {
		t.Fatalf("重载前 private 计数 = %d，想要 3", before)
	}

	cfg := DefaultFilterConfig()
	cfg.DebounceMs = 500
	f.Apply(cfg)

	after := f.Stats()[DropPrivate.String()]
	if after != before {
		t.Errorf("Apply 之后 private 计数变成 %d（原 %d）——计数器不该被重置，"+
			"它是累计证据，重置会让验收里的'累计丢弃 N 条'倒退", after, before)
	}

	// 而且新的一次判定要接着累加，而不是从头开始。
	_ = f.Decide(priv, "fp2", now)
	if got := f.Stats()[DropPrivate.String()]; got != before+1 {
		t.Errorf("再判定一次后 private 计数 = %d，想要 %d", got, before+1)
	}
}

// TestFilterApply_ResetsDebounce 验证换规则会清掉去抖记忆。
//
// 保留旧的去抖状态会造成一种很难查的现象：改完配置、又立刻复制同一份内容，
// 结果什么都不发生（旧规则刚把它去抖掉了），用户会以为"设置没生效"。
func TestFilterApply_ResetsDebounce(t *testing.T) {
	cfg := DefaultFilterConfig()
	cfg.DebounceMs = 60_000 // 一个足够长的窗口，确保"没被重置"就一定会被去抖
	f := NewFilter(cfg, nil)

	raw := &Raw{RawTypes: []string{"public.utf8-plain-text"}, Text: strptr("同一份内容")}
	now := time.Now()

	if d := f.Decide(raw, "fp", now); d != Accept {
		t.Fatalf("首次结论 = %v，想要 Accept", d)
	}
	// 同一时刻、同一指纹：应该被去抖。
	if d := f.Decide(raw, "fp", now); d != DropDebounced {
		t.Fatalf("同指纹立即重来结论 = %v，想要 %v", d, DropDebounced)
	}

	// 换配置（哪怕只是改个尺寸上限）→ 去抖记忆该被清掉。
	cfg2 := cfg
	cfg2.ImageMaxBytes = 4 << 20
	f.Apply(cfg2)

	if d := f.Decide(raw, "fp", now); d != Accept {
		t.Errorf("Apply 之后同一指纹的结论 = %v，想要 Accept"+
			"（换规则后去抖记忆必须清掉，否则用户会以为设置没生效）", d)
	}
}

// TestFilterApply_ConfigRoundTrip 验证 Config() 能回读，便于诊断。
func TestFilterApply_ConfigRoundTrip(t *testing.T) {
	cfg := FilterConfig{
		Enabled:             true,
		Types:               []string{KindText, KindFiles},
		ExcludeApps:         []string{"com.example.*"},
		ExcludePrivateTypes: false,
		ImageMaxBytes:       123,
		DebounceMs:          7,
	}
	f := NewFilter(cfg, nil)
	got := f.Config()
	if got.ImageMaxBytes != 123 || got.DebounceMs != 7 || got.ExcludePrivateTypes {
		t.Errorf("Config() = %+v，与传入的不一致", got)
	}
	if len(got.Types) != 2 || len(got.ExcludeApps) != 1 {
		t.Errorf("Config() 的切片不对：types=%v apps=%v", got.Types, got.ExcludeApps)
	}
}

// TestFilterApply_ConcurrentWithDecide 用 -race 跑"一边判定一边热重载"。
//
// 这是 atomic.Pointer 方案要证明的东西：热路径无锁读、重载整体替换，
// 两者并发时既不能数据竞争，也不能读到半更新的状态。
func TestFilterApply_ConcurrentWithDecide(t *testing.T) {
	f := NewFilter(DefaultFilterConfig(), NewSelfWriteGuard())
	raw := &Raw{RawTypes: []string{"public.utf8-plain-text"}, Text: strptr("并发内容")}
	stop := make(chan struct{})

	var wg sync.WaitGroup
	// 判定者
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			now := time.Now()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				// 每轮的指纹都不同，避免被去抖吃掉（我们要压的是配置读路径，
				// 不是去抖器）。
				_ = f.Decide(raw, "fp-"+string(rune('a'+n))+string(rune('0'+j%10)), now)
			}
		}(i)
	}
	// 重载者
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			cfg := DefaultFilterConfig()
			cfg.DebounceMs = 10 + i%50
			cfg.ImageMaxBytes = int64(1<<20) + int64(i)
			f.Apply(cfg)
		}
		close(stop)
	}()
	wg.Wait()

	// 收尾后配置必须是"最后一次 Apply 的那份"的合法状态，不能是空/nil。
	if c := f.Config(); c.ImageMaxBytes < 1<<20 {
		t.Errorf("并发重载后 Config() = %+v，看起来读到了半更新状态", c)
	}
}

func strptr(s string) *string { return &s }
