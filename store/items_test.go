package store

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"
)

// ── 写入路径：去重与置顶 ────────────────────────────────────────

// HANDOFF-PROMPT 验收 2 的存储层等价断言：
// 同一内容复制 100 次 → 库中始终 1 行且 use_count = 100。
func TestPutItem_DedupeAndPinToTop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const fp = "sha256:dedupe-target"
	first := sampleItem(fp, "同一段内容")
	first.FirstSeenAt = 111
	first.CreatedAt = 111

	for i := 1; i <= 100; i++ {
		it := sampleItem(fp, "同一段内容")
		it.FirstSeenAt = 111 // 每次采集都会填当前时刻，但入库不该覆盖首次时间
		it.CreatedAt = int64(1000 + i)
		it.LastUsedAt = ptrInt64(int64(1000 + i))

		res, err := PutItem(ctx, db.Writer(), &it)
		if err != nil {
			t.Fatalf("第 %d 次 PutItem: %v", i, err)
		}
		if res.UseCount != int64(i) {
			t.Fatalf("第 %d 次 use_count = %d", i, res.UseCount)
		}
		if res.ID != 1 {
			t.Fatalf("第 %d 次返回了新的 id %d（说明去重失效）", i, res.ID)
		}
		if res.FirstSeenAt != 111 {
			t.Fatalf("first_seen_at 被改写成 %d，应保持 111", res.FirstSeenAt)
		}
	}

	alive, err := db.CountAlive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if alive != 1 {
		t.Fatalf("存活条目数 = %d, want 1", alive)
	}

	got, err := db.GetByFingerprint(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if got.UseCount != 100 {
		t.Fatalf("use_count = %d, want 100", got.UseCount)
	}
	if got.FirstSeenAt != 111 {
		t.Fatalf("first_seen_at = %d, want 111", got.FirstSeenAt)
	}
	if got.CreatedAt != 1100 {
		t.Fatalf("created_at = %d, want 1100（排序键应被刷到最新）", got.CreatedAt)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt != 1100 {
		t.Fatalf("last_used_at = %v, want 1100", got.LastUsedAt)
	}
}

func TestPutItem_KeepsUserSetExpiryOnRepeat(t *testing.T) {
	// 用户单条设定的过期策略必须凌驾于自动重算之上：
	// 同一指纹再次复制时不得把 expires_at / ttl_source 覆盖掉
	db := newTestDB(t)
	ctx := context.Background()

	exp := int64(999999)
	src := "item"
	it := sampleItem("sha256:ttl", "内容")
	it.ExpiresAt = &exp
	it.TTLSource = &src
	if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
		t.Fatal(err)
	}

	again := sampleItem("sha256:ttl", "内容")
	global := int64(1)
	g := "global"
	again.ExpiresAt = &global
	again.TTLSource = &g
	if _, err := PutItem(ctx, db.Writer(), &again); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetByFingerprint(ctx, "sha256:ttl")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Fatalf("expires_at = %v, want %d", got.ExpiresAt, exp)
	}
	if got.TTLSource == nil || *got.TTLSource != "item" {
		t.Fatalf("ttl_source = %v, want item", got.TTLSource)
	}
}

func TestPutItem_SoftDeletedCanBeReinserted(t *testing.T) {
	// uq_items_fp_alive 是 partial index：软删之后同一内容必须能重新录入
	db := newTestDB(t)
	ctx := context.Background()

	it := sampleItem("sha256:phoenix", "内容")
	if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		"UPDATE items SET deleted_at = 5000 WHERE fingerprint = ?", "sha256:phoenix"); err != nil {
		t.Fatal(err)
	}

	res, err := PutItem(ctx, db.Writer(), &it)
	if err != nil {
		t.Fatalf("软删后重新插入失败: %v", err)
	}
	if res.UseCount != 1 {
		t.Fatalf("重新插入的 use_count = %d, want 1", res.UseCount)
	}

	alive, _ := db.CountAlive(ctx)
	all, _ := db.CountAll(ctx)
	trashed, _ := db.CountTrashed(ctx)
	if alive != 1 || all != 2 || trashed != 1 {
		t.Fatalf("alive=%d all=%d trashed=%d, want 1/2/1", alive, all, trashed)
	}
}

func TestPutItem_Validations(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := PutItem(ctx, db.Writer(), nil); err == nil {
		t.Fatal("nil item 应报错")
	}
	it := Item{Kind: "text", CreatedAt: 1, FirstSeenAt: 1}
	if _, err := PutItem(ctx, db.Writer(), &it); err == nil {
		t.Fatal("缺 fingerprint 应报错")
	}
	it.Fingerprint = "sha256:x"
	it.Kind = ""
	if _, err := PutItem(ctx, db.Writer(), &it); err == nil {
		t.Fatal("缺 kind 应报错")
	}
}

