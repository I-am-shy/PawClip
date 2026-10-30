//go:build windows

package clipboard

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCF_HDROP_RealClipboard 是 CF_HDROP 的 Windows 真机往返验收。
//
// 默认跳过：它会**覆盖当前系统剪贴板**。跑法：
//
//	PAWCLIP_REAL_CLIPBOARD=1 go test -run TestCF_HDROP_RealClipboard -v ./clipboard/
//
// 它证明两件纯单测证明不了的事：
//
//  1. **读**：把一份按 SDK 布局、显式偏移写出的 CF_HDROP（模拟 Explorer）
//     放进真剪贴板，产品代码读出来的路径必须与写入的逐字相等。2026-10-31
//     的 bug 就是在这一步暴露的（16 字节头 → 每个字符一个"文件"）。
//  2. **写**：用产品代码 Write 一份文件列表，再 Read 回来，路径必须一致；
//     同时按偏移复核头部是 20 字节 / fNC@12=0 / fWide@16=1。
//
// 它不依赖 Wails 或打包产物，因此在没有 64 位工具链的机器上也能跑。
func TestCF_HDROP_RealClipboard(t *testing.T) {
	if os.Getenv("PAWCLIP_REAL_CLIPBOARD") != "1" {
		t.Skip("真机验收：设 PAWCLIP_REAL_CLIPBOARD=1 再跑（会覆盖系统剪贴板）")
	}

	dir := t.TempDir()
	want := []string{
		filepath.Join(dir, "报告 一.txt"),
		filepath.Join(dir, "photo 🐾.png"),
	}
	for _, p := range want {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	be, err := NewBackend(BackendConfig{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	// ① 外部字节 → 产品代码读。
	writeRawCFHDROP(t, explorerDROPFILES(t, want))
	raw, err := be.Read()
	if err != nil {
		t.Fatalf("读剪贴板: %v", err)
	}
	if !reflect.DeepEqual(raw.Files, want) {
		t.Fatalf("解析文件列表 = %#v，想要 %#v（长度 %d vs %d）",
			raw.Files, want, len(raw.Files), len(want))
	}

	// ② 产品代码写 → 产品代码读。
	if err := be.Write(&Payload{Files: want}); err != nil {
		t.Fatalf("写回文件列表: %v", err)
	}
	raw2, err := be.Read()
	if err != nil {
		t.Fatalf("写回后再读: %v", err)
	}
	if !reflect.DeepEqual(raw2.Files, want) {
		t.Fatalf("写回→读回 = %#v，想要 %#v", raw2.Files, want)
	}

	// ③ 写出去的头部必须是 SDK 布局（显式偏移复核，不只看结构体）。
	blob := buildDROPFILES(want)
	if len(blob) < DROPFILESHeaderSize {
		t.Fatalf("buildDROPFILES 只有 %d 字节", len(blob))
	}
	if got := binary.LittleEndian.Uint32(blob[0:4]); got != 20 {
		t.Errorf("pFiles = %d，想要 20", got)
	}
	if got := binary.LittleEndian.Uint32(blob[12:16]); got != 0 {
		t.Errorf("fNC = %d，想要 0", got)
	}
	if got := binary.LittleEndian.Uint32(blob[16:20]); got != 1 {
		t.Errorf("fWide = %d，想要 1", got)
	}
}

// writeRawCFHDROP 把一份已经组装好的 CF_HDROP 字节放进系统剪贴板。
//
// 不走 buildDROPFILES：这是"外部程序（Explorer）写出来的那份字节"的替身。
func writeRawCFHDROP(t *testing.T, blob []byte) {
	t.Helper()

	if err := openClipboardWithRetry(); err != nil {
		t.Fatalf("OpenClipboard: %v", err)
	}
	defer procCloseClipboard.Call()

	if r, _, callErr := procEmptyClipboard.Call(); r == 0 {
		t.Fatalf("EmptyClipboard: %v", callErr)
	}
	if err := setClipboardBytes(cfHDROP, blob); err != nil {
		t.Fatalf("SetClipboardData(CF_HDROP): %v", err)
	}
}
