// 本文件是**目录与业务之间的那一层**：把 catalog 里的字符串接到真正的
// 使用点上（错误、报告、转换失败的原因）。
//
// 为什么从 i18n.go 里拆出来：i18n.go 只放"目录本身"（键、两张表、语言解析），
// 而 i18n_test.go 里有一条测试会扫**除了 i18n.go 之外**的源码，确认每个键
// 都真的有人用。如果这些函数留在 i18n.go 里，那条测试就会把
// "localizeErr 用到了 msgErrNotFound"也算成"没人用"——测试与实现互相打架。
// 拆开之后职责也更清楚：一个是数据，一个是接线。
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zego/pawclip/panel"
	"github.com/zego/pawclip/store"
	"github.com/zego/pawclip/transform"
)

// ── 把"开发者视角的错误"翻成"用户看得懂的一句话" ──────────────

// msgErr 是一个"已经声明好该用哪条文案"的错误。
//
// # 它解决的是什么问题
//
// 有一类错误是在**够不着翻译器的地方**产生的。最典型的是平台层：
//
//	autostart_darwin.go / autostart_windows.go 里是包级函数
//	（setAutoStart、autoStartEnabled），不是 *App 的方法，拿不到 a.T()；
//	reveal_*.go 同理。
//
// 而它们产出的错误**最终是要显示给用户的**——用户在设置里拨一下"开机自启"，
// 底层 launchctl / reg 失败了，错误会一路冒到界面上。
//
// 如果让那些地方直接拼一句中文，英文用户就会看到中文报错。这正是
// §14 第 24 条把"错误提示"列为 i18n 易漏位置的原因：它们不在
// 前端，也不在绑定层显眼的位置，而在"看起来只是系统调用"的深处。
//
// # 做法
//
// 让那一层只声明**用哪条文案**（键 + 参数），不负责措辞：
//
//	return msgf(msgErrAutoStartFail, err, a.T(msgActWriteAutoStart), err)
//
// 真正的渲染推迟到进程边界（localizeErr），那里语言是确定的。
// 这与 store / panel 返回哨兵错误是同一条思路：**越过包边界只传
// "是什么"，不传"怎么说"**。
type msgErr struct {
	key msgKey
	// args 是交给 fmt.Sprintf 的参数（对应文案里的 %s / %v）。
	args []any
	// cause 保留 %w 语义：调用方仍然能用 errors.Is 认出底层错误。
	cause error
}

// Error 让 msgErr 满足 error。
//
// 它用**基准语言（中文）**渲染，因为走到这里的字符串是给日志和开发者看的。
// 看到 "「写入开机自启项」失败：exit status 5" 立刻知道发生了什么；
// 只给一个 `err.autostartFailed` 还得去查表。
//
// 界面不走这条路径 —— localizeErr 会用**当前语言**重新渲染一遍。
func (e *msgErr) Error() string {
	base := catalog[LangZhCN][e.key]
	if base == "" {
		base = catalog[LangEn][e.key]
	}
	if base == "" {
		base = string(e.key)
	}
	if len(e.args) == 0 {
		return base
	}
	return fmt.Sprintf(base, renderArgs(base, catalog[LangZhCN], e.args)...)
}

// renderArgs 把参数里的 msgKey 先翻成对应语言的文本，其余原样。
//
// 为什么参数里会出现 msgKey：像"开机自启"这种流程，错误句子是
// "「%s」失败：%v"，而 %s 那个动作名（"写入开机自启项"）本身也是要翻译的。
// 平台层（autostart_darwin.go 等）拿不到 a.T()，但它**能拿到 msgKey 常量**，
// 于是让它传键、由这里翻——"越过包边界只传是什么，不传怎么说"。
//
// 顺带一个好处：参数是键而不是字符串，意味着**排版时不可能忘了翻**。
// 如果平台层传的是已经拼好的中文，渲染这边无从判断该不该动它。
func renderArgs(tmpl string, table map[msgKey]string, args []any) []any {
	out := make([]any, len(args))
	for i, v := range args {
		mk, ok := v.(msgKey)
		if !ok {
			out[i] = v
			continue
		}
		if s, ok := table[mk]; ok {
			out[i] = s
		} else {
			out[i] = string(mk)
		}
	}
	return out
}

