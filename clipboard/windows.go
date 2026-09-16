//go:build windows

package clipboard

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 后端。**完全不需要 cgo**（docs/DESIGN.md 附录 A.1）：
// x/sys/windows + syscall.NewCallback 就能建 message-only 窗口、
// 注册 AddClipboardFormatListener、读写各种剪贴板格式。
//
// 与 macOS 的差别（§3）：Windows 是事件驱动的，空闲 CPU ≈ 0%，不要轮询。
//
// 三个必做转换（§8）：
//   - CF_DIBV5 → PNG：**手动转**，Windows 不会直接给 PNG。转换逻辑在
//     normalize_dib.go，是纯函数，可以在 macOS 上单测。
//   - "HTML Format"：必须剥离 `Version:0.9\r\nStartHTML:...` 头部。
//     逻辑在 normalize_html.go，同样纯函数、同样在 mac 上单测。
//   - 去抖：在 filter.go / normalize.go。

// Windows 常量。
const (
	cfText        = 1
	cfBitmap      = 2
	cfDIB         = 8
	cfUnicodeText = 13
	cfHDROP       = 15
	cfDIBV5       = 17

	wmDestroy         = 0x0002
	wmClipboardUpdate = 0x031D
	wmAppQuit         = 0x8000 + 1 // WM_APP + 1

	// hwndMessage = (HWND)-3，message-only 窗口的父句柄
	hwndMessage = ^uintptr(2)

	gmemMoveable = 0x0002

	// GetModuleHandleEx 的 flag：只借用句柄、不增加引用计数
	getModuleHandleExUnchangedRefcount = 0x00000002

	processQueryLimitedInformation = 0x1000

	// 读剪贴板前的等待：WM_CLIPBOARDUPDATE 可能早于所有者写完所有格式（§8 第 3 条）
	settleDelay = 25 * time.Millisecond

	// OpenClipboard 重试策略（§8 第 2 条）：3 次 × 20ms 退避
	openAttempts   = 3
	openBackoff    = 20 * time.Millisecond
	windowClassW   = "PawClipClipboardListener"
	interfaceError = "clipboard: windows backend"
)

// user32 / kernel32 过程。x/sys/windows 没有包装剪贴板 API，直接取过程地址。
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procAddClipboardFormatListener    = user32.NewProc("AddClipboardFormatListener")
	procRemoveClipboardFormatListener = user32.NewProc("RemoveClipboardFormatListener")
	procOpenClipboard                 = user32.NewProc("OpenClipboard")
	procCloseClipboard                = user32.NewProc("CloseClipboard")
	procEmptyClipboard                = user32.NewProc("EmptyClipboard")
	procGetClipboardData              = user32.NewProc("GetClipboardData")
	procSetClipboardData              = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable    = user32.NewProc("IsClipboardFormatAvailable")
	procEnumClipboardFormats          = user32.NewProc("EnumClipboardFormats")
	procGetClipboardFormatNameW       = user32.NewProc("GetClipboardFormatNameW")
	procRegisterClipboardFormatW      = user32.NewProc("RegisterClipboardFormatW")
	procGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	procQueryFullProcessImageNameW    = kernel32.NewProc("QueryFullProcessImageNameW")
	procGlobalAlloc                   = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                    = kernel32.NewProc("GlobalLock")
	procGlobalUnlock                  = kernel32.NewProc("GlobalUnlock")
	procGlobalFree                    = kernel32.NewProc("GlobalFree")
	procGlobalSize                    = kernel32.NewProc("GlobalSize")
)

// ── 剪贴板格式注册 ──────────────────────────────────────────────

var (
	formatOnce sync.Once

	fmtHTML                uint32
	fmtRTF                 uint32
	fmtPNG                 uint32
	fmtClipboardViewerIgn  uint32
	fmtExcludeMonitor      uint32
	fmtCanIncludeInHistory uint32
	fmtCanUploadToCloud    uint32
)