func TestItem_FilePathsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	paths := []string{"/Users/me/一 个 文件.txt", "/tmp/b.png"}
	it := Item{
		Kind:        "files",
		FilePaths:   paths,
		Preview:     "一 个 文件.txt 等 2 个文件",
		Fingerprint: "sha256:files",
		FirstSeenAt: 1,
		CreatedAt:   1,
	}
	if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetByFingerprint(ctx, "sha256:files")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FilePaths) != 2 || got.FilePaths[0] != paths[0] || got.FilePaths[1] != paths[1] {
		t.Fatalf("file_paths 往返后 = %v, want %v", got.FilePaths, paths)
	}
}

func TestGetByID_IncludesTrashed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	it := sampleItem("sha256:trash", "内容")
	res, err := PutItem(ctx, db.Writer(), &it)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		"UPDATE items SET deleted_at = 1 WHERE id = ?", res.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetByID(ctx, res.ID); err != nil {
		t.Fatalf("GetByID 应能取到回收站条目: %v", err)
	}
	if _, err := db.GetByFingerprint(ctx, "sha256:trash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByFingerprint 不应返回回收站条目，err = %v", err)
	}
	if _, err := db.GetByID(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 id 应返回 ErrNotFound，err = %v", err)
	}
}

func TestTotalAliveBytes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for i, s := range []string{"12345", "123", "12"} {
		it := sampleItem("sha256:b"+strings.Repeat("x", i+1), s)
		if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
			t.Fatal(err)
		}
	}
	total, err := db.TotalAliveBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5+3+2 {
		t.Fatalf("总字节 = %d, want 10", total)
	}
}

// ── blob 存储 ───────────────────────────────────────────────────

func TestShardPath(t *testing.T) {
	sha := strings.Repeat("9f2a1c3d", 8) // 64 位十六进制
	rel, err := ShardPath(sha, "png")
	if err != nil {
		t.Fatal(err)
	}
	want := "9f/2a/" + sha + ".png"
	if rel != want {
		t.Fatalf("ShardPath = %q, want %q", rel, want)
	}

	// 允许带 sha256: 前缀与带点的扩展名
	rel2, err := ShardPath("sha256:"+sha, ".PNG")
	if err != nil {
		t.Fatal(err)
	}
	if rel2 != "9f/2a/"+sha+".PNG" {
		t.Fatalf("ShardPath = %q", rel2)
	}

	for _, bad := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := ShardPath(bad, "png"); err == nil {
			t.Errorf("ShardPath(%q) 应当报错", bad)
		}
	}
	if _, err := ShardPath(sha, ""); err == nil {
		t.Error("空扩展名应报错")
	}
	if _, err := ShardPath(sha, "../evil"); err == nil {
		t.Error("非法扩展名应报错")
	}
}

func TestSafeRel(t *testing.T) {
	good := []string{"9f/2a/abc.png", "a/b/c.rtf"}
	for _, g := range good {
		if err := SafeRel(g); err != nil {
			t.Errorf("SafeRel(%q) = %v, 不应报错", g, err)
		}
	}
	bad := []string{"", "../evil.png", "a/../../etc/passwd", "/abs/path.png", `c:\x.png`, "a\x00b", "./x.png", "a/./b.png"}
	for _, b := range bad {
		if err := SafeRel(b); err == nil {
			t.Errorf("SafeRel(%q) 应当报错", b)
		}
	}
}

func TestThumbRel(t *testing.T) {
	got, err := ThumbRel("9f/2a/abc.png")
	if err != nil {
		t.Fatal(err)
	}
	if got != "9f/2a/abc.thumb.png" {
		t.Fatalf("ThumbRel = %q", got)
	}
	if _, err := ThumbRel("../evil.png"); err == nil {
		t.Fatal("非法路径应报错")
	}
	if _, err := ThumbRel("9f/2a/noext"); err == nil {
		t.Fatal("无扩展名应报错")
	}
}

func TestBlobStore_PutIsContentAddressedAndIdempotent(t *testing.T) {
	bs := newTestBlobs(t)
	data := []byte("some blob content")

	rel1, sha1, err := bs.Put(data, "rtf")
	if err != nil {
		t.Fatal(err)
	}
	rel2, sha2, err := bs.Put(data, "rtf")
	if err != nil {
		t.Fatal(err)
	}
	if rel1 != rel2 || sha1 != sha2 {
		t.Fatalf("同一内容两次写入应得到同一路径：%q/%q vs %q/%q", rel1, sha1, rel2, sha2)
	}
	if len(sha1) != 64 {
		t.Fatalf("sha 长度 = %d", len(sha1))
	}
	back, err := bs.Get(rel1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatal("读回内容不一致")
	}
	if !bs.Exists(rel1) {
		t.Fatal("Exists 应为真")
	}

	// 不同内容 → 不同路径
	rel3, _, err := bs.Put([]byte("other"), "rtf")
	if err != nil {
		t.Fatal(err)
	}
	if rel3 == rel1 {
		t.Fatal("不同内容不应撞路径")
	}
}

