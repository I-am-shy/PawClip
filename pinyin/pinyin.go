// Package pinyin 把中文串压成拼音首字母串，用于"打首字母找中文"的检索。
//
// 典型用法（DESIGN §11 P2）：
//
//	Initials("文档管理")  // "wdgl"
//	Initials("ZIM 文档")  // "zimwd"
//
// 检索侧把它当作 items.pinyin 列，用子串匹配去找 `zgd` 这类查询。
//
// 三条边界说明：
//
//   - **多音字只取第一个读音**（表由 tools/genpinyin 从 pinyin-data 生成）。
//     "行"取 x 而不是 h，所以"银行"写成 "yx" 而不是 "yh"。这是取舍不是
//     bug —— 拼音搜索是"大概记得怎么念"的模糊入口，不是注音工具；
//     要精确就到 FTS 那边搜汉字本身。
//   - 非汉字的字母/数字**原样保留**（小写化），所以中英混排也能打首字母。
//     标点、空白、emoji 一律跳过——它们不构成"读音"。
//   - 表只覆盖 CJK 基本区（U+4E00–U+9FFF）。扩展区的生僻字按"未知"处理，
//     既不加首字母也不报错：多一个字符猜不出来，不该让整条记录搜不到。
package pinyin

import "strings"

// Initials 返回 s 的拼音首字母串（小写）。
//
// 全部字符都无法映射时返回空串——调用方据此跳过"写 pinyin 列"这一步，
// 而不是往里塞一个空字符串（那会让 pinyin 列看起来"有值但搜不到"）。
func Initials(s string) string {
	if s == "" {
		return ""
	}
	// 预估容量：绝大多数是汉字与 ASCII，按 rune 数估够用。
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= tableLo && r <= tableHi:
			if v := table[r-tableLo]; v != 0 {
				b.WriteByte(v)
			}
		case r < 0x80:
			// ASCII 字母与数字：原样小写保留。
			// 不含空格与标点——见包注释第 2 条。
			c := byte(r)
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				b.WriteByte(c)
			case c >= 'A' && c <= 'Z':
				b.WriteByte(c | 0x20)
			}
		}
		// 其余（标点、空白、emoji、其它语种）跳过。
	}
	return b.String()
}

// MaxQueryLen 是拼音查询的长度上限（字节）。
//
// 超过它的查询不再走拼音路径：那已经不是在"打首字母"，而是在贴着
// pinyin 列做一次近似全文匹配，既慢又没意义。
const MaxQueryLen = 16

// Query 判断一段用户输入该不该走拼音检索，是则返回规范化后的查询串。
//
// 判据刻意收得很紧——**只有纯 ASCII 字母/数字**才算：
//
//	"zgd"    → "zgd", true
//	"ZGD"    → "zgd", true
//	"文"      → "", false   （汉字本身走 FTS，拼音列帮不上忙）
//	"文档"    → "", false
//	"zgd "   → "", false    （含空格：可能是"多个词"的检索，交给 FTS）
//	"a"      → "a", true
//
// 为什么排除含空格的输入：`zim wd` 这种是"两个词"的意图，而 pinyin 列是
// 一整条无分隔的首字母串，用整串去匹配必然不中。让它落到 FTS 上，
// 由那边按短语/多词处理，比在这里猜更靠谱。
func Query(s string) (string, bool) {
	if s == "" || len(s) > MaxQueryLen {
		return "", false
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c|0x20)
		default:
			// 出现任何非字母数字就整体放弃（不是"跳过它"）：
			// 见上面"排除含空格"的理由。
			return "", false
		}
	}
	if len(out) == 0 {
		return "", false
	}
	return string(out), true
}

// Known 报告一个汉字是否在表内（表覆盖范围内的已收录字）。
// 只用于诊断与测试，检索路径上不调用。
func Known(r rune) bool {
	if r < tableLo || r > tableHi {
		return false
	}
	return table[r-tableLo] != 0
}
