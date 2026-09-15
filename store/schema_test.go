package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpen_CreatesSchema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var ver int
	if err := db.Reader().QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	if ver != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", ver, SchemaVersion)
	}

	var mode string
	if err := db.Reader().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	for _, name := range []string{
		"items", "categories", "tags", "item_tags", "imports", "settings", "items_fts",
	} {
		var n int
		err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE name = ?", name).Scan(&n)
		if err != nil {
			t.Fatalf("查表 %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("表 %s 不存在", name)
		}
	}
	for _, name := range []string{"items_ai", "items_ad", "items_au"} {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name = ?", name).Scan(&n); err != nil {
			t.Fatalf("查触发器: %v", err)
		}
		if n != 1 {
			t.Errorf("触发器 %s 不存在", name)
		}
	}

	if !db.FTSAvailable() {
		t.Fatal("带 sqlite_fts5 标签构建时 FTS 应可用（测试必须用 -tags sqlite_fts5 跑）")
	}
	if note := db.IntegrityNote(); note != "" {
		t.Fatalf("首次创建库不应有完整性告警：%s", note)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")

	for i := 0; i < 3; i++ {
		db, err := Open(Options{Path: path, Logger: testLogger()})
		if err != nil {
			t.Fatalf("第 %d 次 Open: %v", i+1, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("第 %d 次 Close: %v", i+1, err)
		}
	}
}

func TestOpen_RejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")

	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(Options{Path: path, Logger: testLogger()})
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestOpen_MigratesFromZero(t *testing.T) {
	// 空文件（= user_version 0）应被正常迁移到 v1
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var ver int
	if err := db.Reader().QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", ver, SchemaVersion)
	}
}

func TestOpen_CreatesDataDirWith0700(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	db, err := Open(Options{Path: filepath.Join(nested, "pawclip.db"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("数据目录权限 = %o, want 700", perm)
	}
}

// ── clean_shutdown 标记 ─────────────────────────────────────────

func TestCleanShutdownMarker_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")
	marker := filepath.Join(dir, cleanShutdownMarker)

	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(marker) {
		t.Fatal("Open 之后必须写下 clean_shutdown 标记（进程活着就代表可能异常退出）")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if fileExists(marker) {
		t.Fatal("正常 Close 之后标记必须被删除")
	}
}

func TestCleanShutdownMarker_TriggersIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")
	marker := filepath.Join(dir, cleanShutdownMarker)

	// 先建好库
	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 模拟上次异常退出：库已存在但标记还在
	if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("带标记重开失败: %v", err)
	}
	defer db2.Close()
	if note := db2.IntegrityNote(); note != "" {
		t.Fatalf("健康库的完整性检查应通过，得到：%s", note)
	}
}

func TestSetCleanShutdownMarkerEnabled_False(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Path: filepath.Join(dir, "pawclip.db"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.SetCleanShutdownMarkerEnabled(false); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(dir, cleanShutdownMarker)) {
		t.Fatal("关闭该设置后标记必须被清掉，下次启动才不会做完整性检查")
	}
}