func TestBlobStore_RejectsEmpty(t *testing.T) {
	bs := newTestBlobs(t)
	if _, _, err := bs.Put(nil, "png"); err == nil {
		t.Fatal("空内容应被拒绝")
	}
}

func TestBlobStore_FileModeAndPermissions(t *testing.T) {
	bs := newTestBlobs(t)
	rel, _, err := bs.Put([]byte("x"), "rtf")
	if err != nil {
		t.Fatal(err)
	}
	abs := bs.Abs(rel)
	st, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("blob 权限 = %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Fatalf("分片目录权限 = %o, want 700", perm)
	}
}

func TestBlobStore_NoTempFileLeftBehind(t *testing.T) {
	bs := newTestBlobs(t)
	rel, _, err := bs.Put([]byte("payload"), "rtf")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(bs.Abs(rel)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("残留临时文件：%s", e.Name())
		}
	}
}

func TestBlobStore_PutThumb(t *testing.T) {
	bs := newTestBlobs(t)

	// 400x200 源图 → 长边 160，比例保持 → 160x80
	src := image.NewNRGBA(image.Rect(0, 0, 400, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}

	rel, shaHex, err := bs.Put(buf.Bytes(), "png")
	if err != nil {
		t.Fatal(err)
	}
	thumbRel, w, h, err := bs.PutThumb(buf.Bytes(), shaHex)
	if err != nil {
		t.Fatalf("PutThumb: %v", err)
	}
	if w != ThumbMaxEdge || h != ThumbMaxEdge/2 {
		t.Fatalf("缩略图尺寸 = %dx%d, want 160x80", w, h)
	}
	if !strings.HasSuffix(thumbRel, ".thumb.png") {
		t.Fatalf("缩略图路径 = %q", thumbRel)
	}
	if !bs.Exists(thumbRel) {
		t.Fatal("缩略图文件不存在")
	}
	// 缩略图应与原图落在同一个分片目录
	if filepath.Dir(thumbRel) != filepath.Dir(rel) {
		t.Fatalf("缩略图分片目录 %q != 原图 %q", filepath.Dir(thumbRel), filepath.Dir(rel))
	}

	tb, err := bs.Get(thumbRel)
	if err != nil {
		t.Fatal(err)
	}
	cfgW, cfgH, err := DecodePNGSize(tb)
	if err != nil {
		t.Fatal(err)
	}
	if cfgW != 160 || cfgH != 80 {
		t.Fatalf("落盘缩略图尺寸 = %dx%d", cfgW, cfgH)
	}
}

func TestBlobStore_PutThumbDoesNotUpscale(t *testing.T) {
	bs := newTestBlobs(t)
	src := image.NewNRGBA(image.Rect(0, 0, 20, 10))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	_, shaHex, err := bs.Put(buf.Bytes(), "png")
	if err != nil {
		t.Fatal(err)
	}
	_, w, h, err := bs.PutThumb(buf.Bytes(), shaHex)
	if err != nil {
		t.Fatal(err)
	}
	if w != 20 || h != 10 {
		t.Fatalf("小图不应被放大，得到 %dx%d", w, h)
	}
}

func TestBlobStore_RemoveAlsoRemovesThumb(t *testing.T) {
	bs := newTestBlobs(t)
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	rel, shaHex, err := bs.Put(buf.Bytes(), "png")
	if err != nil {
		t.Fatal(err)
	}
	thumbRel, _, _, err := bs.PutThumb(buf.Bytes(), shaHex)
	if err != nil {
		t.Fatal(err)
	}

	if err := bs.Remove(rel); err != nil {
		t.Fatal(err)
	}
	if bs.Exists(rel) {
		t.Fatal("原图未被删除")
	}
	if bs.Exists(thumbRel) {
		t.Fatal("缩略图未被连带删除（会留下孤儿文件）")
	}
	// 再删一次不应报错
	if err := bs.Remove(rel); err != nil {
		t.Fatalf("重复删除应幂等：%v", err)
	}
}

func TestWriteFileAtomic_ReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.bin")
	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("second-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second-longer" {
		t.Fatalf("内容 = %q", got)
	}
}

func TestMakeThumbPNG_RejectsGarbage(t *testing.T) {
	if _, _, _, err := MakeThumbPNG([]byte("not a png"), 160); err == nil {
		t.Fatal("非 PNG 应报错")
	}
}

// ── 设置 ────────────────────────────────────────────────────────

func TestSettings_SeedDefaultsAndLoad(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	keys := SettingKeys()
	if len(keys) < 28 {
		t.Fatalf("设置键只有 %d 个，DESIGN.md §9 的表里应不少于 28 项", len(keys))
	}
	for _, k := range keys {
		if _, ok, err := db.GetRaw(ctx, k); err != nil || !ok {
			t.Errorf("键 %s 未被写入默认值（ok=%v err=%v）", k, ok, err)
		}
	}

	s, err := LoadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultSettings()
	if s.Capture.ImageMaxBytes != def.Capture.ImageMaxBytes ||
		s.Retention.DefaultTTLSec != def.Retention.DefaultTTLSec ||
		s.UI.QuickPasteCount != def.UI.QuickPasteCount ||
		s.Storage.WALCheckpointEvery != def.Storage.WALCheckpointEvery {
		t.Fatalf("默认值对不上：%+v", s)
	}
	if len(s.Capture.Types) != 3 || s.Capture.Types[0] != "text" {
		t.Fatalf("capture.types = %v", s.Capture.Types)
	}
	if len(s.Exclude.Apps) != 3 || s.Exclude.Apps[0] != "com.1password.*" {
		t.Fatalf("exclude.apps = %v", s.Exclude.Apps)
	}
}

func TestSettings_SeedIsIdempotentAndKeepsUserValues(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRaw(ctx, KeyRetentionMaxItems, "42"); err != nil {
		t.Fatal(err)
	}
	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if s.Retention.MaxItems != 42 {
		t.Fatalf("重新 Seed 覆盖了用户值：maxItems = %d", s.Retention.MaxItems)
	}
}

func TestSettings_BootstrapLanguageUsedOnlyWhenMissing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := SeedSettings(ctx, db, "zh-CN"); err != nil {
		t.Fatal(err)
	}
	s, _ := LoadSettings(ctx, db)
	if s.UI.Language != "zh-CN" {
		t.Fatalf("首次 Seed 应采用 TOML 的 ui.language，得到 %q", s.UI.Language)
	}

	// 用户改成 en 之后，再 Seed 不得回退到 TOML 值
	if err := db.SetRaw(ctx, KeyUILanguage, `"en"`); err != nil {
		t.Fatal(err)
	}
	if err := SeedSettings(ctx, db, "zh-CN"); err != nil {
		t.Fatal(err)
	}
	s, _ = LoadSettings(ctx, db)
	if s.UI.Language != "en" {
		t.Fatalf("settings 表必须是唯一真源，得到 %q", s.UI.Language)
	}
}

