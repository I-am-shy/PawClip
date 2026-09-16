package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 这个文件钉住检索的**执行计划**，而不只是它的耗时。
//
// 背景：store/query.go 里的 FTS 检索经历过一次改写（JOIN → IN 子查询 +
// INDEXED BY 时间索引），10 万条下稠密查询从 187~924 ms 降到 0.9~2.4 ms。
// 那次改写有两个容易被后人无意破坏的点：
//
//  1. **语义**：换写法时"结果集"必须一模一样。少一条、多一条、顺序变了，
//     都是 bug，而 latency_test.go 只看耗时与命中数，看不出这种差别。
//  2. **计划**：新写法快**只因为它走对了计划**。如果有人把 INDEXED BY 去掉
//     （看起来无害，甚至像"少了个硬编码更干净"），SQLite 会退回
//     "先物化命中集再排序"，于是又变成几百毫秒——而功能测试全绿。
//
// 所以这里用 EXPLAIN QUERY PLAN 直接断言计划本身。

// oldJoinSQL 是改写前的参考实现，**只在这里作为语义基准存在**。
//
// 保留它的理由：性能改写最容易出的错是"顺手改了语义"。有了一份
// 可执行的旧写法，等价性就不再靠人肉 diff SQL 来判断。
const oldJoinSQL = "SELECT i.id FROM items i" +
	" JOIN items_fts ON items_fts.rowid = i.id" +
	" WHERE i.deleted_at IS NULL AND items_fts MATCH ?" +
	" ORDER BY i.created_at DESC, i.id DESC LIMIT ?"

// TestSearchNewPlanKeepsJoinSemantics 断言新写法与旧写法返回完全相同的 id 序列。
//
// 特意同时覆盖"只看存活"与"只看回收站"两种过滤，因为新写法在回收站那条路上
// **不会**用 INDEXED BY（部分索引 idx_items_alive 的前提是 deleted_at IS NULL，
// 在回收站里指定它会直接报 no query solution）。两条路都要证明结果一致。
//
// ⚠️ 只拿 **≥3 字**的查询做对照，这是刻意的：§4.2 的两段式策略会让 1~2 字
// 查询走 LIKE 兜底（trigram 索引不了 3 字以下的片段），那时新写法与旧写法
// 用的根本不是同一条检索路径，拿 FTS 的旧写法当参照只会得到一个假的"不一致"。
// 2 字那一段由 TestSearchCorrectnessAtTwoChars 单独钉（见下）。
func TestSearchNewPlanKeepsJoinSemantics(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ids := seedCorpus(t, db) // fts_test.go 里的 7 条检索语料

	// 造几条回收站里的记录：验证 Trashed 分支。
	if _, err := db.SoftDelete(ctx, []int64{ids["a"], ids["b"]}); err != nil {
		t.Fatalf("软删除: %v", err)
	}
	requireFTS(t, db)

	// 存活区仍然有命中的查询（语料 a 项已被软删，所以不列它）。
	aliveQueries := []string{"hello", "Hello World", "utm_source", "100%", "完成度", "13800138000"}
	// 只存在于回收站里的查询：证明回收站那条路（不用部分索引）也能查出东西。
	// 都是语料第 a 项 "中文文档 链接 图片" 里**连续**的子串：
	// trigram 的短语查询要求连续，写成 "文档链接"（源文本中间有个空格）
	// 会一条都查不到 —— 那种"两个空集相等"的对照什么也证明不了。
	trashOnlyQueries := []string{"中文文档", "中文文"}

	check := func(text string, trashed bool, wantNonEmpty bool) {
		t.Helper()
		if m := db.matchMode(text); m != SearchModeFTS {
			t.Fatalf("查询 %q 走的是 %s —— 这条测试假设 ≥3 字走 FTS，"+
				"前提变了就要重新审视整组对照", text, m)
		}
		pg, err := db.List(ctx, Query{Text: text, Trashed: trashed, Limit: 50})
		if err != nil {
			t.Fatalf("List(%q, trashed=%v): %v", text, trashed, err)
		}
		got := make([]int64, 0, len(pg.Rows))
		for _, r := range pg.Rows {
			got = append(got, r.ID)
		}
		want := refJoinIDs(t, db, text, trashed)
		if !sameIDs(got, want) {
			t.Errorf("查询 %q（trashed=%v）结果与旧写法不一致：\n  新: %v\n  旧: %v",
				text, trashed, got, want)
		}
		// 至少要有一条命中，否则"两边都是空"这种一致毫无信息量。
		if wantNonEmpty && len(want) == 0 {
			t.Errorf("查询 %q（trashed=%v）一条都没命中 —— 对照失去了意义", text, trashed)
		}
	}

	for _, text := range aliveQueries {
		check(text, false, true) // 存活区：必须有命中
		check(text, true, false) // 回收站：命中与否都算，比的是两者一致
	}
	for _, text := range trashOnlyQueries {
		check(text, true, true) // 只该在回收站里命中
	}
}