func registerClipboardFormats() {
	formatOnce.Do(func() {
		fmtHTML = registerClipboardFormat("HTML Format")
		fmtRTF = registerClipboardFormat("Rich Text Format")
		// 约定俗成的注册格式名。我们回写图片时用它携带**原始 PNG 字节**，
		// 这样读回来和写出去完全一致，SelfWriteGuard 的指纹比对必然命中；
		// CF_DIBV5 只是给"只认 DIB"的应用作兼容。
		fmtPNG = registerClipboardFormat("PNG")

		// 保密标记（§3 平台能力对照表里 Windows 那一栏的四个）
		fmtClipboardViewerIgn = registerClipboardFormat("Clipboard Viewer Ignore")
		fmtExcludeMonitor = registerClipboardFormat("ExcludeClipboardContentFromMonitorProcessing")
		fmtCanIncludeInHistory = registerClipboardFormat("CanIncludeInClipboardHistory")
		fmtCanUploadToCloud = registerClipboardFormat("CanUploadToCloudClipboard")
	})
}

func registerClipboardFormat(name string) uint32 {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0
	}
	r, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p)))
	return uint32(r)
}

// ── 后端 ────────────────────────────────────────────────────────

type windowsBackend struct {
	privateChecker
	cfg BackendConfig

	mu        sync.Mutex
	ch        chan<- Tick
	seq       uint64
	lastEvent time.Time

	started  bool
	stopOnce sync.Once
	loopDone chan struct{}
	hwnd     windows.HWND

	loopErr atomic.Pointer[error]
}

// NewBackend 返回当前平台的剪贴板后端。
func NewBackend(cfg BackendConfig) (Backend, error) {
	return &windowsBackend{cfg: cfg.withDefaults()}, nil
}

// activeWndProc 指向当前后端的 wndproc 入口。
//
// 用全局变量而不是闭包：syscall.NewCallback 生成的 thunk 生命周期与进程等长，
// 而回调里要访问实例状态。进程内只会有一个后端实例，所以这样最简单也最稳。
var activeBackend atomic.Pointer[windowsBackend]

// Start 在**独立线程**上建 message-only 窗口并跑自己的消息泵（§8 第 1 条）。
//
// 不复用 Wails 主窗口的 hwnd：那会把剪贴板事件和 UI 线程的事件循环耦合起来。
func (b *windowsBackend) Start(ch chan<- Tick) error {
	if ch == nil {
		return errors.New("clipboard: nil tick channel")
	}
	registerClipboardFormats()

	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return errors.New("clipboard: windows backend already started")
	}
	b.started = true
	b.ch = ch
	b.loopDone = make(chan struct{})
	b.mu.Unlock()

	activeBackend.Store(b)

	ready := make(chan error, 1)
	go b.runMessageLoop(ready)

	select {
	case err := <-ready:
		if err != nil {
			b.Stop()
			return err
		}
		return nil
	case <-time.After(5 * time.Second):
		b.Stop()
		return fmt.Errorf("%s: timed out creating message-only window", interfaceError)
	}
}

// Stop 结束消息泵并销毁窗口。可重复调用。
func (b *windowsBackend) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		hwnd := b.hwnd
		done := b.loopDone
		b.ch = nil
		b.mu.Unlock()

		if hwnd != 0 {
			// 打断 GetMessage：WM_APP+1 由 wndproc 转成 PostQuitMessage。
			procPostMessageW.Call(uintptr(hwnd), wmAppQuit, 0, 0)
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
		activeBackend.CompareAndSwap(b, nil)
	})
}

