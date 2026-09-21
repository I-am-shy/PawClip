package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zego/pawclip/store"
)

// ── 测试脚手架 ──────────────────────────────────────────────────

type harness struct {
	t      *testing.T
	dir    string
	db     *store.DB
	blobs  *store.BlobStore
	catID  map[string]int64 // 分类名 → id
	tagID  map[string]int64 // 标签名 → id
	outDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// t.TempDir 会自动清理；不要用 /tmp 常量路径，否则并行跑会互相踩。
	dir := t.TempDir()
	db, err := store.Open(store.Options{
		Path:   filepath.Join(dir, "pawclip.db"),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := store.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("建 blob 目录: %v", err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatalf("建导出目录: %v", err)
	}
	return &harness{t: t, dir: dir, db: db, blobs: blobs, outDir: out}
}

func (h *harness) ctx() context.Context { return context.Background() }

// putCat 建一个分类并返回 id。
func (h *harness) putCat(name, color string, rule string) int64 {
	h.t.Helper()
	var ruleArg any
	if rule != "" {
		ruleArg = rule
	}
	res, err := h.db.Writer().ExecContext(h.ctx(),
		`INSERT INTO categories (name, color, rule, ttl_seconds, sort_order, created_at)
		 VALUES (?, ?, ?, NULL, 0, 1)`, name, color, ruleArg)
	if err != nil {
		h.t.Fatalf("建分类 %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	if h.catID == nil {
		h.catID = map[string]int64{}
	}
	h.catID[name] = id
	return id
}

func (h *harness) putTag(name, color string) int64 {
	h.t.Helper()
	res, err := h.db.Writer().ExecContext(h.ctx(),
		`INSERT INTO tags (name, color) VALUES (?, ?)`, name, color)
	if err != nil {
		h.t.Fatalf("建标签 %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	if h.tagID == nil {
		h.tagID = map[string]int64{}
	}
	h.tagID[name] = id
	return id
}

// seedItem 直接写一行 items。刻意不走 store 的 upsert 路径：
// 那样会由存储层决定 use_count / first_seen_at，而本文件要断言的是
// "导出再导入之后**每一个字段**都还在"，所以种子数据必须完全可控。
type seedItem struct {
	kind       string
	text       *string
	html       *string
	rtfRel     *string
	imageRel   *string
	thumbRel   *string
	filePaths  []string
	preview    string
	fp         string
	byteSize   int64
	appID      string
	appName    string
	url        *string
	catID      *int64
	pinned     bool
	firstSeen  int64
	expiresAt  *int64
	ttlSource  *string
	createdAt  int64
	lastUsedAt *int64
	useCount   int64
	tagIDs     []int64
}

func (h *harness) seed(it seedItem) int64 {
	h.t.Helper()
	var files any
	if len(it.filePaths) > 0 {
		b, err := json.Marshal(it.filePaths)
		if err != nil {
			h.t.Fatal(err)
		}
		files = string(b)
	}
	nullable := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	nullableI := func(v *int64) any {
		if v == nil {
			return nil
		}
		return *v
	}

	var id int64
	err := h.db.Writer().QueryRowContext(h.ctx(), `
INSERT INTO items (
  kind, text_content, html_content, rtf_path, image_path, thumb_path, file_paths,
  preview, fingerprint, byte_size, source_app_id, source_app_name, source_url,
  category_id, pinned, first_seen_at, expires_at, ttl_source, created_at,
  last_used_at, use_count
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
RETURNING id`,
		it.kind, nullable(it.text), nullable(it.html), nullable(it.rtfRel),
		nullable(it.imageRel), nullable(it.thumbRel), files,
		it.preview, it.fp, it.byteSize, it.appID, it.appName, nullable(it.url),
		nullableI(it.catID), boolToInt(it.pinned), it.firstSeen, nullableI(it.expiresAt),
		nullable(it.ttlSource), it.createdAt, nullableI(it.lastUsedAt), it.useCount,
	).Scan(&id)
	if err != nil {
		h.t.Fatalf("写入条目 %s: %v", it.preview, err)
	}
	for _, tagID := range it.tagIDs {
		if _, err := h.db.Writer().ExecContext(h.ctx(),
			`INSERT INTO item_tags (item_id, tag_id) VALUES (?, ?)`, id, tagID); err != nil {
			h.t.Fatalf("挂标签: %v", err)
		}
	}
	return id
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// makePNGBlob 生成一张纯色 PNG 并落进 blob store，返回相对路径与 sha。
func (h *harness) makePNGBlob(w, hgt int, c color.Color) (rel, sha string) {
	h.t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, hgt))
	for y := 0; y < hgt; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		h.t.Fatal(err)
	}
	rel, sha, err := h.blobs.Put(buf.Bytes(), "png")
	if err != nil {
		h.t.Fatalf("写 blob: %v", err)
	}
	return rel, sha
}

func (h *harness) makeRTFBlob(body string) (rel, sha string) {
	h.t.Helper()
	data := []byte(`{\rtf1\ansi ` + body + `}`)
	rel, sha, err := h.blobs.Put(data, "rtf")
	if err != nil {
		h.t.Fatalf("写 rtf blob: %v", err)
	}
	return rel, sha
}

// seedAll 造一份覆盖全部字段形态的语料，返回各条目的指纹便于断言。
//
// 覆盖点刻意挑"导出/导入最容易丢"的那些：
//   - 三种 blob（图片 / RTF）与缩略图
//   - HTML + 纯文本并存
//   - 文件列表（多路径）
//   - 分类 + 多个标签 + 置顶 + 绝对过期时间 + use_count > 1
//   - 从没被使用过（last_used_at = NULL）
func (h *harness) seedAll() (textFP, browserFP, imageFP, rtfFP, filesFP, pinnedFP string) {
	h.t.Helper()
	workCat := h.putCat("工作", "#4A90D9", `{"match":"any","conditions":[{"field":"sourceAppId","op":"startsWith","value":"com.apple.Safari"}]}`)
	_ = h.putCat("生活", "#E67E22", "")
	tagVip := h.putTag("重要", "#D0342C")
	tagTodo := h.putTag("待办", "#27AE60")

	// 1) 纯文本
	textFP = fpOf("纯文本条目")
	h.seed(seedItem{
		kind: "text", text: strPtr("纯文本条目：Hello 世界"), preview: "纯文本条目：Hello 世界",
		fp: textFP, byteSize: 40, appID: "com.apple.Terminal", appName: "Terminal",
		firstSeen: 1_700_000_000, createdAt: 1_700_000_000, lastUsedAt: i64Ptr(1_700_000_500),
		useCount: 7, catID: &workCat, tagIDs: []int64{tagVip},
	})

	// 2) 浏览器：text + html 并存
	browserFP = fpOf("浏览器条目")
	h.seed(seedItem{
		kind: "text", text: strPtr("浏览器条目纯文本"), html: strPtr(`<b>浏览器条目</b><a href="https://x.test">链接</a>`),
		preview: "浏览器条目纯文本", fp: browserFP, byteSize: 120,
		appID: "com.apple.Safari", appName: "Safari", url: strPtr("https://x.test/page"),
		firstSeen: 1_700_000_100, createdAt: 1_700_000_100, useCount: 2,
		catID: &workCat, tagIDs: []int64{tagVip, tagTodo},
	})

	// 3) 图片：真 PNG + 缩略图，且 last_used_at 为 NULL（从没被用过）
	imgRel, _ := h.makePNGBlob(64, 48, color.RGBA{R: 12, G: 200, B: 90, A: 255})
	thumbRel, _, _, err := h.blobs.PutThumb(mustRead(h.t, h.blobs.Abs(imgRel)), shaOfRel(imgRel))
	if err != nil {
		h.t.Fatalf("缩略图: %v", err)
	}
	imageFP = fpOf("图片条目")
	h.seed(seedItem{
		kind: "image", text: strPtr("剪贴板图片 64×48"), preview: "剪贴板图片",
		fp: imageFP, byteSize: 4096, appID: "com.apple.Preview", appName: "Preview",
		imageRel: &imgRel, thumbRel: &thumbRel,
		firstSeen: 1_700_000_200, createdAt: 1_700_000_200, useCount: 1,
	})

	// 4) RTF
	rtfRel, _ := h.makeRTFBlob(`\b 加粗\b0  与 \i 斜体\i0`)
	rtfFP = fpOf("富文本条目")
	h.seed(seedItem{
		kind: "rtf", text: strPtr("富文本条目"), rtfRel: &rtfRel, preview: "富文本条目",
		fp: rtfFP, byteSize: 512, appID: "com.apple.TextEdit", appName: "TextEdit",
		firstSeen: 1_700_000_300, createdAt: 1_700_000_300, useCount: 3,
	})

	// 5) 文件列表（多路径，含空格与中文）
	filesFP = fpOf("文件条目")
	h.seed(seedItem{
		kind: "files", filePaths: []string{"/Users/me/文稿/报告 终稿.pdf", "/Users/me/a b.txt"},
		preview: "报告 终稿.pdf 等 2 个文件", fp: filesFP, byteSize: 1024,
		appID: "com.apple.finder", appName: "Finder",
		firstSeen: 1_700_000_400, createdAt: 1_700_000_400, useCount: 1,
	})

	// 6) 置顶 + 分类 + 标签（pinned 时 expiresAt 必须为 NULL）
	pinnedFP = fpOf("置顶条目")
	lifeCat := h.catID["生活"]
	h.seed(seedItem{
		kind: "text", text: strPtr("置顶条目内容"), preview: "置顶条目内容",
		fp: pinnedFP, byteSize: 20, appID: "com.apple.Notes", appName: "Notes",
		catID: &lifeCat, pinned: true,
		firstSeen: 1_700_000_500, createdAt: 1_700_000_500, useCount: 5,
		tagIDs: []int64{tagTodo},
	})

	// 7) 带绝对过期时间的普通条目
	exp := int64(1_800_000_000)
	h.seed(seedItem{
		kind: "text", text: strPtr("会过期的条目"), preview: "会过期的条目",
		fp: fpOf("过期条目"), byteSize: 18,
		appID: "com.apple.Terminal", appName: "Terminal",
		firstSeen: 1_700_000_600, createdAt: 1_700_000_600, useCount: 1,
		expiresAt: &exp, ttlSource: strPtr("item"),
	})

	return
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return b
}

func shaOfRel(rel string) string {
	b := filepath.Base(rel)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// fpOf 造一个确定性的 sha256 形态指纹（内容无意义，只要合法且互不相同）。
func fpOf(s string) string {
	sum := sha256Hex([]byte(s))
	return "sha256:" + sum
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── 导出：包结构 ────────────────────────────────────────────────

// TestExport_PackageStructureAndOrder 校验 §2 的条目顺序。
//
// 顺序不是审美问题：README.txt 必须最先（用户双击解包时先看到说明），
// manifest 必须最后（它的 stats 要等全部处理完才知道）。
// 顺序错了的后果是"stats 里条目数永远偏少"这种难查的账不平。
func TestExport_PackageStructureAndOrder(t *testing.T) {
	h := newHarness(t)
	h.seedAll()

	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		ManifestFormat: "json", OutputDir: h.outDir, AppVersion: "9.9.9",
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Items != 7 {
		t.Errorf("导出条目数 = %d，想要 7", res.Items)
	}
	if res.Categories != 2 || res.Tags != 2 {
		t.Errorf("分类/标签 = %d/%d，想要 2/2", res.Categories, res.Tags)
	}
	if res.MissingBlobs != 0 {
		t.Errorf("MissingBlobs = %d，想要 0（种子里所有 blob 都在盘上）", res.MissingBlobs)
	}

	zr, err := zip.OpenReader(res.Path)
	if err != nil {
		t.Fatalf("打开导出的包: %v", err)
	}
	defer func() { _ = zr.Close() }()

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		t.Fatal("空包")
	}
	if names[0] != ReadmeName {
		t.Errorf("第一个条目是 %q，想要 %q（README 必须最先）", names[0], ReadmeName)
	}
	last := names[len(names)-1]
	if last != ManifestJSON && last != ManifestYAML {
		t.Errorf("最后一个条目是 %q，想要清单文件（清单必须最后）", last)
	}

	// blobs/ 必须都在 README 之后、manifest 之前。
	var blobCount, manifestAt int
	for i, n := range names {
		if strings.HasPrefix(n, BlobDirPrefix) {
			blobCount++
			if i == 0 || i == len(names)-1 {
				t.Errorf("blob %q 出现在包的两端（i=%d）", n, i)
			}
		}
		if n == last {
			manifestAt = i
		}
	}
	if blobCount == 0 {
		t.Error("包里没有 blobs/ 条目，但语料里有图片与 RTF")
	}
	if manifestAt == 0 {
		t.Error("清单也是第一个条目，顺序约束没有区分度")
	}

	// 清单里的 stats 必须与实际条目数一致。
	mf := readManifestFromZip(t, zr, last)
	if mf.Stats.Items != res.Items {
		t.Errorf("清单 stats.items = %d，实际导出 %d", mf.Stats.Items, res.Items)
	}
	if mf.Stats.Categories != res.Categories || mf.Stats.Tags != res.Tags {
		t.Errorf("清单 stats 分类/标签 = %d/%d，实际 %d/%d",
			mf.Stats.Categories, mf.Stats.Tags, res.Categories, res.Tags)
	}
	if mf.Format != FormatTag {
		t.Errorf("清单 format = %q，想要 %q", mf.Format, FormatTag)
	}
	if mf.AppVersion != "9.9.9" {
		t.Errorf("清单 appVersion = %q，想要 9.9.9", mf.AppVersion)
	}
	if len(mf.Items) != 7 {
		t.Errorf("清单里 items 长度 = %d，想要 7", len(mf.Items))
	}
}

// TestExport_BlobListedOnlyIfPresent 确认"清单引用到的 blob 一定在包里"。
//
// 反过来的漏法很常见：清单写了 blobs[]，包里却没有那个文件，
// 导入时才在逐条处理时报一串错。这里直接把包的 content 对一遍。
func TestExport_BlobListedOnlyIfPresent(t *testing.T) {
	h := newHarness(t)
	h.seedAll()

	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{OutputDir: h.outDir}, nil, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	zr, err := zip.OpenReader(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()

	have := map[string]bool{}
	for _, f := range zr.File {
		have[f.Name] = true
	}
	mf := readManifestFromZip(t, zr, ManifestJSON)
	refs := 0
	for _, it := range mf.Items {
		for _, b := range it.Blobs {
			refs++
			if !have[b.Path] {
				t.Errorf("条目 %d 引用了 %s，但包里没有这个条目", it.ID, b.Path)
			}
			if b.Bytes <= 0 {
				t.Errorf("条目 %d 的 %s 的 bytes = %d，应回填实际写入量", it.ID, b.Path, b.Bytes)
			}
		}
	}
	if refs == 0 {
		t.Fatal("语料里的 blob 一个都没被引用，这条测试没有区分度")
	}
}

// TestExport_ScopeFilters 校验 scope 过滤真的在过滤。
func TestExport_ScopeFilters(t *testing.T) {
	h := newHarness(t)
	_, _, imageFP, _, _, pinnedFP := h.seedAll()

	// pinned 范围：只该有那一条置顶条目。
	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		Scope: ScopePinned, OutputDir: h.outDir,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export(pinned): %v", err)
	}
	if res.Items != 1 {
		t.Fatalf("pinned 范围导出 %d 条，想要 1", res.Items)
	}
	mf := manifestOf(t, res.Path)
	if mf.Items[0].Fingerprint != pinnedFP {
		t.Errorf("pinned 范围导出的不是置顶条目：%s", mf.Items[0].Fingerprint)
	}

	// 排除图片与混排：语料里只有 1 张图。
	res2, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		ExcludeKinds: []string{"image", "mixed"}, OutputDir: h.outDir,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export(exclude): %v", err)
	}
	if res2.Items != 6 {
		t.Errorf("排除图片后导出 %d 条，想要 6", res2.Items)
	}
	for _, it := range manifestOf(t, res2.Path).Items {
		if it.Kind == "image" {
			t.Errorf("排除规则没生效，导出里有图片条目（fp=%s, 语料里图片是 %s）", it.Fingerprint, imageFP)
		}
	}
}

// ── 验收判据 1：导出 → 清库 → 导入，全部字段一致 ────────────────

// TestAcceptance1_ExportThenImportPreservesEverything 对应 §12 第 1 条。
//
// 断言的是**逐字段相等**，不是"条数对了"——条数对了但 use_count 归 1、
// 分类丢了、过期时间变成 null，全都是这个功能最典型的坏法。
func TestAcceptance1_ExportThenImportPreservesEverything(t *testing.T) {
	h := newHarness(t)
	h.seedAll()

	before := dumpItems(t, h.db)
	if len(before) != 7 {
		t.Fatalf("清库前有 %d 条，想要 7", len(before))
	}

	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		ManifestFormat: "json", OutputDir: h.outDir, AppVersion: "1.2.3",
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// ── 清库：删光条目、分类、标签，模拟"换了一台机器" ──
	wipeAll(t, h.db)
	if n, _ := h.db.CountAll(h.ctx()); n != 0 {
		t.Fatalf("清库后仍有 %d 条", n)
	}

	pc, err := Precheck(h.ctx(), h.db, res.Path, ImportOptions{})
	if err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	if pc.Total != 7 {
		t.Fatalf("预检总条目 = %d，想要 7", pc.Total)
	}
	if pc.WillImport != 7 || pc.SkipDuplicate != 0 || pc.Invalid != 0 {
		t.Errorf("预检结果：将导入 %d，跳过重复 %d，非法 %d；想要 7/0/0",
			pc.WillImport, pc.SkipDuplicate, pc.Invalid)
	}

	ir, err := Import(h.ctx(), h.db, h.blobs, res.Path, ImportOptions{}, pc, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if ir.Status != store.ImportOK {
		t.Errorf("导入状态 = %q，想要 %q（错误：%v）", ir.Status, store.ImportOK, ir.Errors)
	}
	if ir.Imported != 7 || ir.Failed != 0 {
		t.Fatalf("导入 %d 条、失败 %d 条，想要 7/0", ir.Imported, ir.Failed)
	}
	if !ir.RollbackPossible {
		t.Error("RollbackPossible = false，导入后应当可以一键回滚")
	}

	after := dumpItems(t, h.db)
	if len(after) != len(before) {
		t.Fatalf("导入后 %d 条，导出前 %d 条", len(after), len(before))
	}

	// 逐条按指纹对齐后逐字段比较。
	orig := indexByFP(before)
	got := indexByFP(after)
	for fp, o := range orig {
		g, ok := got[fp]
		if !ok {
			t.Errorf("条目 %s 没导回来", fp)
			continue
		}
		compareItem(t, o, g, h)
	}
}

// compareItem 逐字段比较两条条目，只忽略"本来就该变"的字段。
//
// 刻意不忽略的东西（这些恰恰是最容易丢的）：
// use_count / first_seen_at / last_used_at / category_id / 分类名 /
// 标签集合 / pinned / expires_at / byte_size / 来源应用 / 文件列表。
//
// 允许不同的只有 id（两台机器的 ID 无关）与 import_id（导入才有）。
func compareItem(t *testing.T, o, g store.Item, h *harness) {
	t.Helper()
	label := fmt.Sprintf("条目 %s", o.Fingerprint)

	if g.Kind != o.Kind {
		t.Errorf("%s: kind = %q，想要 %q", label, g.Kind, o.Kind)
	}
	if g.Preview != o.Preview {
		t.Errorf("%s: preview = %q，想要 %q", label, g.Preview, o.Preview)
	}
	if g.ByteSize != o.ByteSize {
		t.Errorf("%s: byte_size = %d，想要 %d", label, g.ByteSize, o.ByteSize)
	}
	if g.UseCount != o.UseCount {
		t.Errorf("%s: use_count = %d，想要 %d", label, g.UseCount, o.UseCount)
	}
	if g.FirstSeenAt != o.FirstSeenAt {
		t.Errorf("%s: first_seen_at = %d，想要 %d", label, g.FirstSeenAt, o.FirstSeenAt)
	}
	if g.CreatedAt != o.CreatedAt {
		t.Errorf("%s: created_at = %d，想要 %d", label, g.CreatedAt, o.CreatedAt)
	}
	if !eqI64Ptr(g.LastUsedAt, o.LastUsedAt) {
		t.Errorf("%s: last_used_at = %v，想要 %v", label, ptrStr(g.LastUsedAt), ptrStr(o.LastUsedAt))
	}
	if !eqI64Ptr(g.ExpiresAt, o.ExpiresAt) {
		t.Errorf("%s: expires_at = %v，想要 %v", label, ptrStr(g.ExpiresAt), ptrStr(o.ExpiresAt))
	}
	if g.Pinned != o.Pinned {
		t.Errorf("%s: pinned = %v，想要 %v", label, g.Pinned, o.Pinned)
	}
	if g.SourceAppID != o.SourceAppID || g.SourceAppName != o.SourceAppName {
		t.Errorf("%s: 来源应用 = %s/%s，想要 %s/%s",
			label, g.SourceAppID, g.SourceAppName, o.SourceAppID, o.SourceAppName)
	}
	if !eqStrPtr(g.SourceURL, o.SourceURL) {
		t.Errorf("%s: source_url = %v，想要 %v", label, ptrStr(g.SourceURL), ptrStr(o.SourceURL))
	}
	if !eqStrPtr(g.TextContent, o.TextContent) {
		t.Errorf("%s: text 不一致（got %v, want %v）", label, ptrStr(g.TextContent), ptrStr(o.TextContent))
	}
	if !eqStrPtr(g.HTMLContent, o.HTMLContent) {
		t.Errorf("%s: html 不一致", label)
	}
	if !eqSlice(g.FilePaths, o.FilePaths) {
		t.Errorf("%s: file_paths = %v，想要 %v", label, g.FilePaths, o.FilePaths)
	}

	// 分类：ID 必然不同（新库新 ID），但**名字**必须一致。
	if (g.CategoryID == nil) != (o.CategoryID == nil) {
		t.Errorf("%s: 分类存在性不一致（got %v, want %v）", label, ptrStr(g.CategoryID), ptrStr(o.CategoryID))
	} else if g.CategoryID != nil {
		gn := catName(t, h, *g.CategoryID)
		on := catName(t, h, *o.CategoryID)
		if gn != on {
			t.Errorf("%s: 分类名 = %q，想要 %q", label, gn, on)
		}
	}

	if !eqSlice(gotTags(t, h, g.ID), gotTags(t, h, o.ID)) {
		t.Errorf("%s: 标签集合 = %v，想要 %v", label, gotTags(t, h, g.ID), gotTags(t, h, o.ID))
	}

	// blob 必须真的落在盘上（不能只是数据库里有个路径）。
	for _, rel := range []*string{g.ImagePath, g.ThumbPath, g.RTFPath} {
		if rel == nil {
			continue
		}
		if !h.blobs.Exists(*rel) {
			t.Errorf("%s: 引用的 blob %s 在盘上不存在", label, *rel)
		}
		data, err := h.blobs.Get(*rel)
		if err != nil {
			t.Errorf("%s: 读 blob %s: %v", label, *rel, err)
			continue
		}
		if int64(len(data)) == 0 {
			t.Errorf("%s: blob %s 是空文件", label, *rel)
		}
	}
}

// ── 验收判据 2：同一包导入两次，第二次全部跳过 ──────────────────

// TestAcceptance2_ImportTwiceIsIdempotent 对应 §12 第 2 条。
//
// 关键在第 4c 步的冲突策略：默认 merge 要**累加** use_count，
// 所以"第二次全部跳过"不是靠"什么都不做"达成的——重复条目走的是
// 合并路径，use_count 会涨、last_used_at 会取较新，但**不会新增行**，
// 而且**不会重复插 blob**（blobs/ 是内容寻址的）。
func TestAcceptance2_ImportTwiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seedAll()

	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{OutputDir: h.outDir}, nil, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	wipeAll(t, h.db)

	// 第一次导入
	r1, err := Import(h.ctx(), h.db, h.blobs, res.Path, ImportOptions{}, nil, nil)
	if err != nil {
		t.Fatalf("第一次 Import: %v", err)
	}
	if r1.Imported != 7 || r1.Skipped != 0 || r1.Merged != 0 {
		t.Fatalf("第一次导入：imported=%d skipped=%d merged=%d failed=%d，想要 7/0/0/0",
			r1.Imported, r1.Skipped, r1.Merged, r1.Failed)
	}
	afterFirst := dumpItems(t, h.db)
	fpCountAfterFirst := blobFileCount(t, h.blobs)

	// 第二次导入同一个包
	r2, err := Import(h.ctx(), h.db, h.blobs, res.Path, ImportOptions{}, nil, nil)
	if err != nil {
		t.Fatalf("第二次 Import: %v", err)
	}

	// 行数绝对不能变——这是"幂等"最硬的那条。
	afterSecond := dumpItems(t, h.db)
	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("第二次导入后条目数从 %d 变成 %d（重复导入了）", len(afterFirst), len(afterSecond))
	}
	// 真正**新插入**的行数 = Imported - Merged - Overwritten（见 ImportResult 的注释）。
	if inserted := r2.Imported - r2.Merged - r2.Overwritten; inserted != 0 {
		t.Errorf("第二次导入新插入了 %d 行，想要 0（应全部走冲突路径）", inserted)
	}
	// 默认策略是 merge，所以 7 条都该走 merge。
	if r2.Merged != 7 || r2.Skipped != 0 || r2.Overwritten != 0 {
		t.Errorf("第二次导入 merged=%d skipped=%d overwritten=%d，想要 7/0/0（默认策略是 merge）",
			r2.Merged, r2.Skipped, r2.Overwritten)
	}
	if len(r2.Errors) != 0 {
		t.Errorf("第二次导入报了错：%v", r2.Errors)
	}

	// blob 文件不该变多：内容寻址，同一份字节落同一个路径。
	if n := blobFileCount(t, h.blobs); n != fpCountAfterFirst {
		t.Errorf("第二次导入后 blob 文件数从 %d 变成 %d（重复写了内容相同的 blob）",
			fpCountAfterFirst, n)
	}

	// 第二次导入不该动到"内容"，只动使用统计。
	if r2.Merged > 0 {
		for _, a := range afterSecond {
			for _, b := range afterFirst {
				if a.Fingerprint != b.Fingerprint {
					continue
				}
				if a.UseCount < b.UseCount {
					t.Errorf("条目 %s 的 use_count 从 %d 掉到 %d（merge 应当只增不减）",
						a.Fingerprint, b.UseCount, a.UseCount)
				}
				if a.FirstSeenAt != b.FirstSeenAt {
					t.Errorf("条目 %s 的 first_seen_at 被改写：%d → %d",
						a.Fingerprint, b.FirstSeenAt, a.FirstSeenAt)
				}
			}
		}
	}

	// 回滚：把这一批导进来的全删掉。
	// 注意第二次导入也在 imports 里留了记录，所以这里显式回滚**第二次**那批，
	// 它按定义不该带走任何条目。
	r2Rolled, err := Rollback(h.ctx(), h.db, r2.ImportID)
	if err != nil {
		t.Fatalf("回滚第二次导入: %v", err)
	}
	if r2Rolled != 0 {
		t.Errorf("回滚第二次导入删掉了 %d 条，想要 0（第二次没有新增任何条目——"+
			"若这里非 0，说明第二次导入其实是在 overwrite 而不是 merge，行数不变只是因为覆盖了同一行）", r2Rolled)
	}
	if n, _ := h.db.CountAll(h.ctx()); n != int64(len(afterFirst)) {
		t.Errorf("回滚后条目数 = %d，想要 %d", n, len(afterFirst))
	}

	// 再回滚第一批：这次该把 7 条全带走。
	rolled, err := Rollback(h.ctx(), h.db, r1.ImportID)
	if err != nil {
		t.Fatalf("回滚第一次导入: %v", err)
	}
	if rolled != 7 {
		t.Errorf("回滚第一次导入删掉 %d 条，想要 7", rolled)
	}
	if n, _ := h.db.CountAll(h.ctx()); n != 0 {
		t.Errorf("两次回滚后条目数 = %d，想要 0", n)
	}
}

// ── 草稿（§3.7）─────────────────────────────────────────────────

// seedDraft 直接写一行 drafts。与 seedItem 同一理由：往返测试要断言
// "每个字段都还在"，种子数据就必须完全可控，不能由 store 决定时间戳。
func (h *harness) seedDraft(title, md string, seq, sortOrder, createdAt, updatedAt int64, archived bool) int64 {
	h.t.Helper()
	var seqArg, archivedArg any
	if seq > 0 {
		seqArg = seq
	}
	if archived {
		archivedArg = updatedAt
	}
	var id int64
	err := h.db.Writer().QueryRowContext(h.ctx(), `
INSERT INTO drafts (title, md, seq, sort_order, created_at, updated_at, archived_at)
VALUES (?,?,?,?,?,?,?)
RETURNING id`,
		title, md, seqArg, sortOrder, createdAt, updatedAt, archivedArg).Scan(&id)
	if err != nil {
		h.t.Fatalf("写入草稿 %q: %v", title, err)
	}
	return id
}

// repoDraft 是"往返比较"用的草稿投影。
//
// 只比该比的东西：**不含 id**（两台机器的 ID 无关）、**不含 seq**
// （导入的草稿一律没有编号，见 store.InsertImportedDraft 的三条理由）。
type repoDraft struct {
	Title     string
	MD        string
	Seq       int64
	CreatedAt int64
	UpdatedAt int64
	Archived  bool
}

func draftsOf(t *testing.T, db *store.DB) []repoDraft {
	t.Helper()
	rows, err := db.Writer().Query(
		`SELECT title, md, COALESCE(seq, 0), created_at, updated_at, archived_at IS NOT NULL
		   FROM drafts ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		t.Fatalf("读全部草稿: %v", err)
	}
	defer rows.Close()
	var out []repoDraft
	for rows.Next() {
		var d repoDraft
		if err := rows.Scan(&d.Title, &d.MD, &d.Seq, &d.CreatedAt, &d.UpdatedAt, &d.Archived); err != nil {
			t.Fatalf("scan draft: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历草稿: %v", err)
	}
	return out
}

// TestAcceptance3_DraftsRoundTrip：草稿（含正文里的贴图）往返无损，
// 且一键回滚能把带进来的草稿一并撤掉。
//
// 这条测试的第一个断言就是设计里最容易错的地方：**只被草稿引用的图片**。
// 它不在任何 items.image_path 里，如果 blob 扫描只扫 items（第一版就是
// 那样），这张图不会进包 —— 而正文里的引用还在，导入后就是一张必然
// 破的图，且没有任何报错。
func TestAcceptance3_DraftsRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.seedAll()

	// 草稿专用贴图（不被任何条目引用）。
	imgRel, _ := h.makePNGBlob(4, 4, color.RGBA{R: 9, G: 8, B: 7, A: 255})

	mdA := "会议记录\n\n- 结论一\n- 结论二\n\n![截图](blob/" + imgRel + ")\n\n[资料](https://example.com/doc)\n"
	h.seedDraft("会议记录", mdA, 1, 1, 1_700_000_000, 1_700_000_600, false)
	h.seedDraft("", "", 2, 2, 1_700_000_100, 1_700_000_100, false)
	// 已归档（软删除）的草稿不进包——与回收站条目同一条规则（§5）。
	h.seedDraft("删掉的", "不该出现在包里", 3, 3, 1_700_000_200, 1_700_000_200, true)

	want := []repoDraft{
		{Title: "会议记录", MD: mdA, Seq: 1, CreatedAt: 1_700_000_000, UpdatedAt: 1_700_000_600},
		{Title: "", MD: "", Seq: 2, CreatedAt: 1_700_000_100, UpdatedAt: 1_700_000_100},
	}

	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		ManifestFormat: "json", OutputDir: h.outDir, AppVersion: "1.2.3",
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Drafts != 2 {
		t.Fatalf("导出草稿数 = %d，想要 2（归档的那条不该算）", res.Drafts)
	}

	// ── 包内断言：草稿段存在、正文已改写成包内前缀、贴图在 blobs/ 下 ──
	zr, err := zip.OpenReader(res.Path)
	if err != nil {
		t.Fatalf("打开包: %v", err)
	}
	defer func() { _ = zr.Close() }()

	foundBlob := false
	var mf *zip.File
	for _, f := range zr.File {
		if f.Name == BlobDirPrefix+imgRel {
			foundBlob = true
		}
		if f.Name == ManifestJSON {
			mf = f
		}
	}
	if !foundBlob {
		t.Errorf("草稿专用贴图没进包：%s（blob 扫描漏了 draft 的引用）", BlobDirPrefix+imgRel)
	}
	if mf == nil {
		t.Fatal("包里没有 manifest.json")
	}
	raw, err := readZipEntryLimited(mf, 8<<20)
	if err != nil {
		t.Fatalf("读清单: %v", err)
	}
	man, err := decodeManifest(ManifestJSON, raw)
	if err != nil {
		t.Fatalf("解析清单: %v", err)
	}
	if len(man.Drafts) != 2 || man.Stats.Drafts != 2 {
		t.Fatalf("清单里草稿 %d 条、stats.drafts = %d，想要 2/2", len(man.Drafts), man.Stats.Drafts)
	}
	if !strings.Contains(man.Drafts[0].MD, "](blobs/"+imgRel+")") {
		t.Errorf("清单位图引用应为包内前缀 blobs/\n---\n%s", man.Drafts[0].MD)
	}
	if strings.Contains(man.Drafts[0].MD, "](blob/") {
		t.Errorf("清单里不该出现库内前缀 blob/（两套前缀混了）\n---\n%s", man.Drafts[0].MD)
	}

	// ── 换一台机器：清库后导入 ──
	wipeAll(t, h.db)

	pc, err := Precheck(h.ctx(), h.db, res.Path, ImportOptions{})
	if err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	if pc.TotalDrafts != 2 || pc.WillImportDrafts != 2 {
		t.Errorf("预检草稿 = %d/%d，想要 2/2", pc.TotalDrafts, pc.WillImportDrafts)
	}

	ir, err := Import(h.ctx(), h.db, h.blobs, res.Path, ImportOptions{}, pc, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if ir.DraftsImported != 2 || ir.DraftsFailed != 0 {
		t.Fatalf("导入草稿 %d 条、失败 %d 条，想要 2/0（错误：%v）",
			ir.DraftsImported, ir.DraftsFailed, ir.Errors)
	}
	if !ir.RollbackPossible {
		t.Error("RollbackPossible = false，导入草稿后应当可以一键回滚")
	}

	got := draftsOf(t, h.db)
	if len(got) != len(want) {
		t.Fatalf("导入后草稿 %d 条，想要 %d", len(got), len(want))
	}
	for i := range want {
		// 库内形态：引用必须改回 blob/ 前缀。
		w := want[i]
		w.MD = strings.ReplaceAll(w.MD, "blobs/", "blob/")
		// 导入的草稿不占本机编号（seq 一律 NULL）。
		w.Seq = 0
		if got[i] != w {
			t.Errorf("草稿[%d] 不一致\n got %+v\nwant %+v", i, got[i], w)
		}
	}

	// 贴图必须真的落盘了：正文里留着引用但文件不在 = 破图。
	if _, err := os.Stat(h.blobs.Abs(imgRel)); err != nil {
		t.Errorf("导入后贴图不在 blobs/ 里：%v", err)
	}

	// ── 一键回滚要把草稿也带走 ──
	if _, err := Rollback(h.ctx(), h.db, ir.ImportID); err != nil {
		t.Fatalf("回滚: %v", err)
	}
	if left := draftsOf(t, h.db); len(left) != 0 {
		t.Errorf("回滚后还剩 %d 条草稿：%+v", len(left), left)
	}
}

// 范围导出不该带草稿：pinned / category / range 选的是**条目**的子集，
// 草稿与这三个条件正交（§3.7）。带进去等于无视用户在向导里选的范围。
func TestExport_NonFullScopeExcludesDrafts(t *testing.T) {
	h := newHarness(t)
	h.seedAll()
	h.seedDraft("草稿 1", "正文", 1, 1, 1_700_000_000, 1_700_000_000, false)

	for _, scope := range []ExportScope{ScopePinned, ScopeCategory, ScopeRange} {
		t.Run(string(scope), func(t *testing.T) {
			res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
				Scope: scope, ManifestFormat: "json", OutputDir: h.outDir,
			}, nil, nil)
			if err != nil {
				t.Fatalf("Export(%s): %v", scope, err)
			}
			if res.Drafts != 0 {
				t.Errorf("scope=%s 导出了 %d 条草稿，想要 0", scope, res.Drafts)
			}
		})
	}
	// full 时必须带上（否则上面的断言可能因为"永远不导出"而假绿）。
	res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{
		Scope: ScopeFull, ManifestFormat: "json", OutputDir: h.outDir,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Export(full): %v", err)
	}
	if res.Drafts != 1 {
		t.Errorf("scope=full 导出 %d 条草稿，想要 1", res.Drafts)
	}
}

// TestImport_AllConflictPoliciesDoNotDeadlock 是**死锁**的回归测试。
//
// 背景：写句柄是 SetMaxOpenConns(1) 的单连接池，而导入循环用一个长事务
// 占住那唯一一条连接。第一版 importOne 在事务里调了 db.SoftDelete /
// db.MergeIntoExisting —— 这两者都走 d.w.ExecContext，也就是向池里再要
// 一条连接。池子里没有、也不会再有：**永久阻塞，不报错、不超时**。
//
// 三种策略走的是三条不同分支，所以三条都得跑一遍：
//
//	merge     → MergeIntoExistingTx
//	skip      → 直接返回（本来就不碰库）
//	overwrite → SoftDeleteTx
//
// 这条测试的价值在于"跑得完"本身就是断言：一旦有人把某处改回 db.*，
// 它会挂到 go test 的超时（默认 10 分钟）并打出 goroutine 栈，而不是
// 悄悄通过。
func TestImport_AllConflictPoliciesDoNotDeadlock(t *testing.T) {
	for _, policy := range []string{ConflictMerge, ConflictSkip, ConflictOverwrite} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t)
			h.seedAll()

			res, err := Export(h.ctx(), h.db, h.blobs, ExportOptions{OutputDir: h.outDir}, nil, nil)
			if err != nil {
				t.Fatalf("Export: %v", err)
			}

			// 两次导入，第二次才会走到冲突分支。
			if _, err := Import(h.ctx(), h.db, h.blobs, res.Path,
				ImportOptions{ConflictPolicy: policy}, nil, nil); err != nil {
				t.Fatalf("第一次 Import(%s): %v", policy, err)
			}
			r2, err := Import(h.ctx(), h.db, h.blobs, res.Path,
				ImportOptions{ConflictPolicy: policy}, nil, nil)
			if err != nil {
				t.Fatalf("第二次 Import(%s): %v（如果是「卡住不动」，就是死锁）", policy, err)
			}

			// 三种策略都不该让行数变化。
			if n, _ := h.db.CountAlive(h.ctx()); n != 7 {
				t.Errorf("策略 %s 下存活条目 = %d，想要 7", policy, n)
			}
			switch policy {
			case ConflictSkip:
				if r2.Skipped != 7 {
					t.Errorf("skip 策略应全部跳过，实际 skipped=%d", r2.Skipped)
				}
			case ConflictMerge:
				if r2.Merged != 7 {
					t.Errorf("merge 策略应全部合并，实际 merged=%d", r2.Merged)
				}
			case ConflictOverwrite:
				if r2.Overwritten != 7 {
					t.Errorf("overwrite 策略应全部覆盖，实际 overwritten=%d", r2.Overwritten)
				}
			}
		})
	}
}

// TestImport_SeesOwnUncommittedRows 验证查重走的是**事务内**的连接。
//
// 造一个包里含两条同指纹条目的包（现实里出现在"同一份内容在源机器上
// 曾是两条不同条目"的边缘情况）。若查重走 d.r（另一个连接池），
// 第二次会看不见第一次刚插入的行 —— 要么撞唯一索引走 skipped，
// 要么更糟：把重复的行插进去。
func TestImport_SeesOwnUncommittedRows(t *testing.T) {
	h := newHarness(t)

	pkg := filepath.Join(h.dir, "dup.clipbak")
	now := time.Now()
	mf := Manifest{
		Header: Header{
			Format: FormatTag, FormatVersion: FormatVersion, AppVersion: "test",
			ExportedAt: now, Platform: "macos", Scope: "full",
			Stats: Stats{Items: 2},
		},
		Items: []Item{
			{
				ID: 1, Kind: "text", Preview: "同指纹 A", Fingerprint: fpOf("dup"),
				ByteSize: 10, FirstSeenAt: now, CreatedAt: now, UseCount: 3,
				Text: strPtr("同指纹内容"),
			},
			{
				ID: 2, Kind: "text", Preview: "同指纹 B", Fingerprint: fpOf("dup"),
				ByteSize: 10, FirstSeenAt: now, CreatedAt: now, UseCount: 4,
				Text: strPtr("同指纹内容"),
			},
		},
	}
	writeHandmadeZip(t, pkg, mf, nil)

	r, err := Import(h.ctx(), h.db, h.blobs, pkg, ImportOptions{}, nil, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if n, _ := h.db.CountAll(h.ctx()); n != 1 {
		t.Fatalf("导入后条目数 = %d，想要 1（两条同指纹只该留下一行）", n)
	}
	// 真正新插入的行数 = Imported - Merged - Overwritten。
	if inserted := r.Imported - r.Merged - r.Overwritten; inserted != 1 {
		t.Errorf("新插入 %d 行，想要 1", inserted)
	}
	// 关键的区分性断言：第二条必须走 **merge**，而不是撞唯一索引被当成 skipped。
	//
	// 这两条路径的最终效果都是"只留一行"，所以只看行数分不出来。
	// 差别在于：
	//   走 merge  → 说明事务内的查重**看见了自己刚插入的那行**（正确）
	//   走 skip   → 说明查重看不见未提交的行，只能靠唯一索引兜底
	//              （能兜住，但 use_count 不会累加，而且掩盖了真问题）
	if r.Merged != 1 {
		t.Errorf("第二条 merged = %d，想要 1（若为 0 且 skipped=1，说明查重走的是"+
			"事务外的连接、看不见自己未提交的插入）", r.Merged)
	}
}

// ── 安全：ZIP Slip 与解压炸弹 ───────────────────────────────────

// TestImport_RejectsZipSlip 是本包最重要的安全测试。
//
// 攻击形态：包里有一个条目名形如 `blobs/../../../../tmp/pwned.png`。
// 如果实现用 `filepath.Join(outDir, entryName)` 落盘，就会写到包外。
//
// 本包的防线有两条，这里两条都验：
//  1. validateZipEntries 在预检阶段就拒绝路径穿越的条目名；
//  2. 落盘路径**由 sha256 重算**，根本不用条目名（即使第 1 条漏了也写不到包外）。
func TestImport_RejectsZipSlip(t *testing.T) {
	h := newHarness(t)

	// 造一个手写的最小包：清单引用一个逃逸路径。
	evil := filepath.Join(h.dir, "evil.clipbak")
	outside := filepath.Join(h.dir, "PWNED.txt")

	mf := Manifest{
		Header: Header{
			Format: FormatTag, FormatVersion: FormatVersion, AppVersion: "test",
			ExportedAt: time.Now(), Platform: "macos", Scope: "full",
			Stats: Stats{Items: 1},
		},
		Items: []Item{{
			ID: 1, Kind: "image", Preview: "恶意条目",
			Fingerprint: fpOf("evil"), ByteSize: 10,
			FirstSeenAt: time.Now(), CreatedAt: time.Now(),
			Blobs: []BlobRef{{
				Role: roleImage,
				// 这是关键：条目名逃出 blobs/ 目录。
				Path:   BlobDirPrefix + "../../../" + filepath.Base(outside),
				Mime:   "image/png",
				Bytes:  10,
				SHA256: strings.Repeat("a", 64),
			}},
		}},
	}
	writeHandmadeZip(t, evil, mf, map[string][]byte{
		BlobDirPrefix + "../../../" + filepath.Base(outside): []byte("pwned"),
	})

	pc, err := Precheck(h.ctx(), h.db, evil, ImportOptions{})
	if err == nil {
		// 预检没拒绝 → 至少导入必须拒绝，且不能写出包外文件。
		if _, ierr := Import(h.ctx(), h.db, h.blobs, evil, ImportOptions{}, pc, nil); ierr == nil {
			t.Error("含路径穿越条目名的包既没被预检拒绝，也没被导入拒绝")
		}
	} else {
		t.Logf("预检按预期拒绝了含路径穿越的包：%v", err)
	}

	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("ZIP Slip 成功写出了包外文件：%s", outside)
	}
}

// TestImport_RejectsZipBomb 校验解压炸弹防护（§9）。
//
// 做法：清单声明 BlobBytes 很小，但实际写进包的 blob 解压后很大。
// 预检必须据此拒绝，而不是老老实实解出几 GB 把磁盘写满。
func TestImport_RejectsZipBomb(t *testing.T) {
	h := newHarness(t)

	bomb := filepath.Join(h.dir, "bomb.clipbak")
	// 1 MiB 的零字节，ZIP 压缩后会极小 —— 典型的炸弹形态。
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	sum := sha256Hex(payload)

	mf := Manifest{
		Header: Header{
			Format: FormatTag, FormatVersion: FormatVersion, AppVersion: "test",
			ExportedAt: time.Now(), Platform: "macos", Scope: "full",
			// 谎报：说总共只有 16 字节。
			Stats: Stats{Items: 1, BlobBytes: 16},
		},
		Items: []Item{{
			ID: 1, Kind: "image", Preview: "炸弹",
			Fingerprint: fpOf("bomb"), ByteSize: 16,
			FirstSeenAt: time.Now(), CreatedAt: time.Now(),
			Blobs: []BlobRef{{
				Role: roleImage, Path: BlobDirPrefix + "9f/2a/" + sum + ".png",
				Mime: "image/png", Bytes: 16, SHA256: sum,
			}},
		}},
	}
	writeHandmadeZip(t, bomb, mf, map[string][]byte{
		BlobDirPrefix + "9f/2a/" + sum + ".png": payload,
	})

	pc, err := Precheck(h.ctx(), h.db, bomb, ImportOptions{MaxSingleBytes: 4096})
	if err != nil {
		t.Logf("预检按预期拒绝了炸弹包：%v", err)
		return // 这条就是我们要的行为
	}
	// 预检放行的话，导入必须拒绝，且不能把 1 MiB 真写进去。
	if _, ierr := Import(h.ctx(), h.db, h.blobs, bomb, ImportOptions{MaxSingleBytes: 4096}, pc, nil); ierr == nil {
		t.Error("超限的包既没被预检拒绝，也没被导入拒绝")
	}
	var total int64
	_ = h.blobs.Walk(func(bi store.BlobInfo) error {
		total += bi.Size
		return nil
	})
	if total > 4096 {
		t.Errorf("导入后盘上多了 %d 字节，超过单条目上限 4096（炸弹没有被拦住）", total)
	}
}

// ── 辅助 ────────────────────────────────────────────────────────

func writeHandmadeZip(t *testing.T, path string, mf Manifest, extra map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	// README 先写（顺序约束）。
	w, err := zw.Create(ReadmeName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("handmade test package\n")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(extra))
	for n := range extra {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(extra[n]); err != nil {
			t.Fatal(err)
		}
	}
	w, err = zw.Create(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(mf); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func readManifestFromZip(t *testing.T, zr *zip.ReadCloser, name string) *Manifest {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		raw, err := readZipEntryLimited(f, 64<<20)
		if err != nil {
			t.Fatalf("读清单: %v", err)
		}
		m, err := decodeManifest(name, raw)
		if err != nil {
			t.Fatalf("解析清单: %v", err)
		}
		return m
	}
	t.Fatalf("包里没有 %s", name)
	return nil
}

func manifestOf(t *testing.T, pkgPath string) *Manifest {
	t.Helper()
	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	for _, name := range []string{ManifestJSON, ManifestYAML} {
		for _, f := range zr.File {
			if f.Name == name {
				raw, err := readZipEntryLimited(f, 64<<20)
				if err != nil {
					t.Fatal(err)
				}
				m, err := decodeManifest(name, raw)
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
		}
	}
	t.Fatal("包里没有清单")
	return nil
}

// dumpItems 读出全部条目的完整字段（含回收站）。
func dumpItems(t *testing.T, db *store.DB) []store.Item {
	t.Helper()
	rows, err := db.Writer().Query("SELECT " + strings.Join(itemCols(), ", ") + " FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("读全部条目: %v", err)
	}
	defer rows.Close()
	var out []store.Item
	for rows.Next() {
		var it store.Item
		if err := scanItemRow(rows, &it); err != nil {
			t.Fatalf("scan item: %v", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历条目: %v", err)
	}
	return out
}

// itemCols 与 store.itemColumns 保持一致。
// 复制一份而不是导出它：测试要能独立于 store 的内部改动而报警。
func itemCols() []string {
	return strings.Split(strings.ReplaceAll(strings.TrimSpace(`
id, kind, text_content, html_content, rtf_path, image_path, thumb_path,
file_paths, preview, fingerprint, byte_size, source_app_id, source_app_name, source_url,
category_id, pinned, first_seen_at, expires_at, ttl_source, created_at, last_used_at,
use_count, deleted_at, import_id`), "\n", ""), ", ")
}

func scanItemRow(rows interface{ Scan(...any) error }, it *store.Item) error {
	var (
		text, html, rtf, img, thumb sql.NullString
		files                       sql.NullString
		url                         sql.NullString
		catID, expires, lastUsed    sql.NullInt64
		deleted, importID           sql.NullInt64
		ttlSource                   sql.NullString
		pinned                      int64
	)
	if err := rows.Scan(
		&it.ID, &it.Kind, &text, &html, &rtf, &img, &thumb,
		&files, &it.Preview, &it.Fingerprint, &it.ByteSize,
		&it.SourceAppID, &it.SourceAppName, &url,
		&catID, &pinned, &it.FirstSeenAt, &expires, &ttlSource, &it.CreatedAt,
		&lastUsed, &it.UseCount, &deleted, &importID,
	); err != nil {
		return err
	}
	it.TextContent = nsPtr(text)
	it.HTMLContent = nsPtr(html)
	it.RTFPath = nsPtr(rtf)
	it.ImagePath = nsPtr(img)
	it.ThumbPath = nsPtr(thumb)
	it.SourceURL = nsPtr(url)
	it.TTLSource = nsPtr(ttlSource)
	it.CategoryID = niPtr(catID)
	it.ExpiresAt = niPtr(expires)
	it.LastUsedAt = niPtr(lastUsed)
	it.DeletedAt = niPtr(deleted)
	it.ImportID = niPtr(importID)
	it.Pinned = pinned != 0
	if files.Valid && files.String != "" {
		var arr []string
		if err := json.Unmarshal([]byte(files.String), &arr); err == nil {
			it.FilePaths = arr
		}
	}
	return nil
}

func nsPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func niPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func wipeAll(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"DELETE FROM item_tags",
		"DELETE FROM items",
		"DELETE FROM drafts",
		"DELETE FROM tags",
		"DELETE FROM categories",
		"DELETE FROM imports",
	} {
		if _, err := db.Writer().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("清库 %s: %v", stmt, err)
		}
	}
}

func catName(t *testing.T, h *harness, id int64) string {
	t.Helper()
	var name string
	err := h.db.Writer().QueryRow("SELECT name FROM categories WHERE id = ?", id).Scan(&name)
	if err != nil {
		return fmt.Sprintf("<查不到 id=%d: %v>", id, err)
	}
	return name
}

func gotTags(t *testing.T, h *harness, itemID int64) []string {
	t.Helper()
	rows, err := h.db.Writer().Query(
		`SELECT t.name FROM item_tags it JOIN tags t ON t.id = it.tag_id
		  WHERE it.item_id = ? ORDER BY t.name`, itemID)
	if err != nil {
		t.Fatalf("查标签: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func blobFileCount(t *testing.T, bs *store.BlobStore) int {
	t.Helper()
	n := 0
	if err := bs.Walk(func(store.BlobInfo) error { n++; return nil }); err != nil {
		t.Fatalf("遍历 blob: %v", err)
	}
	return n
}

func indexByFP(items []store.Item) map[string]store.Item {
	out := make(map[string]store.Item, len(items))
	for _, it := range items {
		out[it.Fingerprint] = it
	}
	return out
}

func eqI64Ptr(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func eqStrPtr(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func eqSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ptrStr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *p)
}