// TestSearchCorrectnessAtTwoChars 把 2 字那一段也钉住，并明确它的路径。
//
// 为什么单独一条：上面那条对照只覆盖 FTS 路径，而 §4.2 说的"两段式"里
// 2 字是**另一段**（LIKE 兜底）。这一段最容易在改动 query.go 时被顺手破坏
// ——比如有人把 matchMode 的阈值改成 3 字以上才走 FTS 就以为统一了，
// 却让"搜『文档』搜不到"变成常态。
func TestSearchCorrectnessAtTwoChars(t *testing.T) {
	db := newTestDB(t)
	ids := seedCorpus(t, db)
	requireFTS(t, db)

	cases := []struct {
		text string
		want int64
	}{
		{"文档", ids["a"]}, // 出现在 "中文文档 链接 图片"
		{"链接", ids["a"]},
		{"5G", ids["b"]}, // 纯 ASCII 2 字
		{"报告", ids["d"]}, // 出现在 "今天天气不错，报告在这里"
	}
	for _, c := range cases {
		if m := db.matchMode(c.text); m != SearchModeLike {
			t.Errorf("2 字查询 %q 期望走 LIKE 兜底，实际 %s", c.text, m)
		}
		got := searchIDs(t, db, c.text)
		if !containsID(got, c.want) {
			t.Errorf("2 字查询 %q 没命中第 %d 条（得到 %v）", c.text, c.want, got)
		}
	}
}

// refJoinIDs 用旧写法跑一遍，返回 id 列表。
func refJoinIDs(t *testing.T, db *DB, text string, trashed bool) []int64 {
	t.Helper()
	ctx := context.Background()

	sql := oldJoinSQL
	where := "i.deleted_at IS NULL"
	if trashed {
		// 回收站视图：把存活条件换掉，其余（FTS 命中 + 排序）保持不变。
		sql = strings.Replace(sql, where, "i.deleted_at IS NOT NULL", 1)
	}
	rows, err := db.r.QueryContext(ctx, sql, FTSPhrase(text), 51)
	if err != nil {
		t.Fatalf("参考查询: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameIDs(a, b []int64) bool {
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

// seedDenseMatches 播 n 条都含同一个词的记录，用来造出"命中很多"的场景。
//
// 为什么需要它：执行计划是按**命中密度**选的（见 query.go 的 chooseFTSPlan），
// 而 fts_test.go 那 7 条语料永远造不出稠密命中——用它去断言"要走时间索引"
// 只会得到一个假的失败。密度是这条测试的**输入条件**，必须显式准备。
func seedDenseMatches(t *testing.T, db *DB, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		it := sampleItem(fmt.Sprintf("sha256:dense-%d", i),
			fmt.Sprintf("剪贴板历史管理工具的稠密命中样本 %d", i))
		it.CreatedAt = int64(1_700_000_000 + i)
		it.FirstSeenAt = it.CreatedAt
		if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
			t.Fatalf("播种第 %d 条: %v", i, err)
		}
	}
}

// TestSearchFTSPlanIsChosenByDensity 断言执行计划**跟着命中密度走**。
//
// 这条测试是两个方向的：
//
//	稠密 → 必须按时间索引驱动（提前终止，与命中总数无关）
//	稀疏 → 必须走物化排序（回表次数正比于命中数，远少于扫完整张索引）
//
// 只断言其中一个方向是不够的：把 chooseFTSPlan 写成恒返回某一个值，
// 单向断言照样能过，而另一端的延迟会悄悄恶化到几百毫秒。
func TestSearchFTSPlanIsChosenByDensity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seedCorpus(t, db) // 7 条：任何查询都是"稀疏"
	requireFTS(t, db)

	const sparseQuery = "100%" // 只命中语料第 g 项

	// ── 稀疏端 ──
	if got := db.chooseFTSPlan(ctx, Query{Text: sparseQuery}, SearchModeFTS); got != ftsPlanMaterialize {
		t.Errorf("命中只有 1 条的查询选了 %v，期望 ftsPlanMaterialize\n"+
			"    （时间序扫描在稀疏时要走完整张索引，10 万条实测 27~66 ms）", got)
	}
	sparsePlan := explainListPlan(t, db, Query{Text: sparseQuery, Limit: 50})
	if strings.Contains(sparsePlan, "idx_items_alive") {
		t.Errorf("稀疏查询的计划里出现了 idx_items_alive，说明还是按时间索引扫了：\n    %s", sparsePlan)
	}

	// ── 稠密端 ──
	seedDenseMatches(t, db, ftsScanThreshold+300)
	requireFTS(t, db)

	const denseQuery = "剪贴板历史管理工具的稠密命中样本"

	if got := db.chooseFTSPlan(ctx, Query{Text: denseQuery}, SearchModeFTS); got != ftsPlanTimeIndex {
		t.Errorf("命中 %d 条的查询选了 %v，期望 ftsPlanTimeIndex\n"+
			"    （物化排序在稠密时要回表这么多次再排序，10 万条实测几百毫秒）",
			ftsScanThreshold+300, got)
	}
	densePlan := explainListPlan(t, db, Query{Text: denseQuery, Limit: 50})
	if !strings.Contains(densePlan, "idx_items_alive") {
		t.Errorf("稠密查询没有走时间索引，计划是：\n    %s\n"+
			"    说明 SQLite 退回了「先物化命中集再排序」——\n"+
			"    10 万条下这条路要几百毫秒（§12 的预算是 50 ms）。\n"+
			"    检查 store/query.go 里 IN(子查询) 与 chooseFTSPlan 那两处。", densePlan)
	}
	// 精确断言**驱动表是 items**，而不是笼统地"计划里不出现 items_fts"。
	//
	// 后者是错的：命中集那条子查询本来就要扫 items_fts
	// （计划里的 "LIST SUBQUERY 1 | SCAN items_fts"），它出现是正常的。
	// 要害在于**谁是驱动表**——由 FTS 驱动就回到了老路子（无上界的排序），
	// 由 items 的时间索引驱动才能提前终止。
	if !strings.HasPrefix(densePlan, "SCAN i USING INDEX idx_items_alive") {
		t.Errorf("稠密查询的驱动表不是 items 的时间索引，计划是：\n    %s\n"+
			"    期望以 `SCAN i USING INDEX idx_items_alive` 开头。", densePlan)
	}
}

