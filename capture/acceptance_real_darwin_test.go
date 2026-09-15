//go:build darwin && cgo

package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zego/pawclip/clipboard"
)

// 验收 4：真机抽查 —— 文本 / 图片 / 文件三类都能落库，图片缩略图正确。
//
// 默认跳过：它会**覆盖当前系统剪贴板**，且需要一台有图形会话的 mac。
// 跑法：
//
//	PAWCLIP_REAL_CLIPBOARD=1 go test -tags sqlite_fts5 -run TestAcceptance4 -v ./capture/
//
// 它走的是真 NSPasteboard：真写回 → 真轮询读回 → 真归一化 → 真落库 + 缩略图。
// 唯一被跳过的是"人去点一下复制"这一步，其余全是产品代码。
func TestAcceptance4_RealClipboardRoundTrip(t *testing.T) {
	if os.Getenv("PAWCLIP_REAL_CLIPBOARD") != "1" {
		t.Skip("真机验收：设 PAWCLIP_REAL_CLIPBOARD=1 再跑（会覆盖系统剪贴板）")
	}

	be, err := clipboard.NewBackend(clipboard.BackendConfig{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	p := newTestPipeline(t, be, DefaultConfig())
	p.start(t)
	t.Cleanup(func() { be.Stop() })

	// ① 文本
	text := "PawClip M1 验收 · 文本 · " + time.Now().Format("15:04:05.000")
	p.writeThenCapture(t, be, &clipboard.Payload{Text: &text})

	// ② 图片 320×200 → 缩略图应压到 160×100
	imgPNG := makeSeededPNG(t, 320, 200, 7)
	p.writeThenCapture(t, be, &clipboard.Payload{PNG: imgPNG})

	// ③ 文件 ×2（只登记路径，不复制内容）
	dir := t.TempDir()
	f1 := filepath.Join(dir, "pawclip-报告.txt")
	f2 := filepath.Join(dir, "pawclip-photo.png")
	for _, f := range []string{f1, f2} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.writeThenCapture(t, be, &clipboard.Payload{Files: []string{f1, f2}})

	p.flush(t)

	items := allItems(t, p.db)
	dumpItems(t, p.db)
	if len(items) == 0 {
		t.Fatal("真机链路一条都没落库")
	}

	// ── ① 文本 ──
	var foundText bool
	for _, it := range items {
		if it.Kind == clipboard.KindText && it.Text == text {
			foundText = true
		}
	}
	if !foundText {
		t.Errorf("没找到落库的文本条目（想要 kind=text text=%q）", text)
	}

	// ── ② 图片 + 缩略图 ──
	var foundImage bool
	for _, it := range items {
		if it.Kind != clipboard.KindImage {
			continue
		}
		foundImage = true
		if !it.ImagePath.Valid {
			t.Fatalf("图片条目 image_path 为空")
		}
		assertShardedPath(t, it.ImagePath.String, "png")
		blob, err := os.ReadFile(p.blobs.Abs(it.ImagePath.String))
		if err != nil {
			t.Fatalf("读 image blob: %v", err)
		}
		if w, h := decodeSize(t, blob); w != 320 || h != 200 {
			t.Errorf("落库的图片 = %dx%d，想要 320x200", w, h)
		}
		if !it.ThumbPath.Valid {
			t.Fatal("图片条目 thumb_path 为空：缩略图没生成")
		}
		thumb, err := os.ReadFile(p.blobs.Abs(it.ThumbPath.String))
		if err != nil {
			t.Fatalf("读缩略图: %v", err)
		}
		tw, th := decodeSize(t, thumb)
		if tw != 160 || th != 100 {
			t.Errorf("缩略图 = %dx%d，想要 160x100", tw, th)
		}
		t.Logf("图片条目：image_path=%s (320x200)，thumb_path=%s (%dx%d)",
			it.ImagePath.String, it.ThumbPath.String, tw, th)
	}
	if !foundImage {
		t.Error("没找到落库的图片条目")
	}

	// ── ③ 文件 ──
	var foundFiles bool
	for _, it := range items {
		files := it.files(t)
		if len(files) == 0 {
			continue
		}
		if containsAll(files, f1, f2) {
			foundFiles = true
			if it.Kind != clipboard.KindFiles && it.Kind != clipboard.KindMixed {
				t.Errorf("文件条目 kind = %q，想要 files 或 mixed", it.Kind)
			}
			t.Logf("文件条目：kind=%s preview=%q file_paths=%s", it.Kind, it.Preview, it.FilePaths.String)
		}
	}
	if !foundFiles {
		t.Errorf("没找到携带 %s 与 %s 的条目", f1, f2)
	}

	st := p.cap.Stats()
	t.Logf("真机链路统计：ticks=%d reads=%d accepted=%d drops=%v；守卫拦下自写入 %d 次",
		st.Ticks, st.Reads, st.Accepted, st.Drops, p.cap.Guard().Dropped())
}

// writeThenCapture 模拟"用户复制了这份内容"：先让上层 Armed 守卫再写回
// （就像 M2 里点历史条目回贴那样），等守卫窗口过期，再补一个信号强制读一次。
//
// 它每次都断言"写回 → 读回"的指纹完全一致。这是本文件里最值得守住的断言：
// SelfWriteGuard 完全建立在"同一份内容，写进去与读出来得到同一个指纹"之上。
// 一旦某个平台开始对回写内容做归一化（例如把 PNG 重编码、给文本补一个 RTF
// 孪生体），守卫就会静默失效，表现为验收 1 在真机上开始漏——而假后端永远
// 测不出这件事，因为假后端的"读"就是"写"的镜像。
func (p *pipeline) writeThenCapture(t *testing.T, be clipboard.Backend, payload *clipboard.Payload) {
	t.Helper()

	before := p.cap.Stats().Processed
	p.cap.Guard().Arm(payload.Fingerprint())
	if err := be.Write(payload); err != nil {
		t.Fatalf("写回剪贴板失败: %v", err)
	}

	// 立刻读一次，验证"写进去的"和"读出来的"是同一份内容。
	// 不依赖轮询时序，因此这一步是确定性的。
	raw, err := be.Read()
	if err != nil {
		t.Fatalf("写回后立刻读取失败: %v", err)
	}
	want, got := payload.Fingerprint(), raw.Fingerprint()
	if got != want {
		t.Errorf("写回→读回指纹不一致：\n  payload = %s\n  raw     = %s\n"+
			"（不一致会让 SelfWriteGuard 失效，也就是验收 1 在真机上会漏）", want, got)
	} else {
		t.Logf("写回→读回指纹一致：%s", want)
	}

	// 等自写入守卫窗口（2s）自然过期。这段等待期间轮询若先读到，会被守卫
	// 拦下——顺带在真剪贴板上复现了一次验收 1。
	time.Sleep(2200 * time.Millisecond)

	// 剪贴板内容没有再变，轮询不会产生新信号；手动补一个强制再读一次。
	select {
	case p.cap.ticks <- clipboard.Tick{AtMs: time.Now().UnixMilli()}:
	case <-time.After(2 * time.Second):
		t.Fatal("无法推送变更信号")
	}
	// 等"这一跳完整处理完"（Processed），不是"读到了"（Reads）——用 Reads 的话
	// 可能在 filter/Enqueue 之前就返回，调用方随后的 flush 会把这条漏掉。
	p.waitProcessed(t, before+1)
}

func containsAll(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