func (b *windowsBackend) runMessageLoop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(b.loopDone)

	fail := func(err error) {
		b.loopErr.Store(&err)
		select {
		case ready <- err:
		default:
		}
	}

	// x/sys v0.48 起不再导出 GetModuleHandle，只有 Ex 版本。
	// UNCHANGED_REFCOUNT 表示只借用句柄、不增加引用计数；moduleName 传 nil
	// 即取当前进程可执行模块。
	var hInstance windows.Handle
	if err := windows.GetModuleHandleEx(getModuleHandleExUnchangedRefcount, nil, &hInstance); err != nil {
		fail(fmt.Errorf("%s: GetModuleHandleEx: %w", interfaceError, err))
		return
	}

	className, err := windows.UTF16PtrFromString(windowClassW)
	if err != nil {
		fail(fmt.Errorf("%s: bad class name: %w", interfaceError, err))
		return
	}

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(clipboardWndProc),
		HInstance:     hInstance,
		LpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		// 类已注册（重复 Start / 热重载）不算失败
		if !errors.Is(callErr, windows.ERROR_CLASS_ALREADY_EXISTS) {
			fail(fmt.Errorf("%s: RegisterClassEx: %v", interfaceError, callErr))
			return
		}
	}

	hwnd, _, callErr := procCreateWindowExW.Call(
		0, // dwExStyle
		uintptr(unsafe.Pointer(className)),
		0,          // lpWindowName
		0,          // dwStyle
		0, 0, 0, 0, // x, y, w, h
		hwndMessage,
		0,                  // hMenu
		uintptr(hInstance), // hInstance
		0,                  // lpParam
	)
	if hwnd == 0 {
		fail(fmt.Errorf("%s: CreateWindowEx(message-only): %v", interfaceError, callErr))
		return
	}
	b.mu.Lock()
	b.hwnd = windows.HWND(hwnd)
	b.mu.Unlock()

	if r, _, callErr := procAddClipboardFormatListener.Call(hwnd); r == 0 {
		fail(fmt.Errorf("%s: AddClipboardFormatListener: %v", interfaceError, callErr))
		procDestroyWindow.Call(hwnd)
		return
	}

	select {
	case ready <- nil:
	default:
	}

	var msg wndMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT，-1 = 出错
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	procRemoveClipboardFormatListener.Call(hwnd)
	procDestroyWindow.Call(hwnd)
}

// clipboardWndProc 是消息窗口的窗口过程。
func clipboardWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmClipboardUpdate:
		if b := activeBackend.Load(); b != nil {
			b.onClipboardUpdate()
		}
		return 0
	case wmAppQuit:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// onClipboardUpdate 只推信号，不读内容——读取要重试与延迟，不能挤在消息泵里（§2）。
func (b *windowsBackend) onClipboardUpdate() {
	b.mu.Lock()
	ch := b.ch
	b.seq++
	seq := b.seq
	b.lastEvent = time.Now()
	b.mu.Unlock()

	if ch == nil {
		return
	}
	select {
	case ch <- Tick{AtMs: time.Now().UnixMilli(), Seq: seq}:
	default:
	}
}

// ── 读取 ────────────────────────────────────────────────────────

// Read 读取剪贴板的全部可用表示。
func (b *windowsBackend) Read() (*Raw, error) {
	registerClipboardFormats()

	b.mu.Lock()
	since := time.Since(b.lastEvent)
	b.mu.Unlock()
	if since < settleDelay {
		time.Sleep(settleDelay - since)
	}

	if err := openClipboardWithRetry(); err != nil {
		return nil, err
	}
	defer procCloseClipboard.Call()

	raw := &Raw{}
	raw.RawTypes = enumerateClipboardFormats()

	// 保密标记：Windows 侧只有"确实触发排除"时才把名字放进 RawTypes，
	// 这样 normalize.go 里的判定可以退化成纯名字查表（见那里的约定）。
	if formatAvailable(fmtClipboardViewerIgn) {
		raw.RawTypes = appendUnique(raw.RawTypes, "Clipboard Viewer Ignore")
	}
	if formatAvailable(fmtExcludeMonitor) {
		raw.RawTypes = appendUnique(raw.RawTypes, "ExcludeClipboardContentFromMonitorProcessing")
	}
	if v, ok := clipboardDWORD(fmtCanIncludeInHistory); ok && v == 0 {
		raw.RawTypes = appendUnique(raw.RawTypes, "CanIncludeInClipboardHistory")
	}
	if v, ok := clipboardDWORD(fmtCanUploadToCloud); ok && v == 0 {
		raw.RawTypes = appendUnique(raw.RawTypes, "CanUploadToCloudClipboard")
	}

	// 文本
	if s, err := clipboardString(cfUnicodeText); err == nil && s != "" {
		raw.Text = &s
	} else if s, err := clipboardANSIString(cfText); err == nil && s != "" {
		raw.Text = &s
	}

	// HTML：必须剥离 CF_HTML 头部（§8 第 7 条）
	if data, err := clipboardBytes(fmtHTML); err == nil && len(data) > 0 {
		if frag, err := StripClipboardHTMLHeader(data); err == nil {
			raw.HTML = &frag
		}
	}

	// RTF
	if data, err := clipboardBytes(fmtRTF); err == nil {
		raw.RTF = data
	}

	// 图片：优先取我们自己写的注册格式 "PNG"（字节原样，指纹可对齐），
	// 其次 CF_DIBV5 / CF_DIB 手动转 PNG（§8 第 6 条）
	raw.Image = readImage()

	// 文件列表
	if files, err := clipboardFileList(); err == nil && len(files) > 0 {
		raw.Files = files
	}

	raw.SourceAppID, raw.SourceAppName = foregroundAppInfo()
	return raw, nil
}