// ── FTS5 / 中文检索契约 ────────────────────────────────────────
//
// 这一组锁定的是 DESIGN.md §4.2 的两段式策略。M1 不实现检索，
// 但 schema 与分词器行为必须在 M2 动工前就被钉死，否则 M2 会返工。
func TestFTS5_TrigramBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seed := []string{
		"中文文档 hello world",
		"链接地址 https://example.com",
		"AI 与 js 的 5G 时代",
	}
	for i, s := range seed {
		if _, err := db.Writer().ExecContext(ctx,
			`INSERT INTO items (kind, text_content, preview, fingerprint, first_seen_at, created_at)
			 VALUES ('text', ?, ?, ?, ?, ?)`,
			s, s, "sha256:seed"+string(rune('a'+i)), 1, 1); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// ≥3 字符：trigram 命中
	for _, q := range []string{"中文文", "wor", "hello", "example"} {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM items_fts WHERE items_fts MATCH ?", q).Scan(&n); err != nil {
			t.Fatalf("MATCH %q: %v", q, err)
		}
		if n == 0 {
			t.Errorf("MATCH %q 应命中，实际 0 条", q)
		}
	}

	// 1–2 字符：trigram 必然 0 命中，且必须**不报错**（不报错才能安心走兜底）
	//
	// 注意 "5G 时"：整串 4 字符，但按空格切出来是 "5G" + "时"，两个 token 都短于
	// 3 字符 → trigram 一个 token 都产不出来 → 0 命中。也就是说"判据是整串长度"
	// 这个直觉是错的，M2 的兜底策略不能只看整串字符数，还得处理
	// "FTS 命中 0 条"的回落。这条已在下方 LIKE 组里验证。
	for _, q := range []string{"中文", "文档", "链接", "AI", "js", "5G", "5G 时"} {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM items_fts WHERE items_fts MATCH ?", q).Scan(&n); err != nil {
			t.Fatalf("MATCH %q 报错（不该）：%v", q, err)
		}
		if n != 0 {
			t.Logf("注意：MATCH %q 命中 %d 条（trigram 门槛可能变了）", q, n)
		}
	}

	// LIKE 兜底必须真的能捞到两字词
	for _, tc := range []struct {
		like string
		want int
	}{
		{"%文档%", 1},
		{"%链接%", 1},
		{"%AI%", 1},
		{"%5G%", 1},
		{"%不存在%", 0},
	} {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM items WHERE deleted_at IS NULL AND text_content LIKE ?",
			tc.like).Scan(&n); err != nil {
			t.Fatalf("LIKE %q: %v", tc.like, err)
		}
		if n != tc.want {
			t.Errorf("LIKE %q = %d, want %d", tc.like, n, tc.want)
		}
	}
}

func TestFTS5_TriggersKeepIndexInSync(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	insert := func(fp, text string) int64 {
		res, err := db.Writer().ExecContext(ctx,
			`INSERT INTO items (kind, text_content, preview, fingerprint, first_seen_at, created_at)
			 VALUES ('text', ?, ?, ?, 1, 1)`, text, text, fp)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	matchCount := func(q string) int {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM items_fts WHERE items_fts MATCH ?", q).Scan(&n); err != nil {
			t.Fatalf("MATCH %q: %v", q, err)
		}
		return n
	}

	id := insert("sha256:one", "独一无二的甲内容")
	if got := matchCount("独一无二"); got != 1 {
		t.Fatalf("插入后应命中 1 条，得到 %d", got)
	}

	// UPDATE 触发器必须先把旧内容从索引里删掉
	if _, err := db.Writer().ExecContext(ctx,
		"UPDATE items SET text_content = ?, preview = ? WHERE id = ?",
		"完全不同的乙内容", "完全不同的乙内容", id); err != nil {
		t.Fatal(err)
	}
	if got := matchCount("独一无二"); got != 0 {
		t.Errorf("更新后旧内容仍命中 %d 条", got)
	}
	if got := matchCount("完全不同"); got != 1 {
		t.Errorf("更新后新内容应命中 1 条，得到 %d", got)
	}

	// DELETE 触发器
	if _, err := db.Writer().ExecContext(ctx, "DELETE FROM items WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if got := matchCount("完全不同"); got != 0 {
		t.Errorf("删除后仍命中 %d 条", got)
	}
}

func TestFTS5_UpsertKeepsSingleIndexRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	it := sampleItem("sha256:dup", "重复内容 abcdef")

	for i := 0; i < 5; i++ {
		if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
			t.Fatalf("PutItem #%d: %v", i+1, err)
		}
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx,
		"SELECT count(*) FROM items_fts WHERE items_fts MATCH ?", "abcdef").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FTS 索引里出现了 %d 行，去重失效或触发器重复插入", n)
	}
}

func TestCheckpoint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO items (kind, text_content, preview, fingerprint, first_seen_at, created_at)
		 VALUES ('text','x','x','sha256:ck',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// wal 文件应被截断到接近 0
	wal := db.Path() + "-wal"
	if st, err := os.Stat(wal); err == nil && st.Size() > 0 {
		t.Logf("wal 截断后仍有 %d 字节（SQLite 允许，通常接近 0）", st.Size())
	}
}

func TestReaderIsQueryOnly(t *testing.T) {
	// 读句柄开了 query_only，误写必须立刻报错而不是悄悄绕开单写纪律
	db := newTestDB(t)
	_, err := db.Reader().ExecContext(context.Background(),
		`INSERT INTO items (kind, fingerprint, first_seen_at, created_at)
		 VALUES ('text','sha256:oops',1,1)`)
	if err == nil {
		t.Fatal("读句柄上的写入应当被拒绝")
	}
	t.Logf("读句柄写入被拒，错误信息：%v", err)
}
