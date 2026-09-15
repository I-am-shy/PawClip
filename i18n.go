package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// 后端字符串目录（DESIGN §14 第 24 条点名的"6 个易漏位置"）。
//
// 前端有自己的 i18n（frontend/src/i18n.ts），但下面这些字符串**不经过前端**：
//
//   - 托盘菜单项
//   - 系统通知的标题与正文
//   - 导出/导入完成后的报告（还会写进备份包里的报告文件）
//   - 启动/初始化错误提示
//   - 备份包内 README.txt
//   - 导出文件名里的分类名
//
// 第 24 条之所以把它们单列，就是因为做 i18n 时最容易只改前端、漏掉这六处，
// 结果出现"界面是中文、菜单栏是英文"的割裂。
//
// 语言解析顺序（§9 `ui.language`）：本机设置 → 系统区域 → 回退 en。
// 这里只实现前两级的取值，设置那级由调用方把 ui.language 传进来。

// Lang 是受支持的语言标签。
type Lang string

const (
	LangSystem Lang = "system"
	LangZhCN   Lang = "zh-CN"
	LangEn     Lang = "en"
)

// msgKey 是字符串键。用字符串而不是 iota 常量，是为了让缺项的失败模式
// 是"回退到英文"而不是"打印一个数字"。
type msgKey string

const (
	msgTrayShow            msgKey = "tray.show"
	msgTrayResume          msgKey = "tray.resume"
	msgTrayPause           msgKey = "tray.pause"
	msgTraySettings        msgKey = "tray.settings"
	msgTrayStats           msgKey = "tray.stats"
	msgTrayBackup          msgKey = "tray.backup"
	msgTrayAbout           msgKey = "tray.about"
	msgTrayQuit            msgKey = "tray.quit"
	msgTrayTooltip         msgKey = "tray.tooltip"
	msgNotifyCaptureOff    msgKey = "notify.captureOff"
	msgNotifyCaptureOn     msgKey = "notify.captureOn"
	msgNotifyExportDone    msgKey = "notify.exportDone"
	msgNotifyImportDone    msgKey = "notify.importDone"
	msgNotifyHotkeyTaken   msgKey = "notify.hotkeyTaken"
	msgStartupFailed       msgKey = "startup.failed"
	msgReportExportTitle   msgKey = "report.export.title"
	msgReportImportTitle   msgKey = "report.import.title"
	msgReportItems         msgKey = "report.items"
	msgReportBlobs         msgKey = "report.blobs"
	msgReportTook          msgKey = "report.took"
	msgReportWarnings      msgKey = "report.warnings"
	msgReportErrors        msgKey = "report.errors"
	msgReportSkipped       msgKey = "report.skipped"
	msgReportMerged        msgKey = "report.merged"
	msgReportOverwritten   msgKey = "report.overwritten"
	msgReportInserted      msgKey = "report.inserted"
	msgBackupReadmeTitle   msgKey = "backup.readme.title"
	msgBackupReadmeHowTo   msgKey = "backup.readme.howTo"
	msgBackupReadmeWarning msgKey = "backup.readme.warning"
	msgPasteAccessibility  msgKey = "paste.needsAccessibility"
	msgPasteDegraded       msgKey = "paste.degraded"
	msgExpiredRestore      msgKey = "trash.restoreConflict"
)

// allMsgKeys 是全部 msgKey 的清单。
//
// 它是**刻意手工维护**的，而不是靠反射或代码生成：新增一个 msgKey 却忘了
// 在某一门语言里补翻译，是"后端字符串"这类 i18n 最常见的 bug（§14 第 24 条
// 之所以把这些位置单列，就是因为它们最容易漏）。有一份显式清单，
// 单测就能断言"两张表都恰好覆盖这份清单"，漏一项立刻红。
//
// 换句话说：维护这份清单的那点麻烦，换的是"漏翻译不可能发出去"。
var allMsgKeys = []msgKey{
	msgTrayShow,
	msgTrayResume,
	msgTrayPause,
	msgTraySettings,
	msgTrayStats,
	msgTrayBackup,
	msgTrayAbout,
	msgTrayQuit,
	msgTrayTooltip,
	msgNotifyCaptureOff,
	msgNotifyCaptureOn,
	msgNotifyExportDone,
	msgNotifyImportDone,
	msgNotifyHotkeyTaken,
	msgStartupFailed,
	msgReportExportTitle,
	msgReportImportTitle,
	msgReportItems,
	msgReportBlobs,
	msgReportTook,
	msgReportWarnings,
	msgReportErrors,
	msgReportSkipped,
	msgReportMerged,
	msgReportOverwritten,
	msgReportInserted,
	msgBackupReadmeTitle,
	msgBackupReadmeHowTo,
	msgBackupReadmeWarning,
	msgPasteAccessibility,
	msgPasteDegraded,
	msgExpiredRestore,
}

