package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// 这个文件是 DESIGN §12「检索延迟」那一行的取证。
//
//	| 检索延迟 | 3 字以上 < 50 ms @ 10 万条；2 字 LIKE < 300 ms |
//
// # 为什么要单独写一条"规模"测试，而不是用日常的小库用例
//
// 小库（几十条）上任何检索都是 0.1 ms —— 数字好看，但没有信息量：
// 它既测不出索引有没有真的建，也测不出查询计划有没有退化。而 §12 的
// 指标是**带规模条件**的（"@ 10 万条"），规模本身就是被验收的一部分。
//
// # 这条测试怎么做到"承重"
//
// 光测时间是不够的：一个把 FTS 索引丢掉、退回 LIKE 全表扫的实现，
// 在小库上照样快；反过来，一个**什么都没搜到**的实现快得离谱。
// 所以每个查询都要同时满足三件事，缺一不可：
//
//	① pg.Mode == SearchModeFTS —— 3 字以上确实走了 trigram 索引，
//	   而不是被 List 的降级逻辑悄悄换成了 LIKE（那时虽然也能出结果，
//	   但复杂度已经不是同一件事了）；
//	② 命中了行 —— 结果非空，"快"才有意义；
//	③ 延迟在 §12 的预算内。
//
// 只断言 ③ 的话，把查询改成 `WHERE 0` 就能让它永远绿。
func TestSearchLatencyAt100k(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试：-short 下跳过（播种 10 万条约 20 s）")
	}

	rows := 100_000
	if v := os.Getenv("PAWCLIP_LATENCY_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("PAWCLIP_LATENCY_ROWS=%q 不是正整数", v)
		}
		rows = n
	}

	db := newTestDB(t)
	ctx := context.Background()
	seedBulkCorpus(t, db, rows)

	// 一条**极罕见**的记录，用来量"命中稀疏"那一端。
	//
	// 为什么必须单独量：query.go 的 FTS 检索是"按时间倒序扫、逐条判定命中、
	// 扫够一页就停"。稠密命中时这个策略极快（提前终止），但命中稀疏时它要
	// 走完整张时间索引才凑够一页——**这是那个改写的代价，不是遗漏**。
	// 不把它测出来，就等于假装代价不存在。
	rare := sampleItem("sha256:rare-latency", "极罕见检索词 zzzraretermqqq")
	rare.CreatedAt, rare.FirstSeenAt = 1_700_000_000, 1_700_000_000
	if _, err := PutItem(ctx, db.Writer(), &rare); err != nil {
		t.Fatal(err)
	}

	// 预算直接抄 §12，不在这里放宽。
	//
	// 特意不设"CI 上乘个系数"这种逃生门：这条测试的意义就是拿规格数字
	// 卡住实现。真在某台机器上过不去，那是需要看具体数字作判断的事，
	// 而不是把阈值调到刚好能过。
	cases := []struct {
		name   string
		query  string
		budget time.Duration
		// wantFTS 为真时要求走 trigram 索引路径。
		wantFTS bool
	}{
		{"3 字中文", "剪贴板", 50 * time.Millisecond, true},
		{"4 字中文", "剪贴板历史", 50 * time.Millisecond, true},
		{"8 字英文", "clipboard", 50 * time.Millisecond, true},
		{"中英混排", "剪贴板 clipboard", 50 * time.Millisecond, true},
		{"2 字中文（LIKE 兜底）", "剪贴", 300 * time.Millisecond, false},
		{"2 字英文（LIKE 兜底）", "cl", 300 * time.Millisecond, false},
		// 稀疏命中：整库只有 1 条匹配。这是时间序扫描策略的**代价端**，
		// 预算仍然是 §12 的 50 ms（实测 25~35 ms，余量不大，是刻意的——
		// 余量小才说明这条测试在真的盯着这件事）。
		{"稀疏命中（全库仅 1 条）", "zzzraretermqqq", 50 * time.Millisecond, true},
	}

	const samples = 30
	for _, c := range cases {
		// 先跑一次把连接与页缓存捂热，再开始计时。
		// 不预热的话第一个样本会包含建立连接、sqlite 页缓存冷启动的开销，
		// 而真实使用中用户是在一个已经热了的库上搜索的。
		if _, err := db.List(ctx, Query{Text: c.query, Limit: 50}); err != nil {
			t.Fatalf("%s：预热查询失败：%v", c.name, err)
		}

		lat := make([]time.Duration, 0, samples)
		var matched int
		var mode SearchMode
		for i := 0; i < samples; i++ {
			start := time.Now()
			pg, err := db.List(ctx, Query{Text: c.query, Limit: 50})
			el := time.Since(start)
			if err != nil {
				t.Fatalf("%s：第 %d 次查询失败：%v", c.name, i, err)
			}
			lat = append(lat, el)
			matched = len(pg.Rows)
			mode = pg.Mode
		}

		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50 := lat[len(lat)/2]
		p95 := lat[(len(lat)*95)/100]

		t.Logf("%-22s  query=%-22q  p50=%-9v p95=%-9v 预算=%-8v 命中=%-4d mode=%s",
			c.name, c.query, p50.Round(time.Microsecond), p95.Round(time.Microsecond),
			c.budget, matched, mode)

		// ② 命中了行。
		if matched == 0 {
			t.Errorf("%s：查询 %q 一条都没命中 —— 此时再快也是假的", c.name, c.query)
			continue
		}
		// ① 走的是该走的路径。
		if c.wantFTS && mode != SearchModeFTS {
			t.Errorf("%s：查询 %q 走的是 %s 而不是 FTS —— "+
				"10 万条下退化成 LIKE 全表扫，延迟指标已经不是同一件事了",
				c.name, c.query, mode)
		}
		if !c.wantFTS && mode != SearchModeLike {
			t.Errorf("%s：查询 %q 期望 LIKE 兜底（2 字 trigram 无意义），实际 %s",
				c.name, c.query, mode)
		}
		// ③ 延迟。
		if p95 > c.budget {
			t.Errorf("%s：查询 %q 的 p95 = %v 超出 §12 预算 %v（p50 = %v）",
				c.name, c.query, p95, c.budget, p50)
		}
	}
}

