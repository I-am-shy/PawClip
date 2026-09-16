package pinyin

import (
	"strings"
	"testing"
)

// TestInitials_KnownWords 是最重要的一条：它钉住"具体字 → 具体首字母"。
//
// 只测"结果非空"是不够的——表整体错位（比如少了一个码位的偏移）依然非空，
// 但每一个字都会错。所以这里逐个字对答案。
func TestInitials_KnownWords(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"一", "y"},
		{"文档", "wd"},
		{"文档管理", "wdgl"},
		{"中文", "zw"},
		{"搜索", "ss"},
		{"图片", "tp"},
		{"剪贴板", "jtb"},
		{"链接", "lj"},
		{"复制", "fz"},
		{"粘贴", "zt"},
		{"设置", "sz"},
		{"导出", "dc"},
		{"导入", "dr"},
		{"分类", "fl"},
		{"标签", "bq"},
		{"过期", "gq"},
		{"回收站", "hsz"},
		{"喵喵贴", "mmt"},
		{"剪贴板历史", "jtbls"},
	}
	for _, c := range cases {
		if got := Initials(c.in); got != c.want {
			t.Errorf("Initials(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// TestInitials_MixedScript 中英混排：ASCII 原样保留。
//
// 这条实际上是"打首字母"最常见的形态——内容里有产品名/英文单词，
// 例如 "ZIM 文档" 用户会打 "zimwd"。
func TestInitials_MixedScript(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ZIM 文档", "zimwd"},
		{"Hello 世界", "hellosj"},
		{"v2.0 版本", "v20bb"},
		{"API 接口文档", "apijkwd"},
		// 标点与空白跳过，但不影响其它字符的相对顺序。
		{"文档, 管理; 界面!", "wdgljm"},
		{"  文档  ", "wd"},
		{"文档\n管理", "wdgl"},
	}
	for _, c := range cases {
		if got := Initials(c.in); got != c.want {
			t.Errorf("Initials(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// TestInitials_EdgeCases 空输入与"全无读音"的输入。
func TestInitials_EdgeCases(t *testing.T) {
	// 空串返回空串，不是 nil 也不是 "?"。
	if got := Initials(""); got != "" {
		t.Errorf("Initials(\"\") = %q，想要空串", got)
	}
	// 纯标点/emoji/非汉字文字 → 空串。
	// 强调：不能返回这些字符本身——那会让 pinyin 列里混进标点，
	// 用 "，" 之类的查询能命中，看起来像 bug。
	for _, in := range []string{"   ", "!!!", "🎉🎉", "，。；", "한국어", "Привет"} {
		if got := Initials(in); got != "" {
			t.Errorf("Initials(%q) = %q，想要空串", in, got)
		}
	}
}

// TestInitials_KanjiShareHanziCodepoints 记录一个"看起来像 bug、其实不是"的行为。
//
// 日文汉字与中文汉字在 Unicode 里**共用码位**（日、本、語 就是 U+65E5 /
// U+672C / U+8A9E）。所以给它们算出中文拼音首字母是**正确**的，不是误判：
//
//	Initials("日本語") == "rby"
//
// 我第一版把 日本語 放进了"应为空串"的用例里，跑出来是 "rby" —— 错的
// 是那条断言。这类"看起来该失败却通过了/该通过却失败了"的地方值得留一条
// 测试说明，否则下一个人还会把它当 bug 去"修"。
//
// 好处是真实的：用户想找一段含"日本"的内容，打 "rb" 能命中，
// 这正是拼音搜索该有的行为。韩文/西里尔字母才是真正的"无中文读音"。
func TestInitials_KanjiShareHanziCodepoints(t *testing.T) {
	if got := Initials("日本語"); got != "rby" {
		t.Errorf("Initials(\"日本語\") = %q，想要 \"rby\"（汉字码位共用，中文拼音适用）", got)
	}
	if got := Initials("한국어"); got != "" {
		t.Errorf("Initials(\"한국어\") = %q，想要空串（谚文不是汉字）", got)
	}
}

// TestInitials_UnknownHanziIsSkippedNotFatal 生僻字（表外）不该让整条记录搜不到。
//
// 表只覆盖 CJK 基本区。扩展 B 区的字（如 U+20000 起）按未知处理：
// 跳过它，但**继续处理后面的字**。
func TestInitials_UnknownHanziIsSkippedNotFatal(t *testing.T) {
	// U+20000 是扩展 B 区的字，一定不在表内。
	in := "文档\U00020000管理"
	want := "wdgl"
	if got := Initials(in); got != want {
		t.Errorf("Initials(%q) = %q，想要 %q（生僻字应被跳过而不是中断）", in, got, want)
	}
	if Known('\U00020000') {
		t.Error("Known(U+20000) = true，但它不该在基本区表里")
	}
	if !Known('文') {
		t.Error("Known('文') = false，基本区的常用字必须在表里")
	}
}

// TestQuery_AcceptsOnlyBareASCII 是选路正确性。
//
// 这条决定"什么时候去查 pinyin 列"。判错的两个方向都有代价：
//   - 放太宽（比如接受汉字）→ 每次搜索都多跑一次注定为空的 LIKE，白慢；
//   - 判太紧（比如不接受 1 个字母）→ 用户打 "w" 时找不到"文档"。
func TestQuery_AcceptsOnlyBareASCII(t *testing.T) {
	accept := map[string]string{
		"zgd":  "zgd",
		"ZGD":  "zgd",
		"ZgD":  "zgd",
		"a":    "a",
		"wd":   "wd",
		"v20":  "v20",
		"zim":  "zim",
		"abcd": "abcd",
	}
	for in, want := range accept {
		got, ok := Query(in)
		if !ok {
			t.Errorf("Query(%q) 被拒，但它该被接受", in)
			continue
		}
		if got != want {
			t.Errorf("Query(%q) = %q，想要 %q", in, got, want)
		}
	}

	reject := []string{
		"", "文档", "文", "中 文", "zgd wd", "z d", "a-b", "a.b",
		"a%b", "a_b", "a+b", "zgd!", "  ", "\t", strings.Repeat("a", MaxQueryLen+1),
		"zgd\n", "🎉",
	}
	for _, in := range reject {
		if got, ok := Query(in); ok {
			t.Errorf("Query(%q) = (%q, true)，想要被拒绝", in, got)
		}
	}
	// 边界：刚好到上限要接受，超一个字符要拒绝。
	if _, ok := Query(strings.Repeat("a", MaxQueryLen)); !ok {
		t.Errorf("长度 %d 的查询应被接受（MaxQueryLen 是含端点）", MaxQueryLen)
	}
}

// TestQuery_RejectsSpacesEvenThoughInitialsIgnoresThem 是一处容易搞混的对称性。
//
// Initials 跳过空格（`文档 管理` → `wdgl`），Query 却拒绝含空格的输入。
// 两者看起来矛盾，其实是不同的东西：
//   - Initials 处理的是**存进库的内容**，空格只是排版，跳过它是对的；
//   - Query 处理的是**用户意图**，`zgd wd` 是"两个词"而 pinyin 列是一整条
//     无分隔串，拿整串去匹配必然不中，所以交给 FTS 更靠谱。
//
// 钉住这条是为了防止有人"顺手"把 Query 也改成跳过空格——那会让
// `zgd wd` 变成对 "zg dw" 的匹配，几乎永远搜不到，而用户以为搜索坏了。
func TestQuery_RejectsSpacesEvenThoughInitialsIgnoresThem(t *testing.T) {
	if got := Initials("文档 管理"); got != "wdgl" {
		t.Fatalf("Initials(\"文档 管理\") = %q，想要 \"wdgl\"", got)
	}
	if _, ok := Query("wd gl"); ok {
		t.Error("Query 不该接受含空格的输入")
	}
}

// TestTable_Sanity 表本身的基本健全性。
//
// 防"生成器写错了半张表却没人发现"：表里只允许出现 a-z 与 0。
func TestTable_Sanity(t *testing.T) {
	if len(table) != tableHi-tableLo+1 {
		t.Fatalf("表长度 %d 与码位范围 %d..%d 不符", len(table), tableLo, tableHi)
	}
	if tableLo != 0x4E00 || tableHi != 0x9FFF {
		t.Fatalf("码位范围变成了 %#x..%#x，包注释与生成器都要同步改", tableLo, tableHi)
	}
	known := 0
	for i, b := range table {
		switch {
		case b == 0:
			continue
		case b >= 'a' && b <= 'z':
			known++
		default:
			t.Fatalf("表第 %d 项（码位 %#x）的值是 %#x，只允许 0 或 a-z",
				i, tableLo+i, b)
		}
	}
	// CJK 基本区有 2 万多个码位，pinyin-data 覆盖其中绝大部分。
	// 门槛定在 2 万：低于它说明生成过程出问题了。
	if known < 20000 {
		t.Errorf("表里只有 %d 个汉字有拼音，低于 20000 的门槛——生成器可能错了", known)
	}
	t.Logf("表覆盖 %d 个码位，其中 %d 个有拼音首字母", len(table), known)
}

// TestQueryAndInitials_Compose 端到端：存进去的初始串能被查询命中。
//
// 这是两个函数真正的契约——它们必须能对上。分开测各自都对、合起来不匹配
// 是很可能的（比如 Initials 输出大写而 Query 输出小写）。
func TestQueryAndInitials_Compose(t *testing.T) {
	corpus := []string{
		"剪贴板历史管理",
		"ZIM 文档链接",
		"GitHub Actions 矩阵构建",
		"导出为 .clipbak 文件",
	}
	for _, text := range corpus {
		stored := Initials(text)
		if stored == "" {
			t.Fatalf("Initials(%q) 为空，无法参与后续断言", text)
		}
		// 用这条内容前 3 个首字母去查，必须能作为子串命中。
		if len(stored) < 3 {
			continue
		}
		q, ok := Query(strings.ToUpper(stored[:3]))
		if !ok {
			t.Fatalf("Query(%q) 被拒", stored[:3])
		}
		if !strings.Contains(stored, q) {
			t.Errorf("查 %q 无法在 %q（来自 %q）里命中", q, stored, text)
		}
	}
}
