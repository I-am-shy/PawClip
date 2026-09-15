package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrFTSUnavailable 表示当前构建没有把 FTS5 编进 SQLite。
//
// 触发它几乎总是"构建时漏了 -tags sqlite_fts5"（见 scripts/build.sh），
// 而不是运行时问题。
var ErrFTSUnavailable = errors.New("store: FTS5 is not available in this build (rebuild with -tags sqlite_fts5)")

// 本文件是 DESIGN.md §4.2 / §10 里的 `store/fts.go`：**两段式检索**。
//
//	查询 ≥ 3 字符 → items_fts MATCH ?（trigram 分词器）
//	查询 1–2 字符 → LIKE '%?%' 兜底（trigram 命中不了这么短的查询）
//
// 为什么必须有兜底：FTS5 用 trigram 分词器时，**查询词至少要有 3 个字符**。
// 中文两字词（"链接""图片""文档"）与两字符 ASCII（"AI""5G""js"）都会
// 返回 0 行——不是查询错了，是索引里根本没有长度 < 3 的 token。
// 这是同类工具在国内最常见的差评来源，所以单测强制覆盖
// 1 字 / 2 字 / 3 字 / 中英混排四种输入（见 fts_test.go）。

// matchMode 决定这条查询实际走哪条路。
//
// 分界线是**字符数**（rune 数）而不是字节数：中文一个字 3 字节，
// 按字节算会让"文档"（2 字 / 6 字节）错误地走进 trigram 分支、拿到空结果。
//
// FTS 不可用（构建漏了 -tags sqlite_fts5）时整体退化为 LIKE——
// 这是 §13 风险表里"FTS5 未编入 SQLite"的约定降级路径。
func (d *DB) matchMode(text string) SearchMode {
	if strings.TrimSpace(text) == "" {
		return SearchModeNone
	}
	if UseFTS(text) && d.ftsAvailable {
		return SearchModeFTS
	}
	return SearchModeLike
}

// UseFTS 报告这个查询是否**够长**到能走 FTS（不管 FTS 是否可用）。
//
// 拆成独立函数是为了让"该走哪条路"这件事可以被单测直接钉住，
// 而不必先建一个库。
func UseFTS(text string) bool { return len([]rune(text)) >= TrigramMinRunes }

// ModeFor 是 UseFTS 的可读版：给出发起查询前**预期**会走的路径。
//
// 注意它不考虑 ftsAvailable：FTS 不可用时实际会降级为 LIKE。
// 调用方若需要"实际走了哪条路"，用 Page.Mode。
func ModeFor(text string) SearchMode {
	if strings.TrimSpace(text) == "" {
		return SearchModeNone
	}
	if UseFTS(text) {
		return SearchModeFTS
	}
	return SearchModeLike
}

// FTSPhrase 把用户输入变成一个 FTS5 "短语"查询。
//
// 必须包成双引号短语，原因有两个：
//
//   - 不包的话，输入里的 `AND` / `OR` / `NOT` / `*` / `(` / `"` 都是 FTS5
//     语法，用户搜 `C++`、`a OR b`、`"未闭合` 会直接抛 OperationalError
//     （表现为整个搜索框一敲字符就报错）；
//   - trigram 分词器下，只有短语查询才是"整段子串匹配"——这正是我们要的
//     （trigram 已把文本切成三字滑窗，所以"中文文"能命中"中文文档"）。
//
// 短语内的双引号按 FTS5 规则用两个双引号转义。
func FTSPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// EscapeLike 转义 LIKE 的通配符。
//
// 不转义的话，用户搜 `%` 会命中全部条目（看起来像"搜索坏了"），
// 搜 `a_b` 会错误命中 `axb`。反斜杠必须最先处理，否则会把后面补的
// 反斜杠再转义一次。
func EscapeLike(s string) string {
	if !strings.ContainsAny(s, `\%_`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\', '%', '_':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// FTSRowCount 返回全文索引里的行数，用于"索引是否跟得上"的自检。
//
// external content 表下 count(*) 走的是索引本身，很快。
func (d *DB) FTSRowCount(ctx context.Context) (int64, error) {
	if !d.ftsAvailable {
		return 0, ErrFTSUnavailable
	}
	var n int64
	if err := d.r.QueryRowContext(ctx, "SELECT count(*) FROM items_fts").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count fts rows: %w", err)
	}
	return n, nil
}

// RebuildFTS 重建全文索引。
//
// 用在哪：老库第一次换到带 sqlite_fts5 的构建上（Open 里已经会自动做一次），
// 以及导入之后的行数比对发现索引落后时。
func (d *DB) RebuildFTS(ctx context.Context) error {
	if !d.ftsAvailable {
		return ErrFTSUnavailable
	}
	if _, err := d.w.ExecContext(ctx, `INSERT INTO items_fts(items_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("store: rebuild fts: %w", err)
	}
	return nil
}