// TestSearchTrashPlanDoesNotForcePartialIndex 断言回收站检索**不**指定
// 那个部分索引。
//
// 这是一条"反向"断言，防的是过度优化：idx_items_alive 的前提是
// `deleted_at IS NULL`，回收站查的是 IS NOT NULL。一旦有人把 INDEXED BY
// 挪到公共路径上，这条查询会直接报 no query solution —— 但那时错误发生在
// 运行期（用户点开回收站并搜索），不是编译期。这条测试把它提前到提交时。
func TestSearchTrashPlanDoesNotForcePartialIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ids := seedCorpus(t, db)
	if _, err := db.SoftDelete(ctx, []int64{ids["a"], ids["b"]}); err != nil {
		t.Fatal(err)
	}
	requireFTS(t, db)

	// 能把计划算出来，本身就证明没有对回收站查询指定部分索引。
	plan := explainListPlan(t, db, Query{Text: "文档", Trashed: true, Limit: 50})
	t.Logf("回收站检索计划：%s", plan)

	// 而且结果得是非空的——空结果的"计划正确"没有意义。
	pg, err := db.List(ctx, Query{Text: "文档", Trashed: true, Limit: 50})
	if err != nil {
		t.Fatalf("回收站检索: %v", err)
	}
	if len(pg.Rows) == 0 {
		t.Error("回收站里搜「文档」一条都没命中，这条测试失去了意义")
	}
}

// explainListPlan 取出 List 在给定查询下实际使用的执行计划。
//
// 直接沿用 listSQL（runQuery 用的就是它），而不是在测试里另写一份等价 SQL：
// 另写一份的话，两者一旦漂移，这条测试就在验证一条**没人执行的语句**，
// 比没有更糟——它会给出一个"计划正确"的假保证。
func explainListPlan(t *testing.T, db *DB, q Query) string {
	t.Helper()
	q = q.withDefaults()

	sqlText, args := db.listSQL(context.Background(), q, db.matchMode(q.Text))
	rows, err := db.r.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN 失败：%v\n    SQL: %s", err, sqlText)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(parts) == 0 {
		t.Fatal("执行计划是空的 —— 这条测试失去了意义")
	}
	return strings.Join(parts, " | ")
}