// catalog 是全部后端字符串。结构是 map[Lang]map[msgKey]string。
//
// 形状刻意做成"两张平表"而不是嵌套的 struct：漏翻译一项时，
// 回退逻辑能按 key 逐个降级；用 struct 字段的话漏一项是编译期问题，
// 但新增语言要动所有 struct。
var catalog = map[Lang]map[msgKey]string{
	LangZhCN: {
		msgTrayShow:            "显示 PawClip",
		msgTrayResume:          "继续记录",
		msgTrayPause:           "暂停记录",
		msgTraySettings:        "设置…",
		msgTrayStats:           "统计…",
		msgTrayBackup:          "导出 / 导入…",
		msgTrayAbout:           "关于 PawClip",
		msgTrayQuit:            "退出",
		msgTrayTooltip:         "PawClip · 剪贴板历史",
		msgNotifyCaptureOff:    "已暂停记录",
		msgNotifyCaptureOn:     "已恢复记录",
		msgNotifyExportDone:    "导出完成",
		msgNotifyImportDone:    "导入完成",
		msgNotifyHotkeyTaken:   "全局热键已被其他应用占用",
		msgStartupFailed:       "PawClip 初始化失败，本次运行不会记录任何内容",
		msgReportExportTitle:   "PawClip 导出报告",
		msgReportImportTitle:   "PawClip 导入报告",
		msgReportItems:         "条目",
		msgReportBlobs:         "图片 / 附件",
		msgReportTook:          "耗时",
		msgReportWarnings:      "警告",
		msgReportErrors:        "错误",
		msgReportSkipped:       "跳过",
		msgReportMerged:        "合并",
		msgReportOverwritten:   "覆盖",
		msgReportInserted:      "新增",
		msgBackupReadmeTitle:   "PawClip 备份包",
		msgBackupReadmeHowTo:   "在 PawClip 里用「导出 / 导入 → 导入」选择本文件即可恢复。",
		msgBackupReadmeWarning: "本包含有你复制过的内容（可能包括密码、验证码）。请妥善保管，不要随手分享。",
		msgPasteAccessibility:  "需要在「系统设置 → 隐私与安全性 → 辅助功能」里勾选 PawClip 才能自动粘贴",
		msgPasteDegraded:       "没有「辅助功能」授权，已降级为只复制到剪贴板",
		msgExpiredRestore:      "这条的指纹已被新记录占用，无法恢复",
	},
	LangEn: {
		msgTrayShow:            "Show PawClip",
		msgTrayResume:          "Resume capture",
		msgTrayPause:           "Pause capture",
		msgTraySettings:        "Settings…",
		msgTrayStats:           "Statistics…",
		msgTrayBackup:          "Export / Import…",
		msgTrayAbout:           "About PawClip",
		msgTrayQuit:            "Quit",
		msgTrayTooltip:         "PawClip · clipboard history",
		msgNotifyCaptureOff:    "Capture paused",
		msgNotifyCaptureOn:     "Capture resumed",
		msgNotifyExportDone:    "Export finished",
		msgNotifyImportDone:    "Import finished",
		msgNotifyHotkeyTaken:   "Global hotkey is taken by another app",
		msgStartupFailed:       "PawClip failed to initialise; nothing will be recorded this run",
		msgReportExportTitle:   "PawClip export report",
		msgReportImportTitle:   "PawClip import report",
		msgReportItems:         "Items",
		msgReportBlobs:         "Images / attachments",
		msgReportTook:          "Took",
		msgReportWarnings:      "Warnings",
		msgReportErrors:        "Errors",
		msgReportSkipped:       "Skipped",
		msgReportMerged:        "Merged",
		msgReportOverwritten:   "Overwritten",
		msgReportInserted:      "Inserted",
		msgBackupReadmeTitle:   "PawClip backup",
		msgBackupReadmeHowTo:   "Open PawClip, choose Export / Import → Import, and pick this file to restore.",
		msgBackupReadmeWarning: "This archive contains things you have copied (possibly passwords or codes). Keep it private.",
		msgPasteAccessibility:  "Grant PawClip access in System Settings → Privacy & Security → Accessibility to auto-paste",
		msgPasteDegraded:       "No Accessibility permission; falling back to copy-only",
		msgExpiredRestore:      "Its fingerprint is now taken by a newer item, so it cannot be restored",
	},
}

