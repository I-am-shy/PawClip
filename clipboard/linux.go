//go:build linux

package clipboard

// Linux 骨架（DESIGN.md §10：linux.go —— 预留骨架，不参与编译）。
//
// M1 明确不做 Linux（HANDOFF-PROMPT §二「明确不做」），这里只留一个能被
// build tag 选中的占位实现，保证：
//
//  1. `GOOS=linux go build ./...` 不会因为缺少 NewBackend 而失败；
//  2. 上层拿到的是一个明确的 ErrUnsupported，而不是 nil 解引用；
//  3. 将来要接 X11 / Wayland 时，只需要把本文件的方法体换成真实现，
//     接口与上层代码一行都不用动。
//
// 刻意不返回一个"能编译但会静默什么都不做"的后端——那会让 Linux 上的
// 调用方以为捕获在跑，实际一直空转。

type linuxBackend struct {
	privateChecker
	cfg BackendConfig
}

// NewBackend 在 Linux 上返回 ErrUnsupported。
func NewBackend(cfg BackendConfig) (Backend, error) {
	return nil, ErrUnsupported
}
