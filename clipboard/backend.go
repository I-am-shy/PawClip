// Package clipboard 是平台原生的剪贴板抽象层。
//
// 平台差异被收敛到 Backend 接口后面：macOS 走 cgo + Objective-C shim，
// Windows 走 golang.org/x/sys/windows（无需 cgo）。上层（捕获流水线、回写）
// 完全平台无关。
//
// 接口签名严格照 docs/DESIGN.md §2，不要改。
package clipboard

import (
	"errors"
	"time"
)

// 哨兵错误。上层用 errors.Is 判定。
var (
	// ErrUnsupported 表示当前平台没有可用的后端实现（Linux 骨架）。
	ErrUnsupported = errors.New("clipboard: backend unsupported on this platform")
	// ErrEmpty 表示剪贴板里没有任何我们认识的表示。
	ErrEmpty = errors.New("clipboard: no supported representation on the clipboard")
	// ErrBusy 表示剪贴板被其他进程独占（Windows OpenClipboard 返回 ACCESS_DENIED）。
	ErrBusy = errors.New("clipboard: clipboard is held by another process")
	// ErrFlapping 表示读取期间 changeCount 仍在变，重试次数用尽。
	ErrFlapping = errors.New("clipboard: changeCount kept changing while reading")
	// ErrUnsupportedImage 表示图片的编码形式我们无法转换（如 1bpp 调色板以外的一些变体）。
	ErrUnsupportedImage = errors.New("clipboard: unsupported image encoding")
)

// Backend 是平台原生的剪贴板后端。macOS 与 Windows 各有一个实现，
// 通过 build tag 选择编译，上层完全平台无关。
type Backend interface {
	// Start 启动监听，变更时向 ch 推送信号（只推信号，不推内容）
	Start(ch chan<- Tick) error
	Stop()
	// Read 读取剪贴板全部可用表示。Windows 实现内部自带重试。
	Read() (*Raw, error)
	// Write 写回内容
	Write(p *Payload) error
	// IsPrivate 平台原生"请勿记录"标记检测
	IsPrivate(r *Raw) bool
}

// Tick 是变更信号。真正的读取由捕获 goroutine 去做
// （Windows 需要重试与延迟，不能在这里做）。
type Tick struct {
	AtMs int64
	Seq  uint64
}

// Raw 是归一化后的剪贴板快照。
type Raw struct {
	RawTypes      []string // 原始类型名，用于保密判定与诊断
	Text          *string  // UTF-8
	HTML          *string  // 已剥离平台包装
	RTF           []byte   // 原始 RTF 字节
	Image         *Image   // 已转 PNG + 像素尺寸
	Files         []string
	SourceAppID   string // macOS: bundle id / Windows: exe 路径或 AppUserModelID
	SourceAppName string
}

// Image 是归一化后的位图。
type Image struct {
	PNG    []byte
	Width  int
	Height int
}

// Payload 是要写回剪贴板的内容。nil / 空切片表示该表示不写入。
//
// 与 Raw 的差别：Payload 只承载"要写什么"，不含来源应用等采集期元数据。
type Payload struct {
	Text  *string
	HTML  *string
	RTF   []byte
	PNG   []byte
	Files []string
}

// Empty 判断 payload 是否什么都没带。
func (p *Payload) Empty() bool {
	if p == nil {
		return true
	}
	return p.Text == nil && p.HTML == nil &&
		len(p.RTF) == 0 && len(p.PNG) == 0 && len(p.Files) == 0
}

// privateChecker 把"保密标记判定"实现一次，供两个平台后端内嵌复用。
//
// 之所以内嵌而不是直接用包级函数：Backend 接口要求 IsPrivate 是方法，
// 内嵌既满足接口，又只有一份实现。平台侧若需要追加本地标记，
// 可以在自己的后端类型上再定义同名方法把它遮蔽掉。
type privateChecker struct{}

// IsPrivate 判断快照是否带平台原生"请勿记录"标记。
func (privateChecker) IsPrivate(r *Raw) bool {
	if r == nil {
		return false
	}
	return IsPrivateTypeNames(r.RawTypes)
}

// tickClock 给各平台后端复用：把当前时间转成 Tick.AtMs。
func tickClock(t time.Time) int64 { return t.UnixMilli() }

// BackendConfig 是构造平台后端时的调参，取自 settings 表（docs/DESIGN.md §9）。
type BackendConfig struct {
	// PollIntervalActiveMs ← capture.pollIntervalActiveMs（macOS 专用）
	PollIntervalActiveMs int
	// PollIntervalIdleMs ← capture.pollIntervalIdleMs（macOS 专用）
	PollIntervalIdleMs int
	// IdleThresholdSec ← capture.idleThresholdSec（macOS 专用）
	IdleThresholdSec int
}

// withDefaults 补齐 §9 的默认值：200ms 活跃 / 1000ms 空闲 / 60s 判定阈值。
//
// Windows 完全用不到这三项（事件驱动，无轮询），保留在同一个结构体里只是
// 为了让上层的构造代码不必按平台分叉。
func (c BackendConfig) withDefaults() BackendConfig {
	out := c
	if out.PollIntervalActiveMs <= 0 {
		out.PollIntervalActiveMs = 200
	}
	if out.PollIntervalIdleMs <= 0 {
		out.PollIntervalIdleMs = 1000
	}
	if out.IdleThresholdSec <= 0 {
		out.IdleThresholdSec = 60
	}
	return out
}