// seedBulkCorpus 用**真实写入路径**（PutItem）播 rows 条数据。
//
// 名字里的 bulk 是为了与 fts_test.go 的 seedCorpus 区分开：那个播的是
// 固定的 7 条检索正确性语料（每条都为某个边界而存在），这个播的是
// 规模数据。两者目的不同，混用会让"这条为什么只有 7 行"和
// "这条为什么有 10 万行"都变得难以理解。
//
// 为什么不直接 INSERT：PutItem 会顺带算拼音、写 FTS、走去重与触发器。
// 绕过它播种出来的库可能在结构上就与真实库不同（比如拼音列是空的），
// 那样的延迟数字不反映真实情况。代价是慢一些（单条约 180 µs），
// 但这是"测量对象与生产环境一致"必须付的成本。
//
// 语料的构成是刻意分散的：中文、英文、中英混排、纯数字、代码片段各占一部分，
// 且检索词只出现在其中一部分行里。如果每一行都包含"剪贴板"，
// 那么 FTS 一次查询要吐 10 万条候选，测出来的是最坏情况而不是典型情况。
func seedBulkCorpus(t *testing.T, db *DB, rows int) {
	t.Helper()
	start := time.Now()

	// 命中率控制在 ~10%：查询会真的捞出一批候选（不是空跑），
	// 又不会退化成"整库都是命中项"。
	hit := []string{
		"剪贴板历史管理工具的一条记录",
		"剪贴板 clipboard 混排内容示例",
		"clipboard history entry for testing",
	}
	miss := []string{
		"项目周会纪要 2026 年第三季度",
		"SELECT id, preview FROM items WHERE deleted_at IS NULL",
		"https://example.com/docs?utm_source=newsletter",
		"订单号 20260915181352 已发货",
		"func main() { fmt.Println(\"hello\") }",
		"这是一段与检索词无关的中文文本内容",
		"lorem ipsum dolor sit amet consectetur",
	}

	ctx := context.Background()
	for i := 0; i < rows; i++ {
		var body string
		if i%10 == 0 {
			body = hit[i%len(hit)]
		} else {
			body = miss[i%len(miss)]
		}
		// 后缀让每一行的指纹与正文都不同 —— 否则去重会把 10 万条并成几条。
		text := fmt.Sprintf("%s #%d", body, i)
		it := sampleItem(fmt.Sprintf("sha256:seed-%d", i), text)
		it.CreatedAt = int64(1_700_000_000 + i)
		it.FirstSeenAt = it.CreatedAt
		if _, err := PutItem(ctx, db.Writer(), &it); err != nil {
			t.Fatalf("播种第 %d 条: %v", i, err)
		}
	}

	alive, err := db.CountAlive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if alive != int64(rows) {
		t.Fatalf("播种后存活 %d 条，期望 %d —— 去重意外合并了行，延迟数字不可信",
			alive, rows)
	}
	t.Logf("播种 %d 条耗时 %v", rows, time.Since(start).Round(time.Millisecond))
}
