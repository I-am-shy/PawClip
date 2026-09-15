package panel

import "sync"

// 本文件是**平台无关的事件转交**：原生侧（Carbon 热键处理、AppKit 菜单动作、
// Win32 窗口过程）把事件丢进 channel，由一个后台 goroutine 统一回调 Handler。
//
// 为什么要多这一跳，而不是让原生回调直接调 Handler：
//
//  1. 原生回调跑在**平台 UI 线程**上。在那里跑业务逻辑会把 UI 卡住——
//     macOS 上 Carbon 的热键处理是同步执行的，卡住它整个 App 的键盘输入
//     都会跟着卡；Windows 上卡住窗口过程会让托盘菜单无响应。
//  2. 跨 cgo / syscall 边界 panic 会直接终止进程（连 Go 的 stack trace
//     都打不出来）。隔一层 goroutine，panic 至少还能被 recover 住。
//
// Handler 的两个方法因此**都可能在任意 goroutine 上被调用**，
// 实现者（app.go）必须自己保证线程安全——尤其是别在里面直接碰 WebView。

var (
	cbMu      sync.RWMutex
	cbHandler Handler

	cbOnce    sync.Once
	cbHotkeys = make(chan struct{}, 4)
	cbActions = make(chan Action, 16)
)

// bindHandler 把事件出口挂到某个 Handler 上，并确保消费者已启动。
func bindHandler(h Handler) {
	cbMu.Lock()
	cbHandler = h
	cbMu.Unlock()
	cbOnce.Do(func() { go cbDispatch() })
}

// cbDispatch 是唯一的消费者 goroutine。
func cbDispatch() {
	for {
		select {
		case <-cbHotkeys:
			if h := loadHandler(); h != nil {
				h.OnHotkey()
			}
		case a := <-cbActions:
			// 越界值必须挡掉：跨语言边界传过来的是裸整数，
			// 直接当 Action 用会越界读到别的常量。
			if !a.Valid() {
				continue
			}
			if h := loadHandler(); h != nil {
				h.OnAction(a)
			}
		}
	}
}

func loadHandler() Handler {
	cbMu.RLock()
	defer cbMu.RUnlock()
	return cbHandler
}

// emitHotkey / emitAction 是各平台原生回调用的**非阻塞**投递入口。
//
// 非阻塞是刻意的：连按热键时不要在主线程上排队；
// 丢掉一次重复的"呼出"请求比把 UI 线程堵住好得多。
func emitHotkey() {
	select {
	case cbHotkeys <- struct{}{}:
	default:
	}
}

func emitAction(a Action) {
	if !a.Valid() {
		return
	}
	select {
	case cbActions <- a:
	default:
	}
}
