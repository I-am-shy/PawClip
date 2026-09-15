package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// 本文件钉住 DESIGN.md §12 的验收判据「搜索正确性」：
//
//	1 / 2 / 3 字中文、英文、中英混排、大小写、特殊字符全部命中符合预期
//
// 以及 §4.2 的两段式分界（≥3 字符走 FTS，1–2 字符走 LIKE 兜底）。
// 这两件事必须一起测：结果对但路径错，等于在"中文两字词搜不到"的坑上
// 蒙对了一次——下一版改分词器就会翻车。

// previewMaxChars 与 clipboard.PreviewMaxChars 同值。
//
// 刻意不 import clipboard：store 是持久化层，不该反向依赖平台层
// （否则 Windows 交叉编译 store 时会把 cgo 一起拖进来）。
const previewMaxChars = 200

// searchCorpus 是固定的测试语料。key 用于在用例里指代某一条，
// 避免依赖自增 rowid 的具体取值。
var searchCorpus = []struct {
	key  string
	text string
}{
	{"a", "中文文档 链接 图片"},                                         // 2 字中文：文档 / 链接 / 图片；3 字：中文文
	{"b", "hello world AI js 5G C++"},                           // 英文、2 字 ASCII、符号 C++
	{"c", "Hello World 混排 mixed"},                               // 大小写对照；中英混排
	{"d", "今天天气不错，报告在这里"},                                       // 1 字："报"
	{"e", "https://example.com/path?utm_source=x&utm_medium=y"}, // URL 去 utm
	{"f", "邮箱 a@b.com 手机 13800138000"},                          // @ 与长数字
	{"g", "100% 完成度 _underscore_ a%b"},                          // % 与 _（LIKE 通配符转义）
}

// seedCorpus 把语料写进库，返回 key → id 的映射。
//
// created_at 按语料顺序递增，所以时间线是 g…a，而 id 也是递增分配——
// 两边都不需要依赖"自增从 1 开始"这种隐含假设。
func seedCorpus(t *testing.T, db *DB) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]int64, len(searchCorpus))
	for i, c := range searchCorpus {
		txt := c.text
		it := Item{
			Kind:        "text",
			TextContent: &txt,
			Preview:     c.text,
			Fingerprint: fmt.Sprintf("sha256:%s", strings.Repeat(fmt.Sprintf("%02x", i+1), 32)),
			ByteSize:    int64(len(c.text)),
			FirstSeenAt: int64(i + 1),
			CreatedAt:   int64(i + 1),
		}
		res, err := PutItem(ctx, db.w, &it)
		if err != nil {
			t.Fatalf("seed %q: %v", c.key, err)
		}
		ids[c.key] = res.ID
	}
	return ids
}

