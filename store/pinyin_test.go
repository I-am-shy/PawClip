package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // 只为在"手工造 v1 老库"时直接开原始连接

	"github.com/zego/pawclip/pinyin"
)

// 本文件覆盖两件与"拼音首字母检索"有关的事：
//
//  1. 检索本身能用（FTS 路径与 LIKE 兜底各一条）；
//  2. **v1 老库升到 v2 的升级路径**。第二条是重点：它是真实用户会遇到、
//     而单元测试最容易漏的那条路 —— 现有用例全都从空库开始建，
//     根本走不到 ALTER + 重建 FTS + 回填这条链。

// putText 往库里塞一条文本条目并返回 id。
func putText(t *testing.T, db *DB, fp, text string) int64 {
	t.Helper()
	it := sampleItem(fp, text)
	res, err := PutItem(context.Background(), db.Writer(), &it)
	if err != nil {
		t.Fatalf("PutItem(%q): %v", text, err)
	}
	return res.ID
}

func searchIDs(t *testing.T, db *DB, text string) []int64 {
	t.Helper()
	pg, err := db.List(context.Background(), Query{Text: text})
	if err != nil {
		t.Fatalf("List(%q): %v", text, err)
	}
	ids := make([]int64, 0, len(pg.Rows))
	for _, r := range pg.Rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSearch_PinyinInitials 是 P2 那条功能的正面用例（走 FTS / trigram）。
//
// "剪贴板历史管理" → "jtblsgl"，用户打 jtb 应当命中。
func TestSearch_PinyinInitials(t *testing.T) {
	db := newTestDB(t)

	idClip := putText(t, db, "sha256:p1", "剪贴板历史管理")
	idOther := putText(t, db, "sha256:p2", "导出为 clipbak 文件")
	// 中英混排。这条是**唯一能证伪"pinyin 列没进检索路径"的用例**：
	// 原文 "ZIM 文档" 里 zim 与 文档 之间有空格，所以 "zimwd" 这个串
	// 只可能出现在 pinyin 列（"zimwd"）里，不可能出现在 text_content /
	// preview（"zim 文档"）里。filters 里那三处 `i.pinyin LIKE ?`、
	// 以及 FTS 第三列，任一缺失都会让它变红。
	//
	// 曾经这里写的是 {"gha", "GitHub Actions workflow"}，那是错的：
	// pinyin 包钉死的契约是"ASCII 原样保留"（Initials("Hello 世界") ==
	// "hellosj"），所以它的 pinyin 是 "githubactionsworkflow"，
	// 里面没有 "gha" 这个子串；而 "git"/"hub" 这类查询本来就能被
	// FTS 从正文里命中，测不出 pinyin 的存在。
	idMixed := putText(t, db, "sha256:p3", "ZIM 文档")

	cases := []struct {
		query string
		want  int64
		why   string
	}{
		{"jtb", idClip, "三个首字母"},
		{"jtbls", idClip, "前缀延长"},
		{"lsgl", idClip, "中间片段——子串匹配，不要求从头"},
		{"dcw", idOther, "另一个条目的首字母"},
		{"zimwd", idMixed, "中英混排：跨过原文空格，只有 pinyin 列能命中"},
	}
	for _, c := range cases {
		got := searchIDs(t, db, c.query)
		if !containsID(got, c.want) {
			t.Errorf("搜 %q（%s）没命中 id=%d，实际命中 %v", c.query, c.why, c.want, got)
		}
	}

	// 不能是"什么都命中"：用一个不存在的首字母串验证。
	if got := searchIDs(t, db, "zzzz"); len(got) != 0 {
		t.Errorf("搜 zzzz 命中 %v，想要空（否则说明匹配逻辑退化成恒真）", got)
	}
}

// TestSearch_PinyinViaLikeFallback 覆盖 1–2 个字符的查询。
//
// 这类查询进不了 trigram（最少 3 字符），走的是 LIKE 兜底 —— 所以
// buildFilters 里那三处 `i.pinyin LIKE ?` 必须真的生效，否则
// "打两个首字母"这个最常见的用法会静默失效。
func TestSearch_PinyinViaLikeFallback(t *testing.T) {
	db := newTestDB(t)
	id := putText(t, db, "sha256:p4", "文档管理")

	// 先确认它确实走 LIKE 而不是 FTS：2 个字符够不到 trigram 门槛。
	pg, err := db.List(context.Background(), Query{Text: "wd"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if pg.Mode != SearchModeLike {
		t.Fatalf("2 字查询走到了 %q 路径，想要 %q（否则这条用例没在测兜底）", pg.Mode, SearchModeLike)
	}
	if !containsID([]int64{pg.Rows[0].ID}, id) && len(pg.Rows) == 0 {
		t.Fatalf("搜 wd 未命中，实际 %d 行", len(pg.Rows))
	}

	// 单字符也要能命中（用户边打边搜的第一下）。
	if got := searchIDs(t, db, "w"); !containsID(got, id) {
		t.Errorf("搜单字符 w 没命中 id=%d，实际 %v", id, got)
	}
}

// TestPinyinColumnIsFilledOnInsert 确认 pinyin 由存储层自己填。
//
// 这条防的是"有人把 itemPinyin 从 PutItem 里挪走、改由调用方传"：
// 那样只要有一个调用点忘了传，那条内容就永远搜不到 —— 而不会有测试变红。
func TestPinyinColumnIsFilledOnInsert(t *testing.T) {
	db := newTestDB(t)
	id := putText(t, db, "sha256:p5", "剪贴板")

	var py sql.NullString
	if err := db.Reader().QueryRow(`SELECT pinyin FROM items WHERE id = ?`, id).Scan(&py); err != nil {
		t.Fatalf("读 pinyin: %v", err)
	}
	if !py.Valid {
		t.Fatal("pinyin 是 NULL —— PutItem 没有写这一列")
	}
	if py.String != "jtb" {
		t.Errorf("pinyin = %q，想要 \"jtb\"", py.String)
	}

	// 纯标点的条目：应当写成**空串**而不是 NULL。
	// 这个区别有实际后果：NULL 表示"还没算过"，回填逻辑会反复扫到它；
	// 空串表示"算过，但没有可提取的读音"，回填会跳过。
	idPunct := putText(t, db, "sha256:p6", "！！！")
	py = sql.NullString{}
	if err := db.Reader().QueryRow(`SELECT pinyin FROM items WHERE id = ?`, idPunct).Scan(&py); err != nil {
		t.Fatalf("读 pinyin: %v", err)
	}
	if !py.Valid || py.String != "" {
		t.Errorf("纯标点条目的 pinyin = %+v，想要已设置的空串（不是 NULL）", py)
	}

	// 回填函数跑完之后不该再找到任何待处理行。
	ids, _, err := db.pendingPinyin(100)
	if err != nil {
		t.Fatalf("pendingPinyin: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("还有 %d 行 pinyin 为 NULL，回填没有覆盖到新插入的条目", len(ids))
	}
}

// TestMigrationV1ToV2_UpgradesRealisticOldDatabase 是本次改动里最要紧的一条。
//
// 它手工造一个**真正的 v1 库**（两列 FTS + 旧触发器 + 已有的中文条目），
// 然后用正常的 Open() 打开它，验证升级路径：
//
//	ALTER 加列 → 拆旧 FTS → 重建三列 FTS → rebuild → 回填 pinyin
//
// 为什么必须单独测：所有既有用例都从**空库**开始，走的是"直接建 v2"的路径，
// 完全绕过了迁移。而真实用户手上是 v1 库。这条链上任何一处漏掉，
// 症状都是"升级后老内容用首字母搜不到"或"升级后一条都记不下来"，
// 且都不会让现有测试变红。
func TestMigrationV1ToV2_UpgradesRealisticOldDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")

	// ── 第一步：手工造一个 v1 库 ────────────────────────────────
	// 不能调 Open()（那样会一路升到 v2），所以这里用原生 sqlite3 连接，
	// 照抄 v1 的 DDL 与当时的 FTS 定义。
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("打开原始库: %v", err)
	}
	v1Setup := []string{
		ddlV1,
		`PRAGMA user_version = 1`,
		// v1 的两列 FTS（没有 pinyin）—— 这正是升级要处理掉的东西。
		`CREATE VIRTUAL TABLE items_fts USING fts5(
		   text_content, preview,
		   content='items', content_rowid='id', tokenize='trigram');`,
		`CREATE TRIGGER items_ai AFTER INSERT ON items BEGIN
		   INSERT INTO items_fts(rowid, text_content, preview) VALUES (new.id, new.text_content, new.preview);
		 END;`,
		`CREATE TRIGGER items_ad AFTER DELETE ON items BEGIN
		   INSERT INTO items_fts(items_fts, rowid, text_content, preview) VALUES('delete', old.id, old.text_content, old.preview);
		 END;`,
		`CREATE TRIGGER items_au AFTER UPDATE ON items BEGIN
		   INSERT INTO items_fts(items_fts, rowid, text_content, preview) VALUES('delete', old.id, old.text_content, old.preview);
		   INSERT INTO items_fts(rowid, text_content, preview) VALUES (new.id, new.text_content, new.preview);
		 END;`,
		// 三条 v1 时期就已经存在的条目。pinyin 列还不存在。
		`INSERT INTO items (kind, text_content, preview, fingerprint, byte_size, first_seen_at, created_at)
		 VALUES ('text','剪贴板历史管理','剪贴板历史管理','sha256:old1',8,1,1)`,
		`INSERT INTO items (kind, text_content, preview, fingerprint, byte_size, first_seen_at, created_at)
		 VALUES ('text','文档管理界面','文档管理界面','sha256:old2',6,1,2)`,
		`INSERT INTO items (kind, text_content, preview, fingerprint, byte_size, first_seen_at, created_at)
		 VALUES ('text','!!!','!!!','sha256:old3',3,1,3)`,
		`INSERT INTO items_fts(items_fts) VALUES('rebuild')`,
	}
	for _, stmt := range v1Setup {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("造 v1 库失败于：%.60s…\n%v", stmt, err)
		}
	}
	// 自检：确认造的确实是"两列 FTS 的 v1 库"，否则下面断言的就不是升级路径。
	var ncol int
	if err := raw.QueryRow(`SELECT count(*) FROM pragma_table_info('items_fts')`).Scan(&ncol); err != nil {
		raw.Close()
		t.Fatalf("读 items_fts 列数: %v", err)
	}
	if ncol != 2 {
		raw.Close()
		t.Fatalf("手工造的库有 %d 列 FTS，想要 2 列（否则没在测升级）", ncol)
	}
	var ver int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		raw.Close()
		t.Fatalf("读 user_version: %v", err)
	}
	if ver != 1 {
		raw.Close()
		t.Fatalf("手工造的库 user_version = %d，想要 1", ver)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭原始库: %v", err)
	}

	// ── 第二步：用正常路径打开，触发升级 ────────────────────────
	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open（升级）失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 版本号必须已经推进到 2。
	if err := db.Reader().QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	if ver != SchemaVersion {
		t.Errorf("升级后 user_version = %d，想要 %d", ver, SchemaVersion)
	}

	// FTS 必须是三列版本 —— 否则触发器会往不存在的列里写，
	// 而那个错误只在**下一次插入**时才暴露（这里先一步断言）。
	if err := db.Reader().QueryRow(`SELECT count(*) FROM pragma_table_info('items_fts')`).Scan(&ncol); err != nil {
		t.Fatalf("读 items_fts 列数: %v", err)
	}
	if ncol != 3 {
		t.Errorf("升级后 items_fts 有 %d 列，想要 3（说明旧表没被拆掉）", ncol)
	}

	// pinyin 列必须存在，且老行必须已经被回填。
	ids, previews, err := db.pendingPinyin(100)
	if err != nil {
		t.Fatalf("pendingPinyin: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("升级后仍有 %d 行 pinyin 为 NULL（回填没跑或没跑完）：%v / %v", len(ids), ids, previews)
	}

	// 老内容必须能用首字母搜到（这是用户能感知到的唯一验收）。
	var old1 int64
	if err := db.Reader().QueryRow(`SELECT id FROM items WHERE fingerprint = 'sha256:old1'`).Scan(&old1); err != nil {
		t.Fatalf("取老条目 id: %v", err)
	}
	if got := searchIDs(t, db, "jtb"); !containsID(got, old1) {
		t.Errorf("升级后搜 jtb 没命中老条目 id=%d，实际命中 %v", old1, got)
	}
	// 汉字检索也不能因为重建索引而失效。
	if got := searchIDs(t, db, "文档管理"); len(got) == 0 {
		t.Errorf("升级后汉字检索失效（搜\"文档管理\"命中 0 条）—— rebuild 没成功")
	}

	// ── 第三步：升级后的库必须还能正常写入 ─────────────────────
	// 这条专抓"触发器还指向旧的两列 FTS"这种情况：那时插入会直接报错。
	newID := putText(t, db, "sha256:new1", "升级之后新增的内容")
	if newID == 0 {
		t.Fatal("升级后 PutItem 返回 id=0")
	}
	var py sql.NullString
	if err := db.Reader().QueryRow(`SELECT pinyin FROM items WHERE id = ?`, newID).Scan(&py); err != nil {
		t.Fatalf("读新条目 pinyin: %v", err)
	}
	if py.String != "sjzhxzdnr" {
		// 升(s) 级(j) 之(z) 后(h) 新(x) 增(z) 的(d) 内(n) 容(r)
		t.Errorf("新条目 pinyin = %q，想要 \"sjzhxzdnr\"", py.String)
	}
	if got := searchIDs(t, db, "sjzh"); !containsID(got, newID) {
		t.Errorf("升级后新增的条目用首字母搜不到：搜 sjzh 命中 %v，想要含 %d", got, newID)
	}
}

// TestMigrationIsIdempotent 重复打开不应重复迁移。
//
// 每次 Open 都会走 migrate()，user_version 已经是 2 时必须原地返回；
// 否则 ALTER TABLE ADD COLUMN 会在第二次打开时报 "duplicate column name"，
// 表现为"重启一次就再也开不起库"。
func TestMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pawclip.db")

	for i := 1; i <= 3; i++ {
		db, err := Open(Options{Path: path, Logger: testLogger()})
		if err != nil {
			t.Fatalf("第 %d 次 Open 失败：%v", i, err)
		}
		if i == 1 {
			putText(t, db, "sha256:idem", "幂等测试内容")
		}
		if err := db.Close(); err != nil {
			t.Fatalf("第 %d 次 Close: %v", i, err)
		}
	}

	db, err := Open(Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("最终 Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var n int64
	if err := db.Reader().QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("计数: %v", err)
	}
	if n != 1 {
		t.Errorf("条目数 = %d，想要 1（重复迁移可能把表清掉了？）", n)
	}
	// 幂等性最终要落到"用户能感知的行为还对不对"上：重启三次之后，
	// 拼音检索必须照旧。先钉住列值本身，再用它去查——这样万一期望串写错，
	// 报错会直接指向列值，而不是伪装成"检索坏了"。
	const text = "幂等测试内容"
	wantPinyin := pinyin.Initials(text) // 幂m 等d 测c 试s 内n 容r
	var gotPinyin string
	if err := db.Reader().QueryRow(`SELECT pinyin FROM items`).Scan(&gotPinyin); err != nil {
		t.Fatalf("读 pinyin: %v", err)
	}
	if gotPinyin != wantPinyin {
		t.Fatalf("pinyin = %q，想要 %q（先修这里，再看检索）", gotPinyin, wantPinyin)
	}

	got := searchIDs(t, db, "mdcs")
	if len(got) != 1 {
		t.Errorf("重复打开后拼音检索失效：搜 mdcs 命中 %v，想要恰好 1 条", got)
	}
	// 2 个字符走 LIKE 兜底，同样不能被重复迁移弄坏。
	if got := searchIDs(t, db, "md"); len(got) != 1 {
		t.Errorf("重复打开后短查询（LIKE 路径）失效：搜 md 命中 %v，想要恰好 1 条", got)
	}
}

// TestMigrationV1ToV2_BackfillInBatches 覆盖回填的分批循环。
//
// 每批 500 条，所以造 1200 条能确保至少走三轮循环。
// 不测这个边界的话，"只处理了第一批"这种 bug 会漏过去 —— 症状是
// "库里超过 500 条之后，只有前 500 条能用首字母搜到"。
func TestMigrationV1ToV2_BackfillInBatches(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const total = 1200
	// 直接走 SQL 插入并把 pinyin 留成 NULL，模拟"老库的行"。
	tx, err := db.Writer().Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := 0; i < total; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO items (kind, text_content, preview, fingerprint, byte_size, first_seen_at, created_at)
			 VALUES ('text', ?, ?, ?, 4, 1, ?)`,
			"文档管理", "文档管理", fmt.Sprintf("sha256:batch%04d", i), int64(i+1)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("插入第 %d 条: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// 此时全部是 NULL。
	ids, _, err := db.pendingPinyin(10000)
	if err != nil {
		t.Fatalf("pendingPinyin: %v", err)
	}
	if len(ids) != total {
		t.Fatalf("待回填 %d 行，想要 %d", len(ids), total)
	}

	if err := db.ensurePinyin(); err != nil {
		t.Fatalf("ensurePinyin: %v", err)
	}

	ids, _, err = db.pendingPinyin(10000)
	if err != nil {
		t.Fatalf("pendingPinyin: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("回填后仍有 %d 行未处理（分批循环提前退出了？）", len(ids))
	}

	var filled int64
	if err := db.Reader().QueryRow(`SELECT count(*) FROM items WHERE pinyin = 'wdgl'`).Scan(&filled); err != nil {
		t.Fatalf("计数: %v", err)
	}
	if filled != total {
		t.Errorf("有正确拼音的行数 = %d，想要 %d", filled, total)
	}
}
