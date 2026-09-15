package clipboard

import (
	"sync"
	"time"
)

// selfWriteWindow 是"我们自己刚写完剪贴板"的有效判定窗口。
const selfWriteWindow = 2 * time.Second

// SelfWriteGuard 解决"我们写回剪贴板后又被自己捕获"的问题。
// 比记录 changeCount 更稳：Windows 上拿不到自身写入对应的序号，
// 指纹比对天然跨平台。
type SelfWriteGuard struct {
	mu       sync.Mutex
	expected string
	armedAt  time.Time

	// dropped 累计拦下的自写入次数，仅供诊断/测试读取。
	dropped int64
}

// NewSelfWriteGuard 构造守卫。
//
// 零值本身就可直接用（DESIGN.md §2 的实现没有需要初始化的字段），
// 提供构造函数只是为了让上层不必依赖"零值可用"这个隐含契约。
func NewSelfWriteGuard() *SelfWriteGuard { return &SelfWriteGuard{} }

// Arm 在写剪贴板之前调用。
func (g *SelfWriteGuard) Arm(fingerprint string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expected = fingerprint
	g.armedAt = time.Now()
}

// ShouldDrop 在捕获到内容之后调用，返回 true 表示这次事件应被丢弃。
func (g *SelfWriteGuard) ShouldDrop(fingerprint string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.expected == "" {
		return false
	}
	// 指纹一致且在窗口内 → 是我们自己写的
	if g.expected == fingerprint && time.Since(g.armedAt) < selfWriteWindow {
		g.expected = ""
		g.dropped++
		return true
	}
	// 标记已陈旧，清掉后放行
	if time.Since(g.armedAt) >= selfWriteWindow {
		g.expected = ""
	}
	return false
}

// Reset 清掉未消费的标记。写剪贴板失败时调用，避免残留标记误杀后续一次真实捕获。
func (g *SelfWriteGuard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expected = ""
}

// Armed 当前是否有未消费的标记（诊断用）。
func (g *SelfWriteGuard) Armed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.expected != ""
}

// Dropped 累计被拦下的自写入次数（诊断用）。
func (g *SelfWriteGuard) Dropped() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dropped
}
