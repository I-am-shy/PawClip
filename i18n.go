package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// 后端字符串目录（docs/DESIGN.md §14 第 24 条点名的"6 个易漏位置"）。
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
	msgTrayShow          msgKey = "tray.show"
	msgTrayResume        msgKey = "tray.resume"
	msgTrayPause         msgKey = "tray.pause"
	msgTraySettings      msgKey = "tray.settings"
	msgTrayStats         msgKey = "tray.stats"
	msgTrayBackup        msgKey = "tray.backup"
	msgTrayAbout         msgKey = "tray.about"
	msgTrayQuit          msgKey = "tray.quit"
	msgTrayTooltip       msgKey = "tray.tooltip"
	msgNotifyCaptureOff  msgKey = "notify.captureOff"
	msgNotifyCaptureOn   msgKey = "notify.captureOn"
	msgNotifyExportDone  msgKey = "notify.exportDone"
	msgNotifyImportDone  msgKey = "notify.importDone"
	msgNotifyHotkeyTaken msgKey = "notify.hotkeyTaken"
	// msgNotifyHotkeyInvalid 与 msgNotifyHotkeyTaken 是两件事：
	// 一个说"这个组合被别人占了"，一个说"这个组合根本不是合法的热键"。
	// 混成一句会让用户去关别的程序，而真正的问题是他填错了键名。
	msgNotifyHotkeyInvalid msgKey = "notify.hotkeyInvalid"
	msgReportItems         msgKey = "report.items"
	msgReportBlobs         msgKey = "report.blobs"
	msgReportTook          msgKey = "report.took"
	msgReportWarnings      msgKey = "report.warnings"
	msgReportErrors        msgKey = "report.errors"
	msgReportSkipped       msgKey = "report.skipped"
	msgReportMerged        msgKey = "report.merged"
	msgReportOverwritten   msgKey = "report.overwritten"
	msgReportInserted      msgKey = "report.inserted"
	// msgReportDrafts 用在导出与导入两处报告里：两边都要报草稿条数，
	// 否则用户导出后看到的"条目 12"里到底有没有算草稿，只能靠猜。
	msgReportDrafts        msgKey = "report.drafts"
	msgBackupReadmeTitle   msgKey = "backup.readme.title"
	msgBackupReadmeHowTo   msgKey = "backup.readme.howTo"
	msgBackupReadmeWarning msgKey = "backup.readme.warning"
	msgPasteAccessibility  msgKey = "paste.needsAccessibility"
	msgPasteDegraded       msgKey = "paste.degraded"

	// ── 回写路径上"给用户看的一句话"（§14 第 24 条的"错误提示"）──
	//
	// 这些曾经是 writer.go 里的中文字面量。问题不是"不够优雅"，而是
	// **界面选英文时它们仍然是中文**。凡是要显示给用户的话，都必须能
	// 按当前语言取到，所以它们一律进这份目录。
	msgNoteRTFMissing     msgKey = "note.rtfMissing"
	msgNoteImageMissing   msgKey = "note.imageMissing"
	msgNoteSnapshotFailed msgKey = "note.snapshotFailed"
	msgNoteAutoPasteFail  msgKey = "note.autoPasteFailed"
	msgNoteRestoreFailed  msgKey = "note.restoreFailed"
	msgErrItemTrashed     msgKey = "err.itemTrashed"
	msgErrNoContent       msgKey = "err.noContent"
	msgErrNoPlainText     msgKey = "err.noPlainText"
	msgErrEmptyText       msgKey = "err.emptyText"
	msgErrSeqEmpty        msgKey = "err.seqEmpty"
	msgErrSeqDone         msgKey = "err.seqDone"

	// ── 绑定层把"预期内的失败"翻成用户语言 ──
	//
	// store / panel 这些包里返回的错误是**开发者视角**的一句话
	// （`store: no such item`）。用户看到它等于看到一句天书，
	// 所以在绑定层（进程边界）统一翻一道：认出已知的哨兵错误就换成
	// 本地化文案，认不出来才原样透出——那时的文案是诊断信息，不是提示。
	msgErrNotFound      msgKey = "err.notFound"
	msgErrNotReady      msgKey = "err.notReady"
	msgErrCategoryTaken msgKey = "err.categoryTaken"
	msgErrTagTaken      msgKey = "err.tagTaken"
	msgErrNoPanel       msgKey = "err.noPanel"
	msgErrNotClipbak    msgKey = "err.notClipbak"
	msgErrNoBlobs       msgKey = "err.blobsNotReady"
	msgErrNoGC          msgKey = "err.gcNotRunning"

	// ── 连续粘贴的进度提示（托盘/通知语）──
	msgNotifySeqStarted  msgKey = "notify.seqStarted"
	msgNotifySeqNext     msgKey = "notify.seqNext"
	msgNotifySeqFinished msgKey = "notify.seqFinished"

	// ── 内容转换器失败的原因（docs/DESIGN.md §11 P2）──
	//
	// 一条一个键，与 transform 包的 Kind 一一对应（见 transformErrKey）。
	// 为什么不让 transform 包直接返回中文/英文：那个包是纯逻辑，
	// 它给"种类"，语言由这里决定。
	msgConvInvalidJSON   msgKey = "conv.invalidJSON"
	msgConvMultipleJSON  msgKey = "conv.multipleJSON"
	msgConvInvalidB64    msgKey = "conv.invalidBase64"
	msgConvBinaryResult  msgKey = "conv.binaryResult"
	msgConvMultipleLines msgKey = "conv.multipleLines"
	msgConvTooLarge      msgKey = "conv.tooLarge"
	msgConvEmpty         msgKey = "conv.empty"
	msgConvUnknownOp     msgKey = "conv.unknownOp"
	msgConvFailed        msgKey = "conv.failed"

	// 系统对话框的标题与过滤器名。它们由原生弹窗显示，**前端看不见**，
	// 所以必须走这份后端目录——这正是 §14 第 24 条点名的"易漏位置"。
	//
	// 过滤器名（"PawClip 备份包 (*.clipbak)"）尤其容易漏：它看起来像
	// 一句"给系统看的格式说明"，其实是**用户会读到的一句话**。
	msgPickExportDir        msgKey = "dialog.pickExportDir"
	msgPickBackupFile       msgKey = "dialog.pickBackupFile"
	msgDialogBackupFilter   msgKey = "dialog.backupFilter"
	msgDialogAllFilesFilter msgKey = "dialog.allFilesFilter"

	// ── 从平台层/工具层冒出来、最终显示在界面上的错误 ──
	//
	// 这一组与上面 msgErr* 的区别：它们是在**够不着翻译器的位置**产生的
	// （dialogs.go / autostart*.go / reveal_*.go 里很多是包级函数，
	// 不是 *App 的方法）。解决办法是让那一层只声明"用哪条文案"
	// （msgf(key, cause, args...)），渲染推迟到 localizeErr 那里做。
	// 细节见 messages.go 的 msgErr。
	msgErrUINotReady    msgKey = "err.uiNotReady"
	msgErrEmptyPath     msgKey = "err.emptyPath"
	msgErrOutsideData   msgKey = "err.outsideDataDir"
	msgErrCreateExpDir  msgKey = "err.createExportDir"
	msgErrBackupChanged msgKey = "err.backupChanged"
	msgErrOpenBackup    msgKey = "err.openBackup"
	msgErrBackupIsDir   msgKey = "err.backupIsDir"
	msgErrRevealFailed  msgKey = "err.revealFailed"
	msgErrPathMissing   msgKey = "err.pathMissing"
	msgErrNoFileManager msgKey = "err.noFileManager"
	// 打开外部链接（草稿正文里点一条链接）用得到的两条。
	// 白名单这条是**给被绕过的前端兜底**的：前端的 safeHref 是第一道，
	// 但这里往外递的是"交给系统执行的东西"，不能只看前端的脸色。
	msgErrOpenURLBad    msgKey = "err.openURLUnsupported"
	msgErrOpenURLFail   msgKey = "err.openURLFailed"
	msgErrClipboardWr   msgKey = "err.clipboardWrite"
	msgErrSettingsLoad  msgKey = "err.settingsReload"
	msgErrAutoStartNo   msgKey = "err.autostartUnsupported"
	msgErrAutoStartFail msgKey = "err.autostartFailed"
	msgErrNoHomeDir     msgKey = "err.noHomeDir"
	msgErrNoExePath     msgKey = "err.noExePath"

	// 开机自启各步骤的"动作名"。
	//
	// 它们会作为 msgErrAutoStartFail 的 %s 参数出现
	// （"「写入开机自启项」失败：…"）。也就是说它们本身也是用户要读的文案，
	// 不能写成中文字面量——否则英文界面会冒出"Failed to 「写入开机自启项」"。
	msgActReadAutoStart  msgKey = "act.readAutoStart"
	msgActWriteAutoStart msgKey = "act.writeAutoStart"
	msgActDeleteStart    msgKey = "act.deleteAutoStart"
	msgActRegistry       msgKey = "act.registry"
	msgActLaunchDir      msgKey = "act.launchAgentsDir"

	// 启动期"配置坏了，先用默认值跑"。
	//
	// 它必须存在，是因为原来的行为是**静默退出**（见 main.go 的长注释）：
	// 用户双击图标，什么都不会发生。这条文案是那个场景下唯一能给用户看的
	// 一句话，所以它既是提示、也是"程序还活着"的证据。
	msgBootConfigBroken msgKey = "boot.configBroken"

	// 草稿本（docs/DESIGN.md §4.4）。
	//
	// msgDraftDefaultName 是"新建草稿时的默认名"。它**在后端渲染、
	// 只在创建那一刻**写进库：之后用户切语言，草稿名不会集体跳变——
	// 名字是内容的一部分（用户可以改它），不是界面文案。
	msgDraftDefaultName    msgKey = "draft.defaultName"
	msgErrDraftGone        msgKey = "err.draftGone"
	msgErrDraftImageTooBig msgKey = "err.draftImageTooBig"
	// msgErrDraftImageType 与 msgErrDraftImageBroken 是两件事，不能合并：
	// 前者是"解得开，但不是支持的图片格式"，后者是"base64 就没解开"。
	// 用户贴了一段 HTML 或纯文本时走的是后者——那时说"格式不支持"会把
	// 他引去查图片格式，而他压根没贴图片。
	msgErrDraftImageType   msgKey = "err.draftImageType"
	msgErrDraftImageBroken msgKey = "err.draftImageBroken"
	msgErrLastView         msgKey = "err.lastView"

	// 启动期读配置的三处失败。
	//
	// 它们不再是"开发者日志"了：main.go 现在把它们包进
	// msgBootConfigBroken 显示在界面横幅上，所以必须能翻译。
	msgErrConfigDir   msgKey = "err.configDir"
	msgErrConfigRead  msgKey = "err.configRead"
	msgErrConfigParse msgKey = "err.configParse"
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
	msgNotifyHotkeyInvalid,
	msgReportItems,
	msgReportBlobs,
	msgReportTook,
	msgReportWarnings,
	msgReportErrors,
	msgReportSkipped,
	msgReportMerged,
	msgReportOverwritten,
	msgReportInserted,
	msgReportDrafts,
	msgBackupReadmeTitle,
	msgBackupReadmeHowTo,
	msgBackupReadmeWarning,
	msgPasteAccessibility,
	msgPasteDegraded,
	msgNoteRTFMissing,
	msgNoteImageMissing,
	msgNoteSnapshotFailed,
	msgNoteAutoPasteFail,
	msgNoteRestoreFailed,
	msgErrItemTrashed,
	msgErrNoContent,
	msgErrNoPlainText,
	msgErrEmptyText,
	msgErrSeqEmpty,
	msgErrSeqDone,
	msgErrNotFound,
	msgErrNotReady,
	msgErrCategoryTaken,
	msgErrTagTaken,
	msgErrNoPanel,
	msgErrNotClipbak,
	msgErrNoBlobs,
	msgErrNoGC,
	msgNotifySeqStarted,
	msgNotifySeqNext,
	msgNotifySeqFinished,
	msgConvInvalidJSON,
	msgConvMultipleJSON,
	msgConvInvalidB64,
	msgConvBinaryResult,
	msgConvMultipleLines,
	msgConvTooLarge,
	msgConvEmpty,
	msgConvUnknownOp,
	msgConvFailed,
	msgPickExportDir,
	msgPickBackupFile,
	msgDialogBackupFilter,
	msgDialogAllFilesFilter,
	msgErrUINotReady,
	msgErrEmptyPath,
	msgErrOutsideData,
	msgErrCreateExpDir,
	msgErrBackupChanged,
	msgErrOpenBackup,
	msgErrBackupIsDir,
	msgErrRevealFailed,
	msgErrPathMissing,
	msgErrNoFileManager,
	msgErrOpenURLBad,
	msgErrOpenURLFail,
	msgErrClipboardWr,
	msgErrSettingsLoad,
	msgErrAutoStartNo,
	msgErrAutoStartFail,
	msgErrNoHomeDir,
	msgErrNoExePath,
	msgActReadAutoStart,
	msgActWriteAutoStart,
	msgActDeleteStart,
	msgActRegistry,
	msgActLaunchDir,
	msgBootConfigBroken,
	msgErrConfigDir,
	msgErrConfigRead,
	msgErrConfigParse,
	msgDraftDefaultName,
	msgErrDraftGone,
	msgErrDraftImageTooBig,
	msgErrDraftImageType,
	msgErrDraftImageBroken,
	msgErrLastView,
}