// Unwrap 让 errors.Is/As 能穿透到 cause。
func (e *msgErr) Unwrap() error { return e.cause }

// msgf 造一个"声明了文案键"的错误。cause 可以为 nil。
//
// 注意 args 里**不要**塞中文字面量：那些会被原样拼进英文句子。
// 需要作为参数出现的短语（比如自启各步骤的动作名）本身也得是 msgKey，
// 由调用方用 a.T(...) 取出来再传进来。
func msgf(k msgKey, cause error, args ...any) error {
	return &msgErr{key: k, args: args, cause: cause}
}

// renderedErr 是"已经按某门语言渲染好的一句话"。
//
// 存在的唯一理由是**保留 cause**：localizeErr 把 msgErr 按当前语言说完之后，
// 直接 `errors.New(文本)` 会把 Unwrap 链砍断，于是上层再也无法用
// errors.Is 判断底层原因（恢复流程要区分"文件不存在"与"权限不足"）。
// 这个小类型让"显示用的文本"与"可判断的原因"共存。
type renderedErr struct {
	text  string
	cause error
}

func (e *renderedErr) Error() string { return e.text }
func (e *renderedErr) Unwrap() error { return e.cause }

// localizeErr 把已知的哨兵错误换成当前语言的文案。
//
// # 为什么要做这一道
//
// store / panel 里返回的是 `store: item not found` 这种**诊断**字符串，
// 它出现在界面上的时候是给用户看的——§14 第 24 条把"错误提示"列为
// i18n 易漏位置，说的就是这种情况。所以在进程边界（绑定层）统一翻一次：
//
//   - 认得出（errors.Is 命中哨兵）→ 换成用户语言的提示；
//   - 认不出 → **原样透出**。此时那句话是诊断信息（"磁盘只读"之类），
//     翻译它没有意义，保留原文反而更好定位问题。
//
// 注意用 errors.Is 而不是比字符串：包装过的错误（`fmt.Errorf("...: %w", err)`）
// 也要能认出来。
func (a *App) localizeErr(err error) error {
	if err == nil {
		return nil
	}
	// msgErr 走专门一路：它自己带着键与参数，只是说话的语言还没定。
	// 这里用**当前语言**渲染出来，同时把 cause 接回去。
	//
	// ⚠️ 不能直接 `return err`（那样 Error() 会用基准语言，等于没翻），
	// 也不能 `errors.New(a.Tf(...))`（那样 Unwrap 链就断了）。
	var me *msgErr
	if errors.As(err, &me) {
		return &renderedErr{text: a.render(me.key, me.args...), cause: me.cause}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errors.New(a.T(msgErrNotFound))
	case errors.Is(err, store.ErrDraftGone):
		// 草稿在编辑期间被删掉（另一处入口，或另一台机器导入）。
		// 单独一条而非复用 ErrNotFound：这句要说的是"你手上这条没了，
		// 别再往里写"，而不是"找不到"。
		return errors.New(a.T(msgErrDraftGone))
	case errors.Is(err, store.ErrCategoryNameTaken):
		return errors.New(a.T(msgErrCategoryTaken))
	case errors.Is(err, store.ErrTagNameTaken):
		return errors.New(a.T(msgErrTagTaken))
	case errors.Is(err, panel.ErrUnsupported):
		return errors.New(a.T(msgErrNoPanel))
	case errors.Is(err, panel.ErrHotkeyTaken):
		return errors.New(a.T(msgNotifyHotkeyTaken))
	}
	return err
}

