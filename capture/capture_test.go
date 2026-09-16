package capture

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zego/pawclip/clipboard"
)

func (p *pipeline) start(t *testing.T) {
	t.Helper()
	if err := p.cap.Start(context.Background()); err != nil {
		t.Fatalf("capture.Start: %v", err)
	}
}

// ── 构造与生命周期 ──────────────────────────────────────────────

func TestNew_RejectsNil(t *testing.T) {
	if _, err := New(nil, nil, DefaultConfig(), nil); err == nil {
		t.Fatal("nil backend 应当报错")
	}
	p := newTestPipeline(t, newFakeBackend(), DefaultConfig())
	if _, err := New(p.cap.Backend(), nil, DefaultConfig(), nil); err == nil {
		t.Fatal("nil writer 应当报错")
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.TextMaxChars != clipboard.TextMaxCharsDefault {
		t.Fatalf("TextMaxChars = %d，想要 %d", got.TextMaxChars, clipboard.TextMaxCharsDefault)
	}
	if got.TickBuffer != 8 {
		t.Fatalf("TickBuffer = %d，想要 8", got.TickBuffer)
	}
	// 显式值不被覆盖
	custom := Config{TextMaxChars: 32, TickBuffer: 2}.withDefaults()
	if custom.TextMaxChars != 32 || custom.TickBuffer != 2 {
		t.Fatalf("显式配置被覆盖了：%+v", custom)
	}
}

func TestStart_TwiceAndStopIdempotent(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	if err := p.cap.Start(context.Background()); err == nil {
		t.Fatal("重复 Start 应当报错")
	}
	p.cap.Stop()
	p.cap.Stop() // 幂等
}

func TestStop_WithoutStart(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	// 未 Start 就 Stop 不应死等
	done := make(chan struct{})
	go func() { p.cap.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未启动时 Stop 阻塞了")
	}
}

func TestStart_PropagatesBackendError(t *testing.T) {
	be := &failingStartBackend{}
	p := newTestPipeline(t, be, DefaultConfig())
	if err := p.cap.Start(context.Background()); err == nil {
		t.Fatal("后端 Start 失败时应当向上返回错误")
	}
	p.cap.Stop() // 不应死等
}

type failingStartBackend struct{ fakeBackend }

func (b *failingStartBackend) Start(chan<- clipboard.Tick) error {
	return errors.New("boom")
}

// ── 各类内容的落库 ──────────────────────────────────────────────

func TestCapture_TextPersisted(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	text := "PawClip 文本捕获 🙂"
	b.setRaw(&clipboard.Raw{Text: &text, RawTypes: []string{"public.utf8-plain-text"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 1 {
		t.Fatalf("CountAlive = %d，想要 1", n)
	}
	it, err := p.db.GetByFingerprint(context.Background(),
		clipboard.Fingerprint(clipboard.Content{Text: &text}))
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if it.Kind != clipboard.KindText {
		t.Errorf("Kind = %q，想要 text", it.Kind)
	}
	if it.TextContent == nil || *it.TextContent != text {
		t.Errorf("TextContent = %v，想要 %q", it.TextContent, text)
	}
	if it.Preview != text {
		t.Errorf("Preview = %q，想要 %q", it.Preview, text)
	}
	if it.UseCount != 1 {
		t.Errorf("UseCount = %d，想要 1", it.UseCount)
	}
}

func TestCapture_BrowserCopyTextPlusHTML_AcceptedUnderDefaultTypes(t *testing.T) {
	// 回归用例：浏览器复制一定是 text + html 两个 flavor。
	// 若 filter 按 items.kind 判定，"html" 不在默认 capture.types 里，
	// 绝大多数网页复制会被整体丢掉。
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	text := "hello"
	html := "<p>hello</p>"
	b.setRaw(&clipboard.Raw{
		Text: &text, HTML: &html,
		RawTypes: []string{"public.utf8-plain-text", "public.html"},
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 1 {
		t.Fatalf("文本+HTML 被丢弃了：CountAlive = %d", n)
	}
	items := allItems(t, p.db)
	if items[0].Kind != clipboard.KindHTML {
		t.Errorf("Kind = %q，想要 html（文本组内取信息量最高的那个）", items[0].Kind)
	}
	if items[0].Text != text || items[0].HTML != html {
		t.Errorf("两个 flavor 都该保留：text=%q html=%q", items[0].Text, items[0].HTML)
	}
}

func TestCapture_ImageBlobAndThumbnail(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	pngData := makePNG(t, 320, 200)
	b.setRaw(&clipboard.Raw{
		Image:    mustImage(t, pngData),
		RawTypes: []string{"public.png"},
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	items := allItems(t, p.db)
	if len(items) != 1 {
		t.Fatalf("items = %d，想要 1", len(items))
	}
	it := items[0]
	if it.Kind != clipboard.KindImage {
		t.Fatalf("Kind = %q，想要 image", it.Kind)
	}
	if it.Preview != "图片 320 × 200" {
		t.Errorf("Preview = %q，想要 %q", it.Preview, "图片 320 × 200")
	}

	// 原图：内容寻址路径必须是 两级分片 + sha256.png，且字节与输入一致
	if !it.ImagePath.Valid {
		t.Fatal("image_path 为空")
	}
	assertShardedPath(t, it.ImagePath.String, "png")
	blobBytes, err := os.ReadFile(p.blobs.Abs(it.ImagePath.String))
	if err != nil {
		t.Fatalf("读 image blob: %v", err)
	}
	if string(blobBytes) != string(pngData) {
		t.Error("原图字节被改写了；指纹与自写入守卫都依赖原样字节")
	}
	if w, h := decodeSize(t, blobBytes); w != 320 || h != 200 {
		t.Errorf("原图尺寸 = %dx%d，想要 320x200", w, h)
	}

	// 缩略图：长边压到 ≤160
	if !it.ThumbPath.Valid {
		t.Fatal("thumb_path 为空")
	}
	thumbBytes, err := os.ReadFile(p.blobs.Abs(it.ThumbPath.String))
	if err != nil {
		t.Fatalf("读缩略图: %v", err)
	}
	tw, th := decodeSize(t, thumbBytes)
	if tw != 160 || th != 100 {
		t.Errorf("缩略图尺寸 = %dx%d，想要 160x100（长边 ≤ %d）", tw, th, 160)
	}
	if tw > 160 || th > 160 {
		t.Errorf("缩略图长边 %d/%d 超过 160", tw, th)
	}
}

func TestCapture_RTFBlob(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	rtf := []byte(`{\rtf1\ansi\deff0 Hello \b bold\b0}`)
	b.setRaw(&clipboard.Raw{RTF: rtf, RawTypes: []string{"public.rtf"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	items := allItems(t, p.db)
	if len(items) != 1 {
		t.Fatalf("items = %d，想要 1", len(items))
	}
	if items[0].Kind != clipboard.KindRTF {
		t.Errorf("Kind = %q，想要 rtf", items[0].Kind)
	}
	it, err := p.db.GetByFingerprint(context.Background(),
		clipboard.Fingerprint(clipboard.Content{RTF: rtf}))
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if it.RTFPath == nil {
		t.Fatal("rtf_path 为空")
	}
	assertShardedPath(t, *it.RTFPath, "rtf")
	got, err := os.ReadFile(p.blobs.Abs(*it.RTFPath))
	if err != nil {
		t.Fatalf("读 rtf blob: %v", err)
	}
	if string(got) != string(rtf) {
		t.Error("rtf 字节不一致")
	}
}

func TestCapture_FilesPersisted(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	dir := t.TempDir()
	f1 := filepath.Join(dir, "report.txt")
	f2 := filepath.Join(dir, "photo.png")
	for _, f := range []string{f1, f2} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b.setRaw(&clipboard.Raw{
		Files:    []string{f1, f2},
		RawTypes: []string{"NSFilenamesPboardType"},
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	items := allItems(t, p.db)
	if len(items) != 1 {
		t.Fatalf("items = %d，想要 1", len(items))
	}
	if items[0].Kind != clipboard.KindFiles {
		t.Errorf("Kind = %q，想要 files", items[0].Kind)
	}
	if items[0].Preview != "report.txt 等 2 个文件" {
		t.Errorf("Preview = %q", items[0].Preview)
	}
	files := items[0].files(t)
	if len(files) != 2 || files[0] != f1 || files[1] != f2 {
		t.Errorf("file_paths = %v，想要 %v", files, []string{f1, f2})
	}
	// 这一条很关键：只记路径，不复制文件内容
	if items[0].ByteSize != int64(len(f1)+len(f2)) {
		t.Errorf("byte_size = %d，想要 %d（仅登记路径长度）", items[0].ByteSize, len(f1)+len(f2))
	}
}

func TestCapture_MixedTextAndImage(t *testing.T) {
	// 文本组 + 图片组才是真正的 mixed（html+text 不算，见 Kind()）
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	text := "截图说明"
	pngData := makePNG(t, 40, 30)
	b.setRaw(&clipboard.Raw{
		Text:     &text,
		Image:    mustImage(t, pngData),
		RawTypes: []string{"public.utf8-plain-text", "public.png"},
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	items := allItems(t, p.db)
	if len(items) != 1 {
		t.Fatalf("items = %d，想要 1", len(items))
	}
	if items[0].Kind != clipboard.KindMixed {
		t.Errorf("Kind = %q，想要 mixed", items[0].Kind)
	}
	if items[0].Text != text {
		t.Errorf("mixed 的摘要应优先取纯文本：text=%q preview=%q", items[0].Text, items[0].Preview)
	}
}

// ── 过滤与错误分桶 ──────────────────────────────────────────────

func TestCapture_EmptyDropped(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	// Read 返回 ErrEmpty（Windows 侧"没有任何认识的格式"就是这样）
	b.setReadErr(clipboard.ErrEmpty)
	b.push()
	p.waitEmpty(t, 1)

	st := p.cap.Stats()
	if st.Empty != 1 {
		t.Fatalf("Empty = %d，想要 1（stats=%+v）", st.Empty, st)
	}
	if n := p.countAlive(t); n != 0 {
		t.Fatalf("CountAlive = %d，想要 0", n)
	}
}

func TestCapture_ReadErrorBuckets(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		check func(Stats) int64
	}{
		{"busy", clipboard.ErrBusy, func(s Stats) int64 { return s.Busy }},
		{"flapping", clipboard.ErrFlapping, func(s Stats) int64 { return s.Flapping }},
		{"other", errors.New("disk on fire"), func(s Stats) int64 { return s.ReadErrs }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFakeBackend()
			p := newTestPipeline(t, b, DefaultConfig())
			p.start(t)

			b.setReadErr(tc.err)
			b.push()
			// 等的是"分桶计数"，不是 Ticks——Ticks 在读之前就加了，
			// 用它当同步点会读到一个还没归桶的中间态。
			p.waitStats(t, tc.name, func(s Stats) bool { return tc.check(s) >= 1 })

			if got := tc.check(p.cap.Stats()); got != 1 {
				t.Fatalf("计数 = %d，想要 1；stats=%+v", got, p.cap.Stats())
			}
			// 读失败不该让流水线停摆：恢复后仍能捕获
			b.setReadErr(nil)
			text := "after failure"
			b.setRaw(&clipboard.Raw{Text: &text, RawTypes: []string{"public.utf8-plain-text"}})
			b.push()
			p.waitProcessed(t, 1)
			p.flush(t)
			if n := p.countAlive(t); n != 1 {
				t.Fatalf("恢复后 CountAlive = %d，想要 1", n)
			}
		})
	}
}

func TestCapture_TypeDisabledDropped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Filter.Types = []string{clipboard.GroupImage} // 只收图片
	b := newFakeBackend()
	p := newTestPipeline(t, b, cfg)
	p.start(t)

	text := "should be dropped"
	b.setRaw(&clipboard.Raw{Text: &text, RawTypes: []string{"public.utf8-plain-text"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 0 {
		t.Fatalf("CountAlive = %d，想要 0", n)
	}
	if got := p.cap.Stats().Drops["drop:type_disabled"]; got != 1 {
		t.Errorf("drop:type_disabled = %d，想要 1", got)
	}
}

func TestCapture_PrivateTypeDropped(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	secret := "hunter2"
	b.setRaw(&clipboard.Raw{
		Text:     &secret,
		RawTypes: []string{"public.utf8-plain-text", "org.nspasteboard.ConcealedType"},
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 0 {
		t.Fatalf("保密标记内容被记下来了：CountAlive = %d", n)
	}
	if got := p.cap.Stats().Drops["drop:private_type"]; got != 1 {
		t.Errorf("drop:private_type = %d，想要 1", got)
	}
}

func TestCapture_AppExcluded(t *testing.T) {
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig()) // 默认黑名单含 com.1password.*
	p.start(t)

	pw := "correct horse battery staple"
	b.setRaw(&clipboard.Raw{
		Text:        &pw,
		RawTypes:    []string{"public.utf8-plain-text"},
		SourceAppID: "com.1password.Pro",
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 0 {
		t.Fatalf("黑名单应用的内容被记下来了：CountAlive = %d", n)
	}
	if got := p.cap.Stats().Drops["drop:app_excluded"]; got != 1 {
		t.Errorf("drop:app_excluded = %d，想要 1", got)
	}
}

func TestCapture_ExcludedAppAppliesToDisplayNameToo(t *testing.T) {
	// Windows 侧常常只有 exe 路径而没有 bundle id，两侧都要参与匹配
	b := newFakeBackend()
	p := newTestPipeline(t, b, DefaultConfig())
	p.start(t)

	text := "x"
	b.setRaw(&clipboard.Raw{
		Text:          &text,
		RawTypes:      []string{"public.utf8-plain-text"},
		SourceAppName: "1Password",
	})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	// 默认黑名单是 com.1password.*，显示名 "1Password" 不匹配 → 应放行。
	// 这条用例锁的是"匹配的是模式而不是子串"这个行为。
	if n := p.countAlive(t); n != 1 {
		t.Fatalf("CountAlive = %d，想要 1（显示名不该被子串误伤）", n)
	}
}

func TestCapture_CaptureDisabledDrops(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Filter.Enabled = false
	b := newFakeBackend()
	p := newTestPipeline(t, b, cfg)
	p.start(t)

	text := "paused"
	b.setRaw(&clipboard.Raw{Text: &text, RawTypes: []string{"public.utf8-plain-text"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	if n := p.countAlive(t); n != 0 {
		t.Fatalf("总开关关闭时仍落库：CountAlive = %d", n)
	}
	if got := p.cap.Stats().Drops["drop:capture_disabled"]; got != 1 {
		t.Errorf("drop:capture_disabled = %d，想要 1", got)
	}
}

// ── 截断 vs 完整指纹 ────────────────────────────────────────────

func TestCapture_TruncatesStoredButFingerprintsFull(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TextMaxChars = 16
	b := newFakeBackend()
	p := newTestPipeline(t, b, cfg)
	p.start(t)

	full := strings.Repeat("喵", 100) // 100 个字符
	b.setRaw(&clipboard.Raw{Text: &full, RawTypes: []string{"public.utf8-plain-text"}})
	b.push()
	p.waitProcessed(t, 1)
	p.flush(t)

	// 指纹必须是对**完整内容**算的，否则再去查库就查不到
	it, err := p.db.GetByFingerprint(context.Background(),
		clipboard.Fingerprint(clipboard.Content{Text: &full}))
	if err != nil {
		t.Fatalf("按完整内容指纹查不到：(docs/HANDOFF-PROMPT.md §4 第 4 条被破坏) %v", err)
	}
	if it.TextContent == nil {
		t.Fatal("TextContent 为空")
	}
	if got := []rune(*it.TextContent); len(got) != 16 {
		t.Errorf("落库文本长度 = %d 个字符，想要 16", len(got))
	}
	// 截断按字符而不是字节，不能切出半个 UTF-8 序列
	if !strings.HasPrefix(full, *it.TextContent) {
		t.Error("截断结果不是完整内容的前缀（可能按字节切断了 UTF-8）")
	}
}

// ── 小工具 ──────────────────────────────────────────────────────

func mustImage(t *testing.T, pngData []byte) *clipboard.Image {
	t.Helper()
	img, err := clipboard.ImageFromPNG(pngData)
	if err != nil {
		t.Fatalf("ImageFromPNG: %v", err)
	}
	return img
}

func decodeSize(t *testing.T, pngData []byte) (int, int) {
	t.Helper()
	cfg, err := png.DecodeConfig(strings.NewReader(string(pngData)))
	if err != nil {
		t.Fatalf("decode PNG config: %v", err)
	}
	return cfg.Width, cfg.Height
}

// assertShardedPath 检查 blob 相对路径符合 §5.3 的 两级分片布局。
func assertShardedPath(t *testing.T, rel, ext string) {
	t.Helper()
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		t.Fatalf("blob 路径 %q 不是两级分片（想要 aa/bb/<sha>.<ext>）", rel)
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		t.Fatalf("blob 路径 %q 的分片目录长度不对", rel)
	}
	if !strings.HasSuffix(rel, "."+ext) {
		t.Fatalf("blob 路径 %q 扩展名不是 .%s", rel, ext)
	}
	name := parts[2]
	if len(name) != 64+1+len(ext) {
		t.Fatalf("blob 文件名 %q 不是 64 位 sha256 + 扩展名", name)
	}
	if !strings.HasPrefix(name, parts[0]+parts[1]) {
		t.Fatalf("blob 路径 %q 的两个分片目录不是 sha256 的前 2/第 3-4 位", rel)
	}
}
