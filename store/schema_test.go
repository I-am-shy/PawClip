package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// 这一组锁定的是 docs/DESIGN.md §4.2 的两段式策略。M1 不实现检索，
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

// TestConnectionPragmasAreActuallyApplied 是 DSN 参数方案的回归测试。
//
// 背景：连接级 PRAGMA 原来挂在驱动 ConnectHook 上（`c.Exec("PRAGMA …")`），
// 而 *sqlite3.SQLiteConn 在 `CGO_ENABLED=0` 下是个没有 Exec 的空桩，
// 于是那一行让整个 store 包无法跨平台交叉编译。改成 DSN 参数后编译问题
// 消失了，但引出一个新的、更隐蔽的风险：**DSN 键名写错不会报错，
// 只是不生效**。
//
// 所以这里直接断言"读回来是对的"，而不是断言"我们写了这个参数"。
func TestConnectionPragmasAreActuallyApplied(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if note := db.PragmaNote(); note != "" {
		t.Fatalf("启动自检报告 PRAGMA 未生效：%s", note)
	}

	read := func(h *sql.DB, pragma string) string {
		t.Helper()
		var v string
		if err := h.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
			t.Fatalf("读 PRAGMA %s: %v", pragma, err)
		}
		return v
	}

	for _, c := range []struct{ pragma, want string }{
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // 1 = NORMAL
	} {
		if got := read(db.Writer(), c.pragma); got != c.want {
			t.Errorf("写句柄 PRAGMA %s = %s，期望 %s", c.pragma, got, c.want)
		}
	}
	// 读池里的**每一条**连接都得有 query_only，不能只有第一条。
	// 这里串行地要 4 次连接（MaxOpenConns=4），逐个确认。
	for i := 0; i < 4; i++ {
		if got := read(db.Reader(), "query_only"); got != "1" {
			t.Fatalf("读句柄第 %d 条连接的 query_only = %s，期望 1（DSN 参数没打到新连接上）", i+1, got)
		}
	}
}