func TestSettings_SaveAndReload(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatal(err)
	}

	s, _ := LoadSettings(ctx, db)
	s.Capture.Enabled = false
	s.Capture.Types = []string{"text"}
	s.Retention.MaxItems = 7
	s.UI.Hotkey = "CmdOrCtrl+Alt+V"
	if err := SaveSettings(ctx, db, s); err != nil {
		t.Fatal(err)
	}

	back, err := LoadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if back.Capture.Enabled || len(back.Capture.Types) != 1 || back.Retention.MaxItems != 7 {
		t.Fatalf("往返后设置不一致：%+v", back)
	}
	if back.UI.Hotkey != "CmdOrCtrl+Alt+V" {
		t.Fatalf("hotkey = %q", back.UI.Hotkey)
	}
}

func TestSettings_CorruptRowFallsBackToDefault(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := SeedSettings(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRaw(ctx, KeyRetentionMaxItems, "{not json"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(ctx, db)
	if err != nil {
		t.Fatalf("一个坏行不该让加载失败: %v", err)
	}
	if s.Retention.MaxItems != DefaultSettings().Retention.MaxItems {
		t.Fatalf("坏行应回退默认值，得到 %d", s.Retention.MaxItems)
	}
}

func TestSettings_GetRawMissing(t *testing.T) {
	db := newTestDB(t)
	v, ok, err := db.GetRaw(context.Background(), "nope.nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok || v != "" {
		t.Fatalf("缺失键应返回 ok=false，得到 %q/%v", v, ok)
	}
}

func ptrInt64(v int64) *int64 { return &v }

// 编译期断言：*sql.Tx 与 *sql.DB 都满足 Execer。
var (
	_ Execer = (*sql.Tx)(nil)
	_ Execer = (*sql.DB)(nil)
)