// catalog 是全部后端字符串。结构是 map[Lang]map[msgKey]string。
//
// 形状刻意做成"两张平表"而不是嵌套的 struct：漏翻译一项时，
// 回退逻辑能按 key 逐个降级；用 struct 字段的话漏一项是编译期问题，
// 但新增语言要动所有 struct。
var catalog = map[Lang]map[msgKey]string{
	LangZhCN: {
		msgTrayShow:             "显示 PawClip",
		msgTrayResume:           "继续记录",
		msgTrayPause:            "暂停记录",
		msgTraySettings:         "设置…",
		msgTrayStats:            "统计…",
		msgTrayBackup:           "导出 / 导入…",
		msgTrayAbout:            "关于 PawClip",
		msgTrayQuit:             "退出",
		msgTrayTooltip:          "PawClip · 剪贴板历史",
		msgNotifyCaptureOff:     "已暂停记录",
		msgNotifyCaptureOn:      "已恢复记录",
		msgNotifyExportDone:     "导出完成",
		msgNotifyImportDone:     "导入完成",
		msgNotifyHotkeyTaken:    "全局热键已被其他应用占用",
		msgNotifyHotkeyInvalid:  "「%s」不能当作全局热键（至少要有一个修饰键，主键得是字母、数字、F1-F12 或方向/功能键）",
		msgReportItems:          "条目",
		msgReportBlobs:          "图片 / 附件",
		msgReportTook:           "耗时",
		msgReportWarnings:       "警告",
		msgReportErrors:         "错误",
		msgReportSkipped:        "跳过",
		msgReportMerged:         "合并",
		msgReportOverwritten:    "覆盖",
		msgReportInserted:       "新增",
		msgReportDrafts:         "草稿",
		msgBackupReadmeTitle:    "PawClip 备份包",
		msgBackupReadmeHowTo:    "在 PawClip 里用「导出 / 导入 → 导入」选择本文件即可恢复。",
		msgBackupReadmeWarning:  "本包含有你复制过的内容（可能包括密码、验证码）。请妥善保管，不要随手分享。",
		msgPasteAccessibility:   "需要在「系统设置 → 隐私与安全性 → 辅助功能」里勾选 PawClip 才能自动粘贴",
		msgPasteDegraded:        "没有「辅助功能」授权，已降级为只复制到剪贴板",
		msgNoteRTFMissing:       "RTF 内容已丢失，本次只写文本",
		msgNoteImageMissing:     "图片文件已丢失，本次不带图片",
		msgNoteSnapshotFailed:   "原剪贴板未能读取，粘贴后不恢复",
		msgNoteAutoPasteFail:    "模拟粘贴失败，内容已复制到剪贴板，请手动粘贴",
		msgNoteRestoreFailed:    "原剪贴板恢复失败",
		msgErrItemTrashed:       "这条在回收站里（#%d），先恢复再使用",
		msgErrNoContent:         "这条没有可写回的内容（#%d）",
		msgErrNoPlainText:       "这条没有纯文本内容，无法去格式粘贴",
		msgErrEmptyText:         "结果是空的，没有可粘贴的内容",
		msgErrSeqEmpty:          "连续粘贴需要至少选中一条内容",
		msgErrSeqDone:           "连续粘贴队列已经空了",
		msgErrNotFound:          "找不到这条记录，它可能已经被删除",
		msgErrNotReady:          "PawClip 还没准备好（初始化未完成或已失败）",
		msgErrCategoryTaken:     "已经有同名的分类了",
		msgErrTagTaken:          "已经有同名的标签了",
		msgErrNoPanel:           "当前平台不支持免抢焦点面板",
		msgErrNotClipbak:        "这不是一个 .clipbak 备份包",
		msgErrNoBlobs:           "附件的存储层还没就绪",
		msgErrNoGC:              "回收任务还没启动",
		msgNotifySeqStarted:     "连续粘贴：已开始，还剩 %d 条",
		msgNotifySeqNext:        "连续粘贴：还剩 %d 条",
		msgNotifySeqFinished:    "连续粘贴：已经贴完全部内容",
		msgConvInvalidJSON:      "内容不是合法的 JSON",
		msgConvMultipleJSON:     "内容里有多个 JSON 值，不知道要处理哪一个",
		msgConvInvalidB64:       "内容不是合法的 Base64",
		msgConvBinaryResult:     "解出来不是文本（可能是一段二进制内容）",
		msgConvMultipleLines:    "这个转换只支持单行内容",
		msgConvTooLarge:         "内容太大，转换被拒绝",
		msgConvEmpty:            "内容里没有可转换的文本",
		msgConvUnknownOp:        "不认识这个转换（可能来自更新的版本）",
		msgConvFailed:           "转换失败",
		msgPickExportDir:        "选择导出位置",
		msgPickBackupFile:       "选择要导入的备份包（.clipbak）",
		msgDialogBackupFilter:   "PawClip 备份包 (*.clipbak)",
		msgDialogAllFilesFilter: "所有文件 (*.*)",
		msgErrUINotReady:        "界面还没准备好，请稍后再试",
		msgErrEmptyPath:         "路径是空的",
		msgErrOutsideData:       "只能在 PawClip 自己的数据目录里定位文件",
		msgErrCreateExpDir:      "建不了导出目录：%v",
		msgErrBackupChanged:     "这个包在预检之后被改动过，请重新检查一遍",
		msgErrOpenBackup:        "打不开这个备份包：%v",
		msgErrBackupIsDir:       "选中的是一个目录，不是备份包",
		msgErrRevealFailed:      "在文件管理器里显示失败：%v",
		msgErrPathMissing:       "这个路径不存在了：%v",
		msgErrNoFileManager:     "这个系统上没有可用的文件管理器",
		msgErrOpenURLBad:        "只能用系统默认程序打开 http / https / mailto 链接",
		msgErrOpenURLFail:       "打开链接失败：%v",
		msgErrClipboardWr:       "写剪贴板失败：%v",
		msgErrSettingsLoad:      "设置写进去了，但重新读取失败：%v",
		msgErrAutoStartNo:       "当前系统不支持开机自启",
		msgErrAutoStartFail:     "「%s」失败：%v",
		msgErrNoHomeDir:         "找不到用户主目录：%v",
		msgErrNoExePath:         "确定不了程序自身的位置：%v",
		msgActReadAutoStart:     "读取开机自启状态",
		msgActWriteAutoStart:    "写入开机自启项",
		msgActDeleteStart:       "删除开机自启项",
		msgActRegistry:          "访问注册表 Run 键",
		msgActLaunchDir:         "创建 LaunchAgents 目录",
		msgBootConfigBroken:     "这次先用默认设置跑。原因：%v（修好 config.toml 后重启即可恢复）",
		msgErrConfigDir:         "确定不了用户配置目录：%v",
		msgErrConfigRead:        "读不了配置文件 %s：%v",
		msgErrConfigParse:       "配置文件 %s 语法有错：%v",
		msgDraftDefaultName:     "草稿 %d",
		msgErrDraftGone:         "这条草稿已经被删除了",
		msgErrDraftImageTooBig:  "图片太大（上限 %s）",
		msgErrDraftImageType:    "这个图片格式不支持",
		msgErrDraftImageBroken:  "这段内容不是图片（读不出图片数据）",
		msgErrLastView:          "记不住「上次打开的页面」：%v",
	},
	LangEn: {
		msgTrayShow:             "Show PawClip",
		msgTrayResume:           "Resume capture",
		msgTrayPause:            "Pause capture",
		msgTraySettings:         "Settings…",
		msgTrayStats:            "Statistics…",
		msgTrayBackup:           "Export / Import…",
		msgTrayAbout:            "About PawClip",
		msgTrayQuit:             "Quit",
		msgTrayTooltip:          "PawClip · clipboard history",
		msgNotifyCaptureOff:     "Capture paused",
		msgNotifyCaptureOn:      "Capture resumed",
		msgNotifyExportDone:     "Export finished",
		msgNotifyImportDone:     "Import finished",
		msgNotifyHotkeyTaken:    "Global hotkey is taken by another app",
		msgNotifyHotkeyInvalid:  "\"%s\" cannot be a global hotkey (it needs at least one modifier, and the main key must be a letter, a digit, F1-F12, or a navigation key)",
		msgReportItems:          "Items",
		msgReportBlobs:          "Images / attachments",
		msgReportTook:           "Took",
		msgReportWarnings:       "Warnings",
		msgReportErrors:         "Errors",
		msgReportSkipped:        "Skipped",
		msgReportMerged:         "Merged",
		msgReportOverwritten:    "Overwritten",
		msgReportInserted:       "Inserted",
		msgReportDrafts:         "Drafts",
		msgBackupReadmeTitle:    "PawClip backup",
		msgBackupReadmeHowTo:    "Open PawClip, choose Export / Import → Import, and pick this file to restore.",
		msgBackupReadmeWarning:  "This archive contains things you have copied (possibly passwords or codes). Keep it private.",
		msgPasteAccessibility:   "Grant PawClip access in System Settings → Privacy & Security → Accessibility to auto-paste",
		msgPasteDegraded:        "No Accessibility permission; falling back to copy-only",
		msgNoteRTFMissing:       "The RTF copy was lost; pasting text only",
		msgNoteImageMissing:     "The image file was lost; pasting without it",
		msgNoteSnapshotFailed:   "Could not read the previous clipboard, so it will not be restored",
		msgNoteAutoPasteFail:    "Simulated paste failed; the content is on the clipboard, paste it manually",
		msgNoteRestoreFailed:    "Could not restore the previous clipboard",
		msgErrItemTrashed:       "Item #%d is in the trash — restore it first",
		msgErrNoContent:         "Item #%d has nothing to write back",
		msgErrNoPlainText:       "This item has no plain text, so it cannot be pasted without formatting",
		msgErrEmptyText:         "The result is empty, so there is nothing to paste",
		msgErrSeqEmpty:          "Sequential paste needs at least one selected item",
		msgErrSeqDone:           "The sequential paste queue is empty",
		msgErrNotFound:          "That item no longer exists — it may have been deleted",
		msgErrNotReady:          "PawClip is not ready yet (startup incomplete or failed)",
		msgErrCategoryTaken:     "A category with that name already exists",
		msgErrTagTaken:          "A tag with that name already exists",
		msgErrNoPanel:           "This platform has no focus-preserving panel",
		msgErrNotClipbak:        "That is not a .clipbak archive",
		msgErrNoBlobs:           "The attachment store is not ready",
		msgErrNoGC:              "The reclamation job has not started",
		msgNotifySeqStarted:     "Sequential paste: started, %d left",
		msgNotifySeqNext:        "Sequential paste: %d left",
		msgNotifySeqFinished:    "Sequential paste: everything has been pasted",
		msgConvInvalidJSON:      "That is not valid JSON",
		msgConvMultipleJSON:     "There are several JSON values here; cannot tell which to use",
		msgConvInvalidB64:       "That is not valid Base64",
		msgConvBinaryResult:     "The result is not text (it may be binary data)",
		msgConvMultipleLines:    "This conversion only works on a single line",
		msgConvTooLarge:         "The content is too large; the conversion was refused",
		msgConvEmpty:            "There is no text here to convert",
		msgConvUnknownOp:        "Unknown conversion (possibly from a newer build)",
		msgConvFailed:           "Conversion failed",
		msgPickExportDir:        "Choose where to export",
		msgPickBackupFile:       "Choose a backup archive to import (.clipbak)",
		msgDialogBackupFilter:   "PawClip backups (*.clipbak)",
		msgDialogAllFilesFilter: "All files (*.*)",
		msgErrUINotReady:        "The interface is not ready yet — try again in a moment",
		msgErrEmptyPath:         "The path is empty",
		msgErrOutsideData:       "Only paths inside PawClip's own data folder can be revealed",
		msgErrCreateExpDir:      "Could not create the export folder: %v",
		msgErrBackupChanged:     "The archive changed after the pre-check — check it again",
		msgErrOpenBackup:        "Could not open the backup archive: %v",
		msgErrBackupIsDir:       "That is a folder, not a backup archive",
		msgErrRevealFailed:      "Could not show it in the file manager: %v",
		msgErrPathMissing:       "That path no longer exists: %v",
		msgErrNoFileManager:     "No file manager is available on this system",
		msgErrOpenURLBad:        "Only http / https / mailto links can be opened with the default app",
		msgErrOpenURLFail:       "Could not open the link: %v",
		msgErrClipboardWr:       "Could not write to the clipboard: %v",
		msgErrSettingsLoad:      "The setting was saved, but reloading it failed: %v",
		msgErrAutoStartNo:       "This system does not support launching at login",
		msgErrAutoStartFail:     "Failed to %s: %v",
		msgErrNoHomeDir:         "Could not find your home folder: %v",
		msgErrNoExePath:         "Could not determine where the app itself lives: %v",
		msgActReadAutoStart:     "read the login item state",
		msgActWriteAutoStart:    "write the login item",
		msgActDeleteStart:       "remove the login item",
		msgActRegistry:          "open the registry Run key",
		msgActLaunchDir:         "create the LaunchAgents folder",
		msgBootConfigBroken:     "Running with default settings this time. Reason: %v (fix config.toml and restart to recover)",
		msgErrConfigDir:         "Could not determine your config folder: %v",
		msgErrConfigRead:        "Could not read the config file %s: %v",
		msgErrConfigParse:       "The config file %s has a syntax error: %v",
		msgDraftDefaultName:     "Draft %d",
		msgErrDraftGone:         "This draft has been deleted",
		msgErrDraftImageTooBig:  "That image is too large (limit %s)",
		msgErrDraftImageType:    "That image format is not supported",
		msgErrDraftImageBroken:  "That content isn't an image (no image data could be read)",
		msgErrLastView:          "Could not remember the last opened page: %v",
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