// idList 把 key 列表翻译成按 id 升序排好的 id 列表（idsOf 也按 id 升序）。
func idList(ids map[string]int64, keys ...string) []int64 {
	out := make([]int64, 0, len(keys))
	for _, k := range keys {
		out = append(out, ids[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func requireFTS(t *testing.T, db *DB) {
	t.Helper()
	if !db.FTSAvailable() {
		t.Skip("本构建没有 FTS5（需要 -tags sqlite_fts5）；搜索会整体降级为 LIKE")
	}
}

// idsOf 把一页结果压成排序后的 id 切片，便于与期望集比对。
func idsOf(p *Page) []int64 {
	ids := make([]int64, 0, len(p.Rows))
	for _, r := range p.Rows {
		ids = append(ids, r.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func eqIDs(a, b []int64) bool {
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

// TestSearch_CorrectnessTable 是 §12「搜索正确性」的直接实现。
func TestSearch_CorrectnessTable(t *testing.T) {
	db := newTestDB(t)
	requireFTS(t, db)
	ids := seedCorpus(t, db)

	cases := []struct {
		name string
		q    string
		want []string // 语料 key
		// exactMode 非空时要求模式**恰好**是它；否则只要求属于 allowModes。
		exactMode  SearchMode
		allowModes []SearchMode
	}{
		// ── 1 字中文：必须走 LIKE 兜底（trigram 命中不了 1 个字符）──
		{name: "1字中文", q: "报", want: []string{"d"}, exactMode: SearchModeLike},

		// ── 2 字中文：同样必须兜底，这是国内同类工具最常见的翻车点 ──
		{name: "2字中文-文档", q: "文档", want: []string{"a"}, exactMode: SearchModeLike},
		{name: "2字中文-链接", q: "链接", want: []string{"a"}, exactMode: SearchModeLike},
		{name: "2字中文-图片", q: "图片", want: []string{"a"}, exactMode: SearchModeLike},

		// ── 2 字 ASCII：推论是"英文短查询同样会漏"（§4.2）──
		{name: "2字ASCII-AI", q: "AI", want: []string{"b"}, exactMode: SearchModeLike},
		{name: "2字ASCII-js", q: "js", want: []string{"b"}, exactMode: SearchModeLike},
		{name: "2字ASCII-5G", q: "5G", want: []string{"b"}, exactMode: SearchModeLike},

		// ── 3 字中文：必须**恰好**走 FTS。这一条是主路径没被兜底掩盖的证明 ──
		{name: "3字中文-中文文", q: "中文文", want: []string{"a"}, exactMode: SearchModeFTS},
		{name: "3字中文-报告在", q: "报告在", want: []string{"d"}, exactMode: SearchModeFTS},

		// ── 英文 ──
		{name: "英文-wor", q: "wor", want: []string{"b", "c"}, exactMode: SearchModeFTS},
		{name: "英文-hello", q: "hello", want: []string{"b", "c"}, exactMode: SearchModeFTS},
		{name: "英文-world", q: "world", want: []string{"b", "c"}, exactMode: SearchModeFTS},

		// ── 大小写：FTS trigram 默认 case_sensitive=0，LIKE 对 ASCII 也不区分 ──
		{name: "大小写-HELLO", q: "HELLO", want: []string{"b", "c"}, exactMode: SearchModeFTS},
		{name: "大小写-HeLLo", q: "HeLLo", want: []string{"b", "c"}, exactMode: SearchModeFTS},
		{name: "大小写-Mixed", q: "Mixed", want: []string{"c"}, exactMode: SearchModeFTS},
		{name: "大小写-mIXED", q: "mIXED", want: []string{"c"}, exactMode: SearchModeFTS},

		// ── 中英混排 ──
		{name: "混排-2字", q: "混排", want: []string{"c"}, exactMode: SearchModeLike},
		{name: "混排-英文侧", q: "mixed", want: []string{"c"}, exactMode: SearchModeFTS},

		// ── 特殊字符：FTS 是纯字符滑窗，标点可能对不齐，
		//    所以这几种允许走 "fts+like" 兜底；但**结果必须命中**。
		{name: "特殊-C++", q: "C++", want: []string{"b"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},
		{name: "特殊-邮箱", q: "a@b.com", want: []string{"f"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},
		{name: "特殊-utm", q: "utm_source", want: []string{"e"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},
		{name: "特殊-长数字", q: "13800138000", want: []string{"f"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},

		// ── LIKE 通配符必须被转义，否则搜 % 会命中全部 ──
		{name: "通配符-百分号", q: "100%", want: []string{"g"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},
		{name: "通配符-下划线", q: "_underscore_", want: []string{"g"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},
		{name: "通配符-中缀-百分号", q: "a%b", want: []string{"g"},
			allowModes: []SearchMode{SearchModeFTS, SearchModeFTSFallback}},

		// ── 无命中 ──
		{name: "无命中", q: "这个词一定不在语料里", want: nil,
			allowModes: []SearchMode{SearchModeFTS}},
	}

	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pg, err := db.List(ctx, Query{Text: c.q})
			if err != nil {
				t.Fatalf("List(%q): %v", c.q, err)
			}
			got := idsOf(pg)
			want := idList(ids, c.want...)
			if !eqIDs(got, want) {
				t.Errorf("搜索 %q 命中 %v，想要 %v（模式=%s）", c.q, got, want, pg.Mode)
			}

			switch {
			case c.exactMode != "":
				if pg.Mode != c.exactMode {
					t.Errorf("搜索 %q 走了 %s，想要恰好 %s（%d 个字符）",
						c.q, pg.Mode, c.exactMode, len([]rune(c.q)))
				}
			case len(c.allowModes) > 0:
				ok := false
				for _, m := range c.allowModes {
					if pg.Mode == m {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("搜索 %q 走了 %s，允许的是 %v", c.q, pg.Mode, c.allowModes)
				}
			}
		})
	}
}

// TestSearch_PathBoundary 单独钉住"该走哪条路"的判定，
// 不依赖数据库，因此 FTS 缺失时也能跑。
func TestSearch_PathBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want SearchMode
	}{
		{"", SearchModeNone},
		{"   ", SearchModeNone},
		{"a", SearchModeLike},
		{"ab", SearchModeLike},
		{"中", SearchModeLike},
		{"中文", SearchModeLike},
		{"中文文", SearchModeFTS},
		{"abc", SearchModeFTS},
		{"AI", SearchModeLike},
		{"5G", SearchModeLike},
		{"C++", SearchModeFTS},
		// 字节数陷阱：2 个中文字是 6 字节，按字节算会误判成"够长"。
		{"文档", SearchModeLike},
	}
	for _, c := range cases {
		if got := ModeFor(c.in); got != c.want {
			t.Errorf("ModeFor(%q) = %s，想要 %s（rune 数=%d，字节数=%d）",
				c.in, got, c.want, len([]rune(c.in)), len(c.in))
		}
	}
}

// TestSearch_LikeEscaping 单独验证 LIKE 通配符转义。
//
// 这条不能只靠"搜索结果对"来覆盖：若语料里恰好没有能暴露它的行，
// 测试会绿着放过一个"搜 % 返回全部"的 bug。
func TestSearch_LikeEscaping(t *testing.T) {
	db := newTestDB(t)
	ids := seedCorpus(t, db)
	ctx := context.Background()

	// 搜 "%"：字面量，只该命中 g。不转义的话会命中全部 7 条。
	pg, err := db.List(ctx, Query{Text: "%"})
	if err != nil {
		t.Fatalf("List(%%): %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "g")) {
		t.Errorf("搜索 \"%%\" 命中 %v，想要 %v——LIKE 通配符没转义", got, idList(ids, "g"))
	}

	// 搜 "_"：字面量。语料里含下划线的是 e（utm_source / utm_medium）与 g。
	// 不转义的话 "_" 是"任意单字符"，会命中全部 7 条——所以这条能抓住 bug。
	pg, err = db.List(ctx, Query{Text: "_"})
	if err != nil {
		t.Fatalf("List(_): %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "e", "g")) {
		t.Errorf("搜索 \"_\" 命中 %v，想要 %v——LIKE 通配符没转义",
			got, idList(ids, "e", "g"))
	}
}

func TestSearch_EscapeLikeAndPhrase(t *testing.T) {
	if got, want := EscapeLike(`a%b_c\d`), `a\%b\_c\\d`; got != want {
		t.Errorf("EscapeLike = %q，想要 %q", got, want)
	}
	// 无通配符时不该改动（也避免无谓的分配）。
	if got := EscapeLike("plain text"); got != "plain text" {
		t.Errorf("EscapeLike 改动了不含通配符的输入：%q", got)
	}
	// 双引号必须按 FTS5 规则翻倍，否则用户搜 `"a"` 会让 MATCH 抛错。
	if got, want := FTSPhrase(`a"b`), `"a""b"`; got != want {
		t.Errorf("FTSPhrase = %q，想要 %q", got, want)
	}
}

// TestSearch_FTSQuerySyntaxIsSafe 验证"用户输入里的 FTS5 语法字符不会让查询报错"。
//
// 不把输入包成短语的话，`a OR b`、`*`、`(`、`"` 都会直接抛 OperationalError
// ——表现为"搜索框一敲就报错"。这里是那条保护的回归测试。
func TestSearch_FTSQuerySyntaxIsSafe(t *testing.T) {
	db := newTestDB(t)
	requireFTS(t, db)
	seedCorpus(t, db)
	ctx := context.Background()

	nasty := []string{
		`a OR b`, `NOT x`, `y*`, `(z)`, `"未闭合`, `^^^`, `a NEAR b`,
		`--`, `'; DROP TABLE items; --`, `\`, `%%%`, `___`, `"a" AND "b"`,
		`***`, `(((`,
	}
	for _, q := range nasty {
		if _, err := db.List(ctx, Query{Text: q}); err != nil {
			t.Errorf("搜索 %q 报错（FTS5 语法字符没被安全处理）：%v", q, err)
		}
	}
	// 注入尝试不能真的删表。
	if _, err := db.CountAll(ctx); err != nil {
		t.Fatalf("items 表不见了：%v", err)
	}
}

// TestList_KeysetPagination 验证 keyset 分页不重不漏（§14 第 3 条）。
func TestList_KeysetPagination(t *testing.T) {
	db := newTestDB(t)
	seedCorpus(t, db)
	ctx := context.Background()

	seen := map[int64]int{}
	var cursor *Cursor
	pages := 0
	for {
		pg, err := db.List(ctx, Query{Limit: 3, Cursor: cursor, IncludeTotal: true})
		if err != nil {
			t.Fatalf("List page %d: %v", pages, err)
		}
		if pg.TotalValid && pg.Total != int64(len(searchCorpus)) {
			t.Errorf("Total = %d，想要 %d", pg.Total, len(searchCorpus))
		}
		for _, r := range pg.Rows {
			seen[r.ID]++
		}
		pages++
		if !pg.HasMore {
			break
		}
		if pg.NextCursor == nil {
			t.Fatal("HasMore 为真但 NextCursor 是 nil")
		}
		cursor = pg.NextCursor
	}

	if want := len(searchCorpus); len(seen) != want {
		t.Fatalf("翻页共拿到 %d 条不重复记录，想要 %d（seen=%v）", len(seen), want, seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("id=%d 出现了 %d 次（keyset 分页重复）", id, n)
		}
	}
	if pages != 3 { // 7 条 / 每页 3 = 3 页（最后一页 1 条）
		t.Errorf("共 %d 页，想要 3", pages)
	}
}

// TestList_CreatedAtCollisionCursor 专测"同一 created_at 的第二页"。
//
// created_at 全相同时，单列游标（只比 created_at）会让第二页要么全漏、
// 要么死循环。这里是"游标必须是 (created_at, id) 二元组"的回归测试。
func TestList_CreatedAtCollisionCursor(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const n = 10
	assigned := make([]int64, 0, n)
	for i := 1; i <= n; i++ {
		txt := fmt.Sprintf("same-second item %02d", i)
		it := Item{
			Kind:        "text",
			TextContent: &txt,
			Preview:     txt,
			Fingerprint: fmt.Sprintf("sha256:%064x", i),
			FirstSeenAt: 500,
			CreatedAt:   500, // 全部同一时刻
		}
		res, err := PutItem(ctx, db.w, &it)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		assigned = append(assigned, res.ID)
	}

	seen := map[int64]int{}
	var cursor *Cursor
	for i := 0; i < 20; i++ { // 上限 20 页，防死循环
		pg, err := db.List(ctx, Query{Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range pg.Rows {
			seen[r.ID]++
		}
		if !pg.HasMore {
			break
		}
		cursor = pg.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("created_at 全相同时翻页拿到 %d 条，想要 %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("id=%d 出现 %d 次——单列 created_at 游标在同时刻记录上会重/漏", id, c)
		}
	}
	for _, id := range assigned {
		if seen[id] != 1 {
			t.Errorf("id=%d 未被翻页覆盖到", id)
		}
	}
}

// TestList_Filters 覆盖 kind / 分类 / 标签 / 置顶 / 回收站 / 时间窗过滤。
func TestList_Filters(t *testing.T) {
	db := newTestDB(t)
	ids := seedCorpus(t, db)
	ctx := context.Background()

	// 给 a 打标签、置顶；把 b 移进回收站。
	tagID, err := db.CreateTag(ctx, "常用", "#185FA5")
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if _, err := db.AddTagToItems(ctx, tagID, []int64{ids["a"]}); err != nil {
		t.Fatalf("AddTagToItems: %v", err)
	}
	if err := db.SetPinned(ctx, []int64{ids["a"]}, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if _, err := db.SoftDelete(ctx, []int64{ids["b"]}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	pg, err := db.List(ctx, Query{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "a", "c", "d", "e", "f", "g")) {
		t.Errorf("时间线 = %v，回收站条目 b 不该出现", got)
	}

	// 回收站
	pg, err = db.List(ctx, Query{Trashed: true})
	if err != nil {
		t.Fatalf("List trashed: %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "b")) {
		t.Errorf("回收站 = %v，想要 %v", got, idList(ids, "b"))
	}

	// 置顶过滤
	pg, err = db.List(ctx, Query{PinnedOnly: true})
	if err != nil {
		t.Fatalf("List pinned: %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "a")) {
		t.Errorf("置顶 = %v，想要 %v", got, idList(ids, "a"))
	}

	// 标签过滤
	pg, err = db.List(ctx, Query{TagID: &tagID})
	if err != nil {
		t.Fatalf("List by tag: %v", err)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "a")) {
		t.Errorf("按标签 = %v，想要 %v", got, idList(ids, "a"))
	}
	if len(pg.Rows) == 1 && !eqIDs(pg.Rows[0].TagIDs, []int64{tagID}) {
		t.Errorf("ListRow.TagIDs = %v，想要 [%d]", pg.Rows[0].TagIDs, tagID)
	}

	// kind 过滤：语料里没有图片，但 files 之外还有一个"不存在的 kind"
	pg, err = db.List(ctx, Query{Kinds: []string{"image"}})
	if err != nil {
		t.Fatalf("List by kind: %v", err)
	}
	if len(pg.Rows) != 0 {
		t.Errorf("kind=image 有 %d 条，语料里没有图片", len(pg.Rows))
	}

	// 时间窗（first_seen_at 按语料顺序递增 1..7）
	since := int64(5)
	pg, err = db.List(ctx, Query{Since: &since})
	if err != nil {
		t.Fatalf("List since: %v", err)
	}
	// 5=first_seen_at 5 → e, 6 → f, 7 → g；但 b 在回收站（first_seen_at=2，不在窗口内）
	if got := idsOf(pg); !eqIDs(got, idList(ids, "e", "f", "g")) {
		t.Errorf("first_seen_at >= 5 = %v，想要 %v", got, idList(ids, "e", "f", "g"))
	}

	// 未分类：全部 7 条都没分类，其中 b 在回收站
	pg, err = db.List(ctx, Query{Uncategorized: true})
	if err != nil {
		t.Fatalf("List uncategorized: %v", err)
	}
	if len(pg.Rows) != 6 {
		t.Errorf("未分类 = %d 条，想要 6", len(pg.Rows))
	}

	// 置顶之后 expires_at 必须被清掉（§5.1 的"收藏永不回收"）
	it, err := db.GetByID(ctx, ids["a"])
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if it.ExpiresAt != nil {
		t.Errorf("置顶条目仍有 expires_at=%d，应为 nil", *it.ExpiresAt)
	}
	if it.TTLSource == nil || *it.TTLSource != TTLSourceNever {
		t.Errorf("置顶条目 ttl_source = %v，想要 %q", it.TTLSource, TTLSourceNever)
	}
}

// TestList_DoesNotSelectTextContent 验证列表查询不把正文拉进内存（§14 第 7 条）。
func TestList_DoesNotSelectTextContent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const size = 200000
	long := strings.Repeat("字", size)
	it := Item{
		Kind:        "text",
		TextContent: &long,
		Preview:     strings.Repeat("字", previewMaxChars),
		Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ByteSize:    int64(len(long)),
		FirstSeenAt: 1,
		CreatedAt:   1,
	}
	if _, err := PutItem(ctx, db.w, &it); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pg, err := db.List(ctx, Query{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pg.Rows) != 1 {
		t.Fatalf("拿到 %d 条，想要 1", len(pg.Rows))
	}
	// TextLen 由 SQL 的 length() 算出完整正文长度——说明长度算对了，
	// 而正文本身没有被 SELECT 出来（ListRow 里根本没有承载它的字段）。
	if pg.Rows[0].TextLen != size {
		t.Errorf("TextLen = %d，想要 %d", pg.Rows[0].TextLen, size)
	}
	if len([]rune(pg.Rows[0].Preview)) > previewMaxChars {
		t.Errorf("preview 长 %d，超过 %d", len([]rune(pg.Rows[0].Preview)), previewMaxChars)
	}
}

// TestSearch_DegradesToLikeWhenFTSMissing 验证"FTS 不可用时整体退化为 LIKE"。
//
// 这是 §13 风险表里「FTS5 未编入 SQLite」的约定降级路径，不能在改了
// matchMode 之后失效。
func TestSearch_DegradesToLikeWhenFTSMissing(t *testing.T) {
	db := newTestDB(t)
	ids := seedCorpus(t, db)
	ctx := context.Background()

	// 人为把 FTS 标记关掉，模拟"漏了 -tags sqlite_fts5"的产物。
	db.ftsAvailable = false
	t.Cleanup(func() { db.ftsAvailable = true })

	pg, err := db.List(ctx, Query{Text: "文档"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if pg.Mode != SearchModeLike {
		t.Errorf("FTS 不可用时模式 = %s，想要 %s", pg.Mode, SearchModeLike)
	}
	if got := idsOf(pg); !eqIDs(got, idList(ids, "a")) {
		t.Errorf("降级后搜索\"文档\" = %v，想要 %v", got, idList(ids, "a"))
	}
	if pg.FTSAvailable {
		t.Error("Page.FTSAvailable 应为 false")
	}
}