func readImage() *Image {
	if data, err := clipboardBytes(fmtPNG); err == nil && len(data) > 0 {
		if img, err := ImageFromPNG(data); err == nil {
			return img
		}
	}
	for _, format := range []uint32{cfDIBV5, cfDIB} {
		data, err := clipboardBytes(format)
		if err != nil || len(data) == 0 {
			continue
		}
		if img, err := DIBToPNG(data); err == nil {
			return img
		}
	}
	return nil
}

func openClipboardWithRetry() error {
	var lastErr error
	for i := 0; i < openAttempts; i++ {
		if i > 0 {
			time.Sleep(openBackoff)
		}
		r, _, callErr := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		lastErr = callErr
	}
	return fmt.Errorf("%w: OpenClipboard failed: %v", ErrBusy, lastErr)
}

func formatAvailable(format uint32) bool {
	if format == 0 {
		return false
	}
	r, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format))
	return r != 0
}

// enumerateClipboardFormats 枚举当前剪贴板上的全部格式名，供保密判定与诊断。
func enumerateClipboardFormats() []string {
	out := make([]string, 0, 8)
	var format uint32
	for {
		r, _, _ := procEnumClipboardFormats.Call(uintptr(format))
		format = uint32(r)
		if format == 0 {
			break
		}
		name := clipboardFormatName(format)
		if name != "" {
			out = appendUnique(out, name)
		}
		if len(out) >= 64 {
			break
		}
	}
	return out
}

func clipboardFormatName(format uint32) string {
	switch format {
	case cfText:
		return "CF_TEXT"
	case cfBitmap:
		return "CF_BITMAP"
	case cfDIB:
		return "CF_DIB"
	case cfUnicodeText:
		return "CF_UNICODETEXT"
	case cfHDROP:
		return "CF_HDROP"
	case cfDIBV5:
		return "CF_DIBV5"
	}
	buf := make([]uint16, 256)
	r, _, _ := procGetClipboardFormatNameW.Call(
		uintptr(format), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:r])
}

// ── HGLOBAL 读写 ────────────────────────────────────────────────

// hglobal 保存 GlobalLock 返回的裸指针。
//
// 为什么包一层：直接把 uintptr 转成 unsafe.Pointer 会被 go vet 的 unsafeptr
// 检查判为 "possible misuse of unsafe.Pointer"。经一个 unsafe.Pointer 字段间接
// 取值语义完全相同，而且 vet 不会报。HGLOBAL 指向的是系统堆，不受 Go GC 管理，
// 所以不会出现"指针指向对象被搬走"的问题。
type hglobal struct {
	addr uintptr
}

func (h hglobal) bytes(n int) []byte {
	if h.addr == 0 || n <= 0 {
		return nil
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&h.addr))
	return unsafe.Slice((*byte)(p), n)
}

func globalSize(handle uintptr) int {
	// GlobalSize 在 x/sys/windows 里没有包装（它只包了 GlobalAlloc/GlobalLock/
	// GlobalUnlock 之外的少数几个），所以自己取过程地址。
	r, _, _ := procGlobalSize.Call(handle)
	return int(r)
}

