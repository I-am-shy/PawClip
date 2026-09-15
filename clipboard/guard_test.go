package clipboard

import (
	"sync"
	"testing"
	"time"
)

func TestSelfWriteGuard_DropsOwnWrite(t *testing.T) {
	var g SelfWriteGuard
	fp := "sha256:abc"

	if g.ShouldDrop(fp) {
		t.Fatal("未 Arm 之前不得丢弃任何内容")
	}

	g.Arm(fp)
	if !g.Armed() {
		t.Fatal("Arm 之后 Armed 应为真")
	}
	if !g.ShouldDrop(fp) {
		t.Fatal("窗口内同指纹必须丢弃")
	}
	if g.Armed() {
		t.Fatal("命中后标记必须被清掉（否则后续一次真实捕获也会被误杀）")
	}
	if g.ShouldDrop(fp) {
		t.Fatal("标记已消费，第二次不得再丢弃")
	}
	if g.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", g.Dropped())
	}
}

func TestSelfWriteGuard_DifferentFingerprintPasses(t *testing.T) {
	var g SelfWriteGuard
	g.Arm("sha256:ours")
	if g.ShouldDrop("sha256:someone-else") {
		t.Fatal("指纹不同必须放行")
	}
	if !g.Armed() {
		t.Fatal("未命中不应消耗标记")
	}
	if !g.ShouldDrop("sha256:ours") {
		t.Fatal("随后我们自己的那次仍应被拦下")
	}
}

func TestSelfWriteGuard_StaleMarkerExpires(t *testing.T) {
	var g SelfWriteGuard
	g.Arm("sha256:x")
	// 手工把时间戳拨到窗口之外
	g.mu.Lock()
	g.armedAt = time.Now().Add(-(selfWriteWindow + time.Second))
	g.mu.Unlock()

	if g.ShouldDrop("sha256:x") {
		t.Fatal("窗口已过期，不得再丢弃")
	}
	if g.Armed() {
		t.Fatal("过期标记必须被清掉")
	}
}

func TestSelfWriteGuard_Reset(t *testing.T) {
	var g SelfWriteGuard
	g.Arm("sha256:x")
	g.Reset()
	if g.Armed() {
		t.Fatal("Reset 后不得仍处于 Armed")
	}
	if g.ShouldDrop("sha256:x") {
		t.Fatal("Reset 后不得丢弃")
	}
}

// 100 次连续回贴的场景（HANDOFF-PROMPT 验收 1）在守卫这一层的等价断言。
func TestSelfWriteGuard_HundredArmDropCycles(t *testing.T) {
	var g SelfWriteGuard
	for i := 0; i < 100; i++ {
		fp := "sha256:same-content" // 每次回贴同一内容
		g.Arm(fp)
		if !g.ShouldDrop(fp) {
			t.Fatalf("第 %d 次未被拦下", i+1)
		}
	}
	if g.Dropped() != 100 {
		t.Fatalf("Dropped() = %d, want 100", g.Dropped())
	}
}

func TestSelfWriteGuard_Concurrent(t *testing.T) {
	var g SelfWriteGuard
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				g.Arm("sha256:race")
				g.ShouldDrop("sha256:race")
				g.Armed()
				g.Dropped()
			}
		}()
	}
	wg.Wait()
	if g.Armed() {
		t.Fatal("并发跑完不应残留标记")
	}
}
