//go:build darwin && cgo

package clipboard

/*
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics
#include <stdlib.h>
#include "pasteboard_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// macOS 后端。
//
// 平台约束（docs/DESIGN.md §3 / §7）：
//   - 系统没有剪贴板变更通知，只能轮询 NSPasteboard.changeCount；
//   - 自适应间隔：检测到用户空闲 > idleThresholdSec 时从 200ms 降到 1000ms，
//     否则笔记本续航会被 0.2s 轮询吃掉；
//   - 来源 App 必须与剪贴板内容在**同一次**原生调用里取（§7 第 2 条）。

type darwinBackend struct {
	privateChecker
	cfg BackendConfig

	mu   sync.Mutex
	ch   chan<- Tick
	stop chan struct{}
}

// NewBackend 返回当前平台的剪贴板后端。
func NewBackend(cfg BackendConfig) (Backend, error) {
	return &darwinBackend{cfg: cfg.withDefaults()}, nil
}

// Start 启动轮询 goroutine，变更时向 ch 推送信号（只推信号，不推内容）。
func (b *darwinBackend) Start(ch chan<- Tick) error {
	if ch == nil {
		return errors.New("clipboard: nil tick channel")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch != nil {
		return errors.New("clipboard: darwin backend already started")
	}
	b.ch = ch
	stop := make(chan struct{})
	b.stop = stop
	// ch 以参数形式传进去：poll 不读 b.ch，于是 Stop 改 b.ch 不会与它竞争。
	go b.poll(ch, stop)
	return nil
}

// Stop 停止轮询。可重复调用。
func (b *darwinBackend) Stop() {
	b.mu.Lock()
	stop := b.stop
	b.stop = nil
	b.ch = nil
	b.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// poll 是轮询循环。
//
// ⚠️ 这是全项目唯一的热路径，docs/DESIGN.md §14 第 9 条对它有硬要求：
//   - 每 0.2s 只读一个 NSInteger（C.pb_change_count 内部就是纯 ivar 读取）；
//   - 循环里不构造 Objective-C 对象（不取 types、不取字符串）；
//   - Go 侧循环内不 defer、不新建结构体。
//
// 所以下面用复用的 Timer 而不是 time.After（后者每轮都会分配 Timer + channel）。
func (b *darwinBackend) poll(ch chan<- Tick, stop chan struct{}) {
	// 固定到一条 OS 线程：整个进程生命周期只锁一次，代价可忽略，
	// 换来的是所有 AppKit 调用始终来自同一条线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	active := time.Duration(b.cfg.PollIntervalActiveMs) * time.Millisecond
	idle := time.Duration(b.cfg.PollIntervalIdleMs) * time.Millisecond
	threshold := float64(b.cfg.IdleThresholdSec)

	timer := time.NewTimer(active)
	defer timer.Stop()

	last := C.pb_change_count()
	var seq uint64

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}

		if float64(C.pb_seconds_since_last_input()) > threshold {
			timer.Reset(idle)
		} else {
			timer.Reset(active)
		}

		now := C.pb_change_count()
		if now == last {
			continue
		}
		last = now

		seq++
		select {
		case ch <- Tick{AtMs: time.Now().UnixMilli(), Seq: seq}:
		default:
			// 队列满说明捕获侧已经落后了。丢这个信号是安全的：捕获侧每读一次
			// 拿到的都是剪贴板**当前状态**，排队中的信号自然会把它读走。
		}
	}
}

// Read 读取剪贴板的全部可用表示。
//
// 内部带抖动重试：读到快照后再看一次 changeCount，不一致说明读取过程中
// 剪贴板又被写了，这一份快照不可信（docs/DESIGN.md §4 第 12 条）。
func (b *darwinBackend) Read() (*Raw, error) {
	const maxAttempts = 3

	var (
		snap   C.pb_snapshot
		errbuf [512]C.char
	)

	for attempt := 1; ; attempt++ {
		if rc := C.pb_read(&snap, &errbuf[0], C.int(len(errbuf))); rc != 0 {
			return nil, fmt.Errorf("clipboard: darwin read failed: %s", C.GoString(&errbuf[0]))
		}
		if int64(C.pb_change_count()) == int64(snap.change_count) {
			break
		}
		C.pb_snapshot_free(&snap)
		if attempt >= maxAttempts {
			return nil, ErrFlapping
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer C.pb_snapshot_free(&snap)

	return snapshotToRaw(&snap), nil
}

// Write 写回剪贴板。
func (b *darwinBackend) Write(p *Payload) error {
	if p == nil || p.Empty() {
		return errors.New("clipboard: empty payload")
	}

	var (
		cText, cHTML *C.char
		cRTF, cPNG   unsafe.Pointer
		rtfLen       C.int
		pngLen       C.int
		filePtrs     []*C.char
	)

	if p.Text != nil {
		cText = C.CString(*p.Text)
		defer C.free(unsafe.Pointer(cText))
	}
	if p.HTML != nil {
		cHTML = C.CString(*p.HTML)
		defer C.free(unsafe.Pointer(cHTML))
	}
	if len(p.RTF) > 0 {
		cRTF = C.CBytes(p.RTF)
		rtfLen = C.int(len(p.RTF))
		defer C.free(cRTF)
	}
	if len(p.PNG) > 0 {
		cPNG = C.CBytes(p.PNG)
		pngLen = C.int(len(p.PNG))
		defer C.free(cPNG)
	}
	var filesArg **C.char
	if len(p.Files) > 0 {
		filePtrs = make([]*C.char, len(p.Files))
		for i, f := range p.Files {
			if filePtrs[i] = C.CString(f); filePtrs[i] != nil {
				defer C.free(unsafe.Pointer(filePtrs[i]))
			}
		}
		filesArg = &filePtrs[0]
	}

	var errbuf [512]C.char
	rc := C.pb_write(
		cText, cHTML,
		cRTF, rtfLen,
		cPNG, pngLen,
		filesArg, C.int(len(filePtrs)),
		&errbuf[0], C.int(len(errbuf)),
	)
	if rc != 0 {
		return fmt.Errorf("clipboard: darwin write failed: %s", C.GoString(&errbuf[0]))
	}
	return nil
}

// snapshotToRaw 把 C 侧快照拷进 Go 内存。
//
// 每个字段都做了**拷贝**：C 侧那块内存马上会被 pb_snapshot_free 释放，
// 返回的 *Raw 必须能独立存活。
func snapshotToRaw(s *C.pb_snapshot) *Raw {
	r := &Raw{}

	if s.types_json != nil {
		if err := json.Unmarshal([]byte(C.GoString(s.types_json)), &r.RawTypes); err != nil {
			// 类型名缺失只会影响保密判定与诊断，不该让整次捕获失败
			r.RawTypes = nil
		}
	}
	if s.text != nil {
		v := C.GoString(s.text)
		r.Text = &v
	}
	if s.html != nil {
		v := C.GoString(s.html)
		r.HTML = &v
	}
	if s.rtf != nil && s.rtf_len > 0 {
		r.RTF = C.GoBytes(s.rtf, s.rtf_len)
	}
	if s.png != nil && s.png_len > 0 {
		// 原样保留 PNG 字节（不重编码）——items.fingerprint 就是这些字节的 sha256，
		// 也是 SelfWriteGuard 能认出"这是我自己刚写的"的前提。
		if img, err := ImageFromPNG(C.GoBytes(s.png, s.png_len)); err == nil {
			r.Image = img
		}
	}
	if s.files_json != nil {
		if err := json.Unmarshal([]byte(C.GoString(s.files_json)), &r.Files); err != nil {
			r.Files = nil
		}
	}
	if s.app_id != nil {
		r.SourceAppID = C.GoString(s.app_id)
	}
	if s.app_name != nil {
		r.SourceAppName = C.GoString(s.app_name)
	}
	return r
}