// TestForeignKeysCascadeActuallyWorks 验证外键**真的**在级联。
//
// foreign_keys=OFF 是那种"任何测试都不会变红"的静默失效：删掉一个分类后
// items.category_id 会指向一个不存在的分类，界面表现为"分类下的条目数
// 与实际不符"。所以不能只查 PRAGMA 的值，要真的删一次看结果。
func TestForeignKeysCascadeActuallyWorks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO categories (id, name, sort_order, created_at) VALUES (9, '级联测试', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO items (kind, fingerprint, category_id, first_seen_at, created_at)
		 VALUES ('text','sha256:fk1',9,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM categories WHERE id = 9`); err != nil {
		t.Fatal(err)
	}

	// 条目本身不该被删（那是 ON DELETE SET NULL），但 category_id 必须归 NULL。
	var cat sql.NullInt64
	if err := db.Writer().QueryRowContext(ctx,
		`SELECT category_id FROM items WHERE fingerprint = 'sha256:fk1'`).Scan(&cat); err != nil {
		t.Fatalf("条目被一起删掉了？%v", err)
	}
	if cat.Valid {
		t.Fatalf("分类已删除，条目的 category_id 仍是 %d —— 外键级联没生效（foreign_keys=OFF）", cat.Int64)
	}
}

// TestDSNWithKeepsExistingQueryString 覆盖"路径里已经带查询串"的情况。
//
// 直接再拼一个 '?' 的话，SQLite 会把第一个 '?' 之后的整段当成文件名，
// 结果是打开一个叫 "db.sqlite?_foreign_keys=on&_busy_timeout=5000" 的
// **新文件**——看起来一切正常，其实数据写进了另一个库。
func TestDSNWithKeepsExistingQueryString(t *testing.T) {
	got := dsnWith("/tmp/x.db?_txlock=immediate", true)
	if strings.Count(got, "?") != 1 {
		t.Fatalf("DSN 里出现了 %d 个 '?'（应当只有 1 个）：%s", strings.Count(got, "?"), got)
	}
	if !strings.HasPrefix(got, "/tmp/x.db?_txlock=immediate&") {
		t.Fatalf("原有查询串没被保留：%s", got)
	}
	for _, want := range []string{"_foreign_keys=on", "_busy_timeout=5000", "_synchronous=NORMAL", "_query_only=on"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN 缺参数 %s：%s", want, got)
		}
	}
	// 写句柄不该带 query_only。
	if w := dsnWith("/tmp/x.db", false); strings.Contains(w, "query_only") {
		t.Errorf("写句柄的 DSN 不该带 query_only：%s", w)
	}
}

// TestFTSMirrorsWholeItemsTableNotJustAlive 钉住一个反直觉的不变量。
//
// external content FTS 表 + 软删除的组合有个容易误解的地方：
//
//	软删除一条 → items 行数不变、存活数 -1、**FTS 行数不变**
//
// 因为软删除走的是 UPDATE，而 items_au 触发器会把那一行"先删后插"回索引。
// 也就是说 FTS 镜像的是**整张 items 表**，不是"存活条目"。
//
// 为什么值得专门钉一条测试：backup 的导入收尾会拿 FTS 行数与数据库对账，
// 第一版拿它比 CountAlive，在 overwrite 策略（先软删旧行再插新行）下
// 必然误报——而误报会把真正的不一致淹掉。
//
// 顺带在这个不变量之上验一件更要紧的事：**软删除的条目查不出来**。
// 索引里留着它是无害的，前提是查询侧带 deleted_at IS NULL ——
// 这一条如果哪天被改坏，用户会在历史列表里看到自己删掉的东西。
func TestFTSMirrorsWholeItemsTableNotJustAlive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	put := func(fp, text string) int64 {
		t.Helper()
		res, err := db.Writer().ExecContext(ctx,
			`INSERT INTO items (kind, text_content, preview, fingerprint, byte_size, first_seen_at, created_at)
			 VALUES ('text', ?, ?, ?, 1, 1, 1)`, text, text, fp)
		if err != nil {
			t.Fatalf("插入 %s: %v", fp, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	id1 := put("sha256:fts1", "一二三 甲乙丙")
	_ = put("sha256:fts2", "四五六 丁戊己")

	assertCounts := func(step string, wantItems, wantAlive, wantFTS int64) {
		t.Helper()
		items, err := db.CountAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		alive, err := db.CountAlive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fts, err := db.FTSRowCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if items != wantItems || alive != wantAlive || fts != wantFTS {
			t.Errorf("%s：items=%d alive=%d fts=%d，想要 %d/%d/%d",
				step, items, alive, fts, wantItems, wantAlive, wantFTS)
		}
	}
	assertCounts("插入两条后", 2, 2, 2)

	// 软删除一条：FTS 行数**不该**跟着少。
	if _, err := db.SoftDelete(ctx, []int64{id1}); err != nil {
		t.Fatal(err)
	}
	assertCounts("软删除一条后", 2, 1, 2)

	// 真正的物理删除才该让 FTS 行数下降。
	if _, err := db.Purge(ctx, []int64{id1}); err != nil {
		t.Fatal(err)
	}
	assertCounts("物理删除一条后", 1, 1, 1)

	// 物理删除之后，索引里真的搜不到那条了。
	pg, err := db.List(ctx, Query{Text: "一二三"})
	if err != nil {
		t.Fatalf("检索: %v", err)
	}
	if len(pg.Rows) != 0 {
		t.Errorf("物理删除后仍能搜到 %d 条（FTS 的 delete 命令没生效）", len(pg.Rows))
	}

	// 而软删除期间也该搜不到——这条断言的实现依赖查询侧的 deleted_at 过滤。
	_ = put("sha256:fts3", "七八九 庚辛壬")
	id3, err := db.GetByFingerprint(ctx, "sha256:fts3")
	if err != nil {
		t.Fatalf("取 fts3: %v", err)
	}
	if _, err := db.SoftDelete(ctx, []int64{id3.ID}); err != nil {
		t.Fatal(err)
	}
	pg, err = db.List(ctx, Query{Text: "七八九"})
	if err != nil {
		t.Fatalf("检索: %v", err)
	}
	if len(pg.Rows) != 0 {
		t.Errorf("软删除的条目被搜出来了 %d 条——查询侧丢了 deleted_at IS NULL 过滤"+
			"（FTS 索引里留着软删除的行是正常的，但查询必须挡住）", len(pg.Rows))
	}
}