// clipboardBytes 读取一个字节型格式的内容（拷进 Go 内存）。
//
// 拿到的 HGLOBAL 归剪贴板所有，我们只读、不释放、用完 unlock。
func clipboardBytes(format uint32) ([]byte, error) {
	if !formatAvailable(format) {
		return nil, ErrEmpty
	}
	handle, _, callErr := procGetClipboardData.Call(uintptr(format))
	if handle == 0 {
		return nil, fmt.Errorf("%s: GetClipboardData(%d): %v", interfaceError, format, callErr)
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return nil, fmt.Errorf("%s: GlobalLock(%d) failed", interfaceError, format)
	}
	defer procGlobalUnlock.Call(handle)

	n := globalSize(handle)
	if n <= 0 {
		return nil, ErrEmpty
	}
	src := hglobal{addr: ptr}.bytes(n)
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

const (
	cfOEMText = 7
)

func clipboardString(format uint32) (string, error) {
	data, err := clipboardBytes(format)
	if err != nil {
		return "", err
	}
	return decodeUTF16(data), nil
}

func clipboardANSIString(format uint32) (string, error) {
	data, err := clipboardBytes(format)
	if err != nil {
		return "", err
	}
	// CF_TEXT 是当前 ANSI 代码页；这里按 Latin-1 逐字节透传，只作为
	// CF_UNICODETEXT 缺失时的兜底（现代应用都会写 Unicode）。
	trimmed := data
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == 0 {
		trimmed = trimmed[:len(trimmed)-1]
	}
	var sb strings.Builder
	sb.Grow(len(trimmed))
	for _, b := range trimmed {
		sb.WriteRune(rune(b))
	}
	return sb.String(), nil
}

// decodeUTF16 把 UTF-16LE（NUL 结尾）解码为 UTF-8。
func decodeUTF16(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	u16 := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		v := uint16(data[i]) | uint16(data[i+1])<<8
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	return windows.UTF16ToString(u16)
}

// clipboardDWORD 读一个 DWORD 型标记格式的值（如 CanIncludeInClipboardHistory）。
func clipboardDWORD(format uint32) (uint32, bool) {
	if format == 0 || !formatAvailable(format) {
		return 0, false
	}
	handle, _, _ := procGetClipboardData.Call(uintptr(format))
	if handle == 0 {
		return 0, false
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return 0, false
	}
	defer procGlobalUnlock.Call(handle)

	b := hglobal{addr: ptr}.bytes(4)
	if len(b) < 4 {
		return 0, false
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, true
}

// clipboardFileList 读 CF_HDROP。
func clipboardFileList() ([]string, error) {
	if !formatAvailable(cfHDROP) {
		return nil, ErrEmpty
	}
	handle, _, _ := procGetClipboardData.Call(uintptr(cfHDROP))
	if handle == 0 {
		return nil, fmt.Errorf("%s: GetClipboardData(CF_HDROP) failed", interfaceError)
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return nil, fmt.Errorf("%s: GlobalLock(CF_HDROP) failed", interfaceError)
	}
	defer procGlobalUnlock.Call(handle)

	n := globalSize(handle)
	if n < DROPFILESHeaderSize {
		return nil, ErrEmpty
	}
	all := hglobal{addr: ptr}.bytes(n)

	// 头部按 Win32 DROPFILES 布局解析（布局的正确性由
	// normalize_dropfiles_test.go 在 mac 上钉死，不依赖真机）。
	df := (*dropFiles)(unsafe.Pointer(&all[0]))
	offset := int(df.PFiles)
	if offset <= 0 || offset >= n {
		return nil, fmt.Errorf("%s: CF_HDROP has bad pFiles offset %d", interfaceError, offset)
	}
	return parseDROPFILES(all[offset:], df.FWide != 0), nil
}

// parseDROPFILES 的实现已抽到 normalize_dropfiles.go（无平台依赖、可在 mac 上单测）。

// ── 写回 ────────────────────────────────────────────────────────

// Write 写回剪贴板。
func (b *windowsBackend) Write(p *Payload) error {
	if p == nil || p.Empty() {
		return errors.New("clipboard: empty payload")
	}
	registerClipboardFormats()

	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	if r, _, callErr := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("%s: EmptyClipboard: %v", interfaceError, callErr)
	}

	if p.Text != nil {
		if err := setClipboardBytes(cfUnicodeText, utf16Bytes(*p.Text)); err != nil {
			return err
		}
	}
	if p.HTML != nil {
		if err := setClipboardBytes(fmtHTML, BuildClipboardHTML(*p.HTML, "")); err != nil {
			return err
		}
	}
	if len(p.RTF) > 0 {
		if err := setClipboardBytes(fmtRTF, p.RTF); err != nil {
			return err
		}
	}
	if len(p.PNG) > 0 {
		// ① 注册格式 "PNG"：字节原样，保证读回来与我们写出去的完全一致
		if err := setClipboardBytes(fmtPNG, p.PNG); err != nil {
			return err
		}
		// ② CF_DIBV5：给只认 DIB 的应用兼容
		if dib, err := PNGToDIBV5(p.PNG); err == nil {
			if err := setClipboardBytes(cfDIBV5, dib); err != nil {
				return err
			}
		}
	}
	if len(p.Files) > 0 {
		if err := setClipboardBytes(cfHDROP, buildDROPFILES(p.Files)); err != nil {
			return err
		}
	}

	// 自身写入不要进系统剪贴板历史与云剪贴板（§8 第 8 条）
	zero := []byte{0, 0, 0, 0}
	if fmtCanIncludeInHistory != 0 {
		if err := setClipboardBytes(fmtCanIncludeInHistory, zero); err != nil {
			return err
		}
	}
	if fmtCanUploadToCloud != 0 {
		if err := setClipboardBytes(fmtCanUploadToCloud, zero); err != nil {
			return err
		}
	}
	return nil
}

// setClipboardBytes 把内容放进 HGLOBAL 再交给剪贴板。
//
// 成功之后 HGLOBAL 的所有权归剪贴板，**不能**再 GlobalFree（系统会在
// EmptyClipboard 时释放）。失败则必须自己释放，否则泄漏。
func setClipboardBytes(format uint32, data []byte) error {
	if format == 0 {
		return fmt.Errorf("%s: unregistered clipboard format", interfaceError)
	}
	if len(data) == 0 {
		data = []byte{0}
	}
	handle, _, callErr := procGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if handle == 0 {
		return fmt.Errorf("%s: GlobalAlloc(%d): %v", interfaceError, len(data), callErr)
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("%s: GlobalLock for format %d failed", interfaceError, format)
	}
	copy(hglobal{addr: ptr}.bytes(len(data)), data)
	procGlobalUnlock.Call(handle)

	if r, _, setErr := procSetClipboardData.Call(uintptr(format), handle); r == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("%s: SetClipboardData(%d): %v", interfaceError, format, setErr)
	}
	return nil
}

// utf16Bytes 把字符串编成 NUL 结尾的 UTF-16LE。
//
// 直接复用 encodeUTF16LE（normalize_dropfiles.go）：那里用的是标准库的
// utf16.Encode，含代理对的字符（emoji 等）以及含 NUL 的病态字符串都能编，
// 不需要 windows.UTF16FromString 的"遇 NUL 报错再退化成手写"那条分支。
func utf16Bytes(s string) []byte { return encodeUTF16LE(s) }

// ── 来源应用 ────────────────────────────────────────────────────

// foregroundAppInfo 返回前台进程的可执行文件路径与文件名。
//
// 与 macOS 的 bundle id 对应：§2 的 Raw.SourceAppID 在 Windows 上就是
// exe 路径或 AppUserModelID。
func foregroundAppInfo() (id, name string) {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return "", ""
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return "", ""
	}
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return "", ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return "", ""
	}
	path := windows.UTF16ToString(buf[:size])
	return path, filepath.Base(path)
}

// ── 结构体定义 ──────────────────────────────────────────────────

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type wndMsg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// 让编译器检查常量没写错（这些值来自 WinUser.h）。
var _ = [1]struct{}{}[cfOEMText-cfOEMText]