var (
	langOnce sync.Once
	// sysLang 是"系统区域"级的结果，只解析一次（OS 语言在一次运行内不会变）。
	sysLang Lang
)

// DetectSystemLang 读系统语言并按 §9 的规则降级到 zh-CN / en。
//
// 只认识这两个值：设计上没有第三语言，与其把 "fr-FR" 传下去让每一层
// 各自决定回退到谁，不如在这里就归一化掉。
func DetectSystemLang() Lang {
	langOnce.Do(func() {
		raw := firstNonEmpty(
			os.Getenv("LC_ALL"),
			os.Getenv("LC_MESSAGES"),
			os.Getenv("LANG"),
		)
		// LANGUAGE 是 GNU 扩展，可能是 "zh_CN:en_US" 这样的列表。
		if v := os.Getenv("LANGUAGE"); v != "" {
			raw = v
		}

		switch {
		case strings.HasPrefix(strings.ToLower(raw), "zh"):
			sysLang = LangZhCN
		case raw == "":
			// 环境变量为空（macOS 图形程序常见：GUI 进程拿不到 shell 的 LANG）。
			// 这时**不能**默认成 en —— 而是交给上层按系统 API 判断；
			// 上层没结果时再回退 en（§9 的顺序）。
			sysLang = LangSystem
		default:
			sysLang = LangEn
		}
	})
	return sysLang
}

// resolveLang 按 §9 的顺序定出实际使用的一门语言。
func resolveLang(pref string) Lang {
	switch Lang(strings.TrimSpace(pref)) {
	case LangZhCN:
		return LangZhCN
	case LangEn:
		return LangEn
	}
	if l := DetectSystemLang(); l != LangSystem {
		return l
	}
	// macOS 上补一次系统 API 探测：GUI 进程的环境变量经常是空的，
	// 光看 LANG 会把中文用户的界面判成英文。
	if l := nativeSystemLang(); l != LangSystem {
		return l
	}
	return LangEn
}

// catalogFor 取某门语言的整张表。
func catalogFor(l Lang) map[msgKey]string {
	if m, ok := catalog[l]; ok {
		return m
	}
	return catalog[LangEn]
}

// T 是"翻译"。找不到的键**回退到英文**而不是返回 key：
// 用户看到一句英文，好过看到 `tray.pause`。
func (a *App) T(k msgKey) string {
	if s, ok := catalogFor(a.lang())[k]; ok {
		return s
	}
	if s, ok := catalog[LangEn][k]; ok {
		return s
	}
	return string(k)
}

// Tf 是带格式化参数的翻译。
func (a *App) Tf(k msgKey, args ...any) string {
	s := a.T(k)
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// lang 返回当前生效语言（每次调用都重新解析，因为设置可能被改）。
func (a *App) lang() Lang {
	pref := LangSystem
	if s := a.Settings(); s != nil && s.UI.Language != "" {
		pref = Lang(s.UI.Language)
	}
	return resolveLang(string(pref))
}

// Lang 把当前语言暴露给前端，让前端的 i18n 与后端用同一套判定。
func (a *App) Lang() string { return string(a.lang()) }

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