// render 按当前语言渲染一条带参数的文案。
//
// 它是 localizeErr 与 msgErr 共用的那一半逻辑：参数里可能是 msgKey，
// 先翻再拼。之所以不直接调 Tf，是因为 Tf 会把 msgKey 参数交给
// fmt.Sprintf 打印成 `act.writeAutoStart` 这样的键名。
func (a *App) render(k msgKey, args ...any) string {
	if len(args) == 0 {
		return a.T(k)
	}
	return fmt.Sprintf(a.T(k), renderArgs(a.T(k), catalogFor(a.lang()), args)...)
}

// transformErrKey 把 transform 包的"失败种类"映射到文案键。
//
// 这张表是**前后端之间的契约**：后端返回 kind，中文用户与英文用户各看到
// 自己语言的句子。认不出的 kind 落到 msgConvFailed（"转换失败"），
// 而不是把英文原文抛给中文用户。
func transformErrKey(kind string) msgKey {
	switch kind {
	case transform.KindInvalidJSON:
		return msgConvInvalidJSON
	case transform.KindMultipleJSON:
		return msgConvMultipleJSON
	case transform.KindInvalidBase64:
		return msgConvInvalidB64
	case transform.KindBinaryResult:
		return msgConvBinaryResult
	case transform.KindMultipleLines:
		return msgConvMultipleLines
	case transform.KindTooLarge:
		return msgConvTooLarge
	case transform.KindEmpty:
		return msgConvEmpty
	case transform.KindUnknownOp:
		return msgConvUnknownOp
	default:
		return msgConvFailed
	}
}

// ── 导出 / 导入报告（§14 第 24 条的"结果报告"）─────────────────
//
// 这两个函数的产物有两个去处：
//
//	① 系统通知的正文（后端自己渲染，前端看不见，所以必须走目录）；
//	② pushEvent(EventNotice, …)，由前端弹成一句 toast。
//
// 一行式的写法是刻意的：系统通知的正文会被截断到一两行，
// 多行报告在那里等于没写。

// exportReport 把一次导出的结果压成一句话。
func (a *App) exportReport(r *ExportResult) string {
	if r == nil {
		return ""
	}
	s := fmt.Sprintf("%s %d · %s %d · %s %.1fs",
		a.T(msgReportItems), r.Items,
		a.T(msgReportBlobs), r.Blobs,
		a.T(msgReportTook), float64(r.TookMs)/1000,
	)
	// 草稿只在 scope=full 时进包，非 full 时这个数字必然是 0。
	// 仍然只在非 0 时补一句：报告是一行式的（见本文件开头），
	// 而"草稿 0"对用户没有任何信息量。
	if r.Drafts > 0 {
		s += fmt.Sprintf(" · %s %d", a.T(msgReportDrafts), r.Drafts)
	}
	if n := len(r.Warnings); n > 0 {
		s += fmt.Sprintf(" · %s %d", a.T(msgReportWarnings), n)
	}
	return s
}

// importReport 把一次导入的结果压成一句话。
func (a *App) importReport(r *ImportResult) string {
	if r == nil {
		return ""
	}
	parts := []string{
		fmt.Sprintf("%s %d", a.T(msgReportInserted), r.Inserted),
	}
	if r.DraftsImported > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", a.T(msgReportDrafts), r.DraftsImported))
	}
	if r.Merged > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", a.T(msgReportMerged), r.Merged))
	}
	if r.Overwritten > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", a.T(msgReportOverwritten), r.Overwritten))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", a.T(msgReportSkipped), r.Skipped))
	}
	if n := len(r.Errors); n > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", a.T(msgReportErrors), n))
	}
	return strings.Join(parts, " · ")
}

// readmePreamble 生成备份包里 README.txt 的开头那段提示。
//
// 这三句是 §14 第 24 条点名的"备份包内 README.txt"那一处：
// 它是**给人看的**（"这是什么 / 怎么恢复 / 别外传"），所以要跟着界面语言走。
func (a *App) readmePreamble() string {
	return strings.Join([]string{
		a.T(msgBackupReadmeTitle),
		a.T(msgBackupReadmeHowTo),
		a.T(msgBackupReadmeWarning),
	}, "\n")
}
