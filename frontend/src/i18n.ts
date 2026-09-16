// 前端字符串目录。
//
// ⚠️ 后端另有一份（后端 i18n.go），两份**必须都存在**，不能合并：
// 后端的那些字符串不经过 WebView（托盘菜单、系统通知、导出报告、
// 备份包 README）。docs/DESIGN.md §14 第 24 条把它们单列成"易漏清单"就是这个原因。
//
// 语言来源：调用后端的 Lang()。**不在前端自己判断 navigator.language**
// 是因为语言解析顺序（§9：本机设置 → 系统区域 → 回退 en）里"系统区域"
// 在 macOS 上要读 AppleLocale，那是后端才能做的事。前端自己做一套必然
// 会与后端判定不一致，结果就是"托盘中文、界面英文"。

export type Lang = 'zh-CN' | 'en'

type Dict = Record<string, string>

const zh: Dict = {
  'app.name': 'PawClip',
  'app.tagline': '剪贴板历史',

  'nav.list': '历史',
  'nav.settings': '设置',
  'nav.stats': '统计',
  'nav.backup': '导出 / 导入',
  'nav.categories': '分类',
  'nav.tags': '标签',

  'search.placeholder': '搜索历史…（⌘⇧V 呼出）',
  'search.clear': '清空',
  'search.filterEmpty': '没有匹配的历史',
  'search.hint.title': '为什么搜不到？',
  'search.hint.fts': '全文检索可用（FTS5 trigram）',
  'search.hint.like': '全文检索不可用，已降级为逐行匹配（慢，且对两字词更明显）',
  'search.hint.mode': '本次走的是',

  'list.empty': '还没有任何记录。复制点什么试试。',
  'list.emptyTrashed': '回收站是空的。',
  'list.loading': '加载中…',
  'list.loadMore': '加载更多',
  'list.end': '到底了',
  'list.count': '共 {n} 条',
  'list.all': '全部',
  'list.uncategorized': '未分类',
  'list.countApprox': '{n}+ 条',
  'list.trash': '回收站',

  'kind.text': '文本',
  'kind.html': '富文本',
  'kind.image': '图片',
  'kind.files': '文件',
  'kind.rtf': '富文本',
  'kind.mixed': '混合',

  'item.pinned': '已置顶',
  'item.usedTimes': '用过 {n} 次',
  'item.files': '{n} 个文件',
  'item.chars': '{n} 字',
  'item.expiresAt': '过期于 {t}',
  'item.noExpiry': '永不主动过期',

  // 相对时间。**必须走词典而不是在 format.ts 里写死中文**：
  // 写死的话切到英文界面会冒出"3 分钟前"，而那正是最容易漏的位置
  // （docs/DESIGN.md §14 第 24 条说的是后端，前端同理）。
  'time.justNow': '刚刚',
  'time.minutesAgo': '{n} 分钟前',
  'time.hoursAgo': '{n} 小时前',
  'time.daysAgo': '{n} 天前',

  'action.paste': '粘贴',
  'action.copy': '只复制',
  'action.preview': '预览',
  'action.pin': '置顶',
  'action.unpin': '取消置顶',
  'action.delete': '删除',
  'action.restore': '恢复',
  'action.purge': '彻底删除',
  'action.more': '更多',

  'trash.empty': '清空回收站',
  'trash.emptyConfirm': '清空回收站？其中的条目将被彻底删除，无法恢复。',
  'trash.restored': '已恢复 {n} 条',
  'trash.conflict': '有 {n} 条因指纹冲突未能恢复',



  'settings.lang': '界面语言',
  'settings.lang.system': '跟随系统',
  'settings.theme': '外观',
  'settings.theme.system': '跟随系统',
  'settings.theme.light': '浅色',
  'settings.theme.dark': '深色',
  'settings.hotkey': '全局热键',
  'settings.hotkey.warn':
    '注意：全局热键会吞掉所有应用里的这个组合。⌘⇧V 在很多编辑器里是"粘贴并匹配样式"。',
  'settings.pasteMode': '点击历史时',
  'settings.pasteMode.clipboard': '只复制到剪贴板',
  'settings.pasteMode.autoPaste': '直接粘贴到前台应用',
  'settings.restoreClipboard': '粘贴后恢复原剪贴板',
  'settings.restoreDelayMs': '恢复延迟（毫秒）',
  'settings.quickPaste': '⌘/Ctrl + 1..N 直贴条数',
  'settings.closeOnBlur': '点击面板以外时自动收起',
  'settings.closeOnBlur.note': '点到别的应用、桌面或别的窗口时收起面板（点面板内部不受影响）。',
  'settings.idleDestroy': '面板静默多少秒后收起',
  'settings.idleDestroy.note':
    'DESIGN 要求的是"销毁"以释放 WebView 内存。当前 Wails 版本没有窗口销毁 API，' +
    '所以这里是"收起"。统计页会显示实测内存。',

  'settings.captureEnabled': '记录剪贴板',
  'settings.captureTypes': '记录的类型',
  'settings.imageMaxBytes': '超过这个大小的图片不记录',
  'settings.textMaxChars': '超长文本截断到多少字符',
  'settings.debounceMs': '连续变更合并窗口（毫秒）',
  'settings.pollActiveMs': '活跃时轮询间隔（毫秒，macOS）',
  'settings.pollIdleMs': '空闲时轮询间隔（毫秒，macOS）',
  'settings.idleThresholdSec': '多久算空闲（秒）',
  'settings.excludeApps': '不记录这些应用',
  'settings.excludeApps.note':
    '每行一条，支持 * 与 ? 通配。填 bundle id（com.1password.*）或应用名都可以。',
  'settings.excludePrivateTypes': '跳过标记为"请勿记录"的内容（密码管理器等）',

  'settings.retentionTitle': '保留策略',
  'settings.defaultTtlSec': '默认保留时长（秒，0 = 不限）',
  'settings.onExpire': '到期后',
  'settings.onExpire.trash': '移到回收站',
  'settings.onExpire.delete': '直接删除',
  'settings.onExpire.archive': '保留内容但不再自动过期',
  'settings.trashTtlSec': '回收站保留多久（秒）',
  'settings.maxItems': '最多保留多少条（0 = 不限）',
  'settings.maxDiskBytes': '最多占用多少字节（0 = 不限）',
  'settings.gcIntervalSec': '回收间隔（秒）',
  'settings.autostart': '开机自启',
  'settings.autostart.note': '开关显示的是**系统里的实际状态**，不是设置里的意图。',

  'settings.storage': '存储',
  'settings.cleanShutdownMarker': '用标记文件判断是否需要完整性检查',
  'settings.dataDir': '数据目录',
  'settings.configPath': '配置文件',
  'settings.openDataDir': '在访达/资源管理器中打开',
  'settings.saved': '已保存',
  'settings.saveFailed': '保存失败：{err}',
  'settings.uncovered': '以下设置项在界面上还没有控件，暂时只读：',

  'stats.title': '统计',
  'stats.alive': '存活条目',
  'stats.trashed': '回收站',
  'stats.all': '合计',
  'stats.aliveBytes': '条目登记占用',
  'stats.diskBytes': '图片实际占用',
  'stats.diskFiles': '图片文件数',
  'stats.events': '捕获次数',
  'stats.kinds': '按类型',
  'stats.topApps': '来源应用 Top',
  'stats.gc': '回收',
  'stats.gcRuns': '已执行轮次',
  'stats.gcRunNow': '立即回收',
  'stats.gcRunning': '回收中…',
  'stats.lastGC': '上一轮',
  'stats.paused': '回收已暂停（导出中）',
  'stats.memory': '内存',
  'stats.rss': '常驻内存',
  'stats.webContent': 'WebKit 内容进程',
  'stats.destroyYes': '支持',
  'stats.destroyNo': '不支持（见下方说明）',
  'stats.unknown': '未知',

  'backup.export': '导出',
  'backup.import': '导入',
  'backup.scope': '范围',
  'backup.scope.full': '全部',
  'backup.scope.favorites': '仅置顶',
  'backup.includeExpired': '包含已过期条目',
  'backup.embedFiles': '把文件类条目的文件也打进包里',
  'backup.start': '开始导出',
  'backup.exporting': '导出中…',
  'backup.exportDone': '导出完成',
  'backup.pickFile': '选择 .clipbak 文件…',
  'backup.prechecking': '预检中…',
  'backup.precheckTitle': '确认导入',
  'backup.total': '包内条目',
  'backup.willImport': '将导入',
  'backup.skipDup': '重复跳过',
  'backup.skipExpired': '已过期跳过',
  'backup.inserted': '真正新增',
  'backup.merged': '合并',
  'backup.overwritten': '覆盖',
  'backup.failed': '失败',
  'backup.invalid': '无法识别',
  'backup.conflictPolicy': '遇到本机已有的内容',
  'backup.conflict.merge': '合并（累加使用次数，保留本机分类）',
  'backup.conflict.skip': '跳过',
  'backup.conflict.overwrite': '覆盖（先删除本机那条）',
  'backup.expiryPolicy': '过期时间处理',
  'backup.expiry.keep': '保留包里的',
  'backup.expiry.rebase': '按导入时间重算',
  'backup.expiry.reset': '重置为不过期',
  'backup.alreadyImported': '这个包你之前导入过。',
  'backup.confirm': '确认导入',
  'backup.importing': '导入中…',
  'backup.importDone': '导入完成',
  'backup.rollback': '撤销上次导入',
  'backup.rollbackConfirm': '撤销上次导入？那批导进来的条目会被删除。',
  'backup.lastImport': '最近一次导入',
  'backup.noLastImport': '没有可撤销的导入批次',
  'backup.path': '路径',
  'backup.tookMs': '耗时',

  // ── 内容转换器（§11 P2）──
  'conv.title': '转换',
  'conv.plainText': '去格式贴纯文本',
  'conv.apply': '转换',
  'conv.result': '转换结果',
  'conv.copyResult': '复制结果',
  'conv.pasteResult': '粘贴结果',
  'conv.pastePlain': '去格式粘贴',
  'conv.empty': '这条没有文本可转换',
  'conv.json.beautify': 'JSON 美化',
  'conv.json.minify': 'JSON 压成一行',
  'conv.lines.removeBlank': '去掉空行',
  'conv.url.stripUTM': 'URL 去跟踪参数',
  'conv.base64.encode': 'Base64 编码',
  'conv.base64.decode': 'Base64 解码',
  'conv.case.upper': '全部大写',
  'conv.case.lower': '全部小写',
  'conv.case.title': '首字母大写',
  'conv.err.invalidJSON': '内容不是合法的 JSON',
  'conv.err.multipleJSON': '内容里有多个 JSON 值，不知道要处理哪一个',
  'conv.err.invalidBase64': '内容不是合法的 Base64',
  'conv.err.binaryResult': '解出来不是文本（可能是一段二进制内容）',
  'conv.err.multipleLines': '这个转换只支持单行内容',
  'conv.err.tooLarge': '内容太大，转换被拒绝',
  'conv.err.empty': '内容里没有可转换的文本',
  'conv.err.unknownOp': '不认识这个转换（可能来自更新的版本）',
  'conv.err.failed': '转换失败',

  // ── 连续粘贴（§11 P2）──
  'seq.start': '连续粘贴',
  'seq.startHint': '把选中的 {n} 条排成队列，之后每次取一条',
  'seq.remaining': '连续粘贴 · 剩余 {n} / {total}',
  'seq.next': '粘贴下一项',
  'seq.clear': '结束队列',
  'seq.keyHint': '{mod}⏎',

  // 启动期警告的标题。与 err.init 并列但**语义不同**：
  // err.init 是"坏了"，这个是"能用，但有件事你得知道"。
  // 用同一个标题会让用户以为程序挂了，从而去卸载重装。
  'warn.boot': '注意',
  'err.init': '后端初始化失败，本次运行不会记录任何内容',
  'err.generic': '操作失败：{err}',

  'about.title': '关于',
  'about.version': '版本',

  'common.close': '关闭',
  'common.cancel': '取消',
  'common.confirm': '确定',
  'common.none': '（无）',
  'common.warnings': '警告',
  'common.errors': '错误',
}

const en: Dict = {
  'app.name': 'PawClip',
  'app.tagline': 'Clipboard history',

  'nav.list': 'History',
  'nav.settings': 'Settings',
  'nav.stats': 'Stats',
  'nav.backup': 'Export / Import',
  'nav.categories': 'Categories',
  'nav.tags': 'Tags',

  'search.placeholder': 'Search history…  (⌘⇧V to summon)',
  'search.clear': 'Clear',
  'search.filterEmpty': 'Nothing matches',
  'search.hint.title': 'Why no results?',
  'search.hint.fts': 'Full-text search is available (FTS5 trigram)',
  'search.hint.like': 'Full-text search unavailable; degraded to row scan (slower, notably for 1–2 char queries)',
  'search.hint.mode': 'This query used',

  'list.empty': 'Nothing recorded yet. Copy something.',
  'list.emptyTrashed': 'Trash is empty.',
  'list.loading': 'Loading…',
  'list.loadMore': 'Load more',
  'list.end': 'End',
  'list.count': '{n} items',
  'list.all': 'All',
  'list.uncategorized': 'Uncategorized',
  'list.countApprox': '{n}+ items',
  'list.trash': 'Trash',

  'kind.text': 'Text',
  'kind.html': 'Rich text',
  'kind.image': 'Image',
  'kind.files': 'Files',
  'kind.rtf': 'Rich text',
  'kind.mixed': 'Mixed',

  'item.pinned': 'Pinned',
  'item.usedTimes': 'used {n}×',
  'item.files': '{n} files',
  'item.chars': '{n} chars',
  'item.expiresAt': 'expires {t}',
  'item.noExpiry': 'never auto-expires',
  'time.justNow': 'just now',
  'time.minutesAgo': '{n} min ago',
  'time.hoursAgo': '{n} h ago',
  'time.daysAgo': '{n} d ago',

  'action.paste': 'Paste',
  'action.copy': 'Copy only',
  'action.preview': 'Preview',
  'action.pin': 'Pin',
  'action.unpin': 'Unpin',
  'action.delete': 'Delete',
  'action.restore': 'Restore',
  'action.purge': 'Delete permanently',
  'action.more': 'More',

  'trash.empty': 'Empty trash',
  'trash.emptyConfirm': 'Empty the trash? Those items will be permanently deleted.',
  'trash.restored': 'Restored {n}',
  'trash.conflict': '{n} could not be restored (fingerprint taken)',



  'settings.lang': 'Language',
  'settings.lang.system': 'Follow system',
  'settings.theme': 'Appearance',
  'settings.theme.system': 'Follow system',
  'settings.theme.light': 'Light',
  'settings.theme.dark': 'Dark',
  'settings.hotkey': 'Global hotkey',
  'settings.hotkey.warn':
    'Heads-up: a global hotkey swallows that combination everywhere. ⌘⇧V means "paste and match style" in many editors.',
  'settings.pasteMode': 'When clicking an item',
  'settings.pasteMode.clipboard': 'Copy to clipboard only',
  'settings.pasteMode.autoPaste': 'Paste into the frontmost app',
  'settings.restoreClipboard': 'Restore previous clipboard after pasting',
  'settings.restoreDelayMs': 'Restore delay (ms)',
  'settings.quickPaste': '⌘/Ctrl + 1..N quick-paste count',
  'settings.closeOnBlur': 'Hide when clicking outside the panel',
  'settings.closeOnBlur.note':
    'Collapses when you click another app, the desktop, or another window. Clicks inside the panel do not count.',
  'settings.idleDestroy': 'Collapse the panel after N idle seconds',
  'settings.idleDestroy.note':
    'The design asks for *destroy* to free WebView memory. The current Wails version exposes ' +
    'no window-destroy API, so this collapses instead. The Stats page shows measured memory.',

  'settings.captureEnabled': 'Record clipboard',
  'settings.captureTypes': 'Recorded kinds',
  'settings.imageMaxBytes': 'Skip images larger than this',
  'settings.textMaxChars': 'Truncate long text to this many characters',
  'settings.debounceMs': 'Consecutive-change merge window (ms)',
  'settings.pollActiveMs': 'Poll interval while active (ms, macOS)',
  'settings.pollIdleMs': 'Poll interval while idle (ms, macOS)',
  'settings.idleThresholdSec': 'Idle after (seconds)',
  'settings.excludeApps': 'Never record from these apps',
  'settings.excludeApps.note':
    'One per line, * and ? wildcards supported. Bundle id (com.1password.*) or app name both work.',
  'settings.excludePrivateTypes': 'Skip content marked "do not record" (password managers etc.)',

  'settings.retentionTitle': 'Retention',
  'settings.defaultTtlSec': 'Default lifetime (seconds, 0 = unlimited)',
  'settings.onExpire': 'On expiry',
  'settings.onExpire.trash': 'Move to trash',
  'settings.onExpire.delete': 'Delete outright',
  'settings.onExpire.archive': 'Keep content, stop auto-expiring',
  'settings.trashTtlSec': 'Keep trash for (seconds)',
  'settings.maxItems': 'Max items (0 = unlimited)',
  'settings.maxDiskBytes': 'Max disk bytes (0 = unlimited)',
  'settings.gcIntervalSec': 'Collection interval (seconds)',
  'settings.autostart': 'Launch at login',
  'settings.autostart.note': 'The toggle shows the *actual system state*, not the intent stored in settings.',

  'settings.storage': 'Storage',
  'settings.cleanShutdownMarker': 'Use a marker file to decide whether to run integrity checks',
  'settings.dataDir': 'Data directory',
  'settings.configPath': 'Config file',
  'settings.openDataDir': 'Open in Finder / Explorer',
  'settings.saved': 'Saved',
  'settings.saveFailed': 'Save failed: {err}',
  'settings.uncovered': 'These settings have no control yet and are read-only:',

  'stats.title': 'Statistics',
  'stats.alive': 'Live items',
  'stats.trashed': 'In trash',
  'stats.all': 'Total',
  'stats.aliveBytes': 'Declared size',
  'stats.diskBytes': 'Image bytes on disk',
  'stats.diskFiles': 'Image files',
  'stats.events': 'Capture events',
  'stats.kinds': 'By kind',
  'stats.topApps': 'Top source apps',
  'stats.gc': 'Collection',
  'stats.gcRuns': 'Runs',
  'stats.gcRunNow': 'Collect now',
  'stats.gcRunning': 'Collecting…',
  'stats.lastGC': 'Last run',
  'stats.paused': 'Paused (export in progress)',
  'stats.memory': 'Memory',
  'stats.rss': 'Resident memory',
  'stats.webContent': 'WebKit content processes',
  'stats.destroyYes': 'Supported',
  'stats.destroyNo': 'Not supported (see note)',
  'stats.unknown': 'unknown',

  'backup.export': 'Export',
  'backup.import': 'Import',
  'backup.scope': 'Scope',
  'backup.scope.full': 'Everything',
  'backup.scope.favorites': 'Pinned only',
  'backup.includeExpired': 'Include expired items',
  'backup.embedFiles': 'Embed files for file-type items',
  'backup.start': 'Start export',
  'backup.exporting': 'Exporting…',
  'backup.exportDone': 'Export finished',
  'backup.pickFile': 'Choose a .clipbak file…',
  'backup.prechecking': 'Pre-checking…',
  'backup.precheckTitle': 'Confirm import',
  'backup.total': 'Items in archive',
  'backup.willImport': 'Will import',
  'backup.skipDup': 'Duplicate, skipped',
  'backup.skipExpired': 'Expired, skipped',
  'backup.inserted': 'Newly inserted',
  'backup.merged': 'Merged',
  'backup.overwritten': 'Overwritten',
  'backup.failed': 'Failed',
  'backup.invalid': 'Unrecognised',
  'backup.conflictPolicy': 'When an item already exists here',
  'backup.conflict.merge': 'Merge (add use counts, keep local category)',
  'backup.conflict.skip': 'Skip',
  'backup.conflict.overwrite': 'Overwrite (delete the local one first)',
  'backup.expiryPolicy': 'Expiry handling',
  'backup.expiry.keep': 'Keep the archive’s',
  'backup.expiry.rebase': 'Rebase onto import time',
  'backup.expiry.reset': 'Reset to never expire',
  'backup.alreadyImported': 'You have imported this archive before.',
  'backup.confirm': 'Import',
  'backup.importing': 'Importing…',
  'backup.importDone': 'Import finished',
  'backup.rollback': 'Undo last import',
  'backup.rollbackConfirm': 'Undo the last import? Items from that batch will be deleted.',
  'backup.lastImport': 'Last import',
  'backup.noLastImport': 'No import batch available to undo',
  'backup.path': 'Path',
  'backup.tookMs': 'Took',

  // ── Content converters (§11 P2) ──
  'conv.title': 'Convert',
  'conv.plainText': 'Paste as plain text',
  'conv.apply': 'Convert',
  'conv.result': 'Result',
  'conv.copyResult': 'Copy result',
  'conv.pasteResult': 'Paste result',
  'conv.pastePlain': 'Paste without formatting',
  'conv.empty': 'This item has no text to convert',
  'conv.json.beautify': 'Beautify JSON',
  'conv.json.minify': 'Minify JSON',
  'conv.lines.removeBlank': 'Remove blank lines',
  'conv.url.stripUTM': 'Strip tracking parameters',
  'conv.base64.encode': 'Base64 encode',
  'conv.base64.decode': 'Base64 decode',
  'conv.case.upper': 'UPPER CASE',
  'conv.case.lower': 'lower case',
  'conv.case.title': 'Title Case',
  'conv.err.invalidJSON': 'That is not valid JSON',
  'conv.err.multipleJSON': 'There are several JSON values here; cannot tell which to use',
  'conv.err.invalidBase64': 'That is not valid Base64',
  'conv.err.binaryResult': 'The result is not text (it may be binary data)',
  'conv.err.multipleLines': 'This conversion only works on a single line',
  'conv.err.tooLarge': 'The content is too large; the conversion was refused',
  'conv.err.empty': 'There is no text here to convert',
  'conv.err.unknownOp': 'Unknown conversion (possibly from a newer build)',
  'conv.err.failed': 'Conversion failed',

  // ── Sequential paste (§11 P2) ──
  'seq.start': 'Sequential paste',
  'seq.startHint': 'Queue the {n} selected items and take them one at a time',
  'seq.remaining': 'Sequential paste · {n} of {total} left',
  'seq.next': 'Paste next',
  'seq.clear': 'End queue',
  'seq.keyHint': '{mod}⏎',

  'warn.boot': 'Heads up',
  'err.init': 'Backend failed to initialise; nothing will be recorded this run',
  'err.generic': 'Failed: {err}',

  'about.title': 'About',
  'about.version': 'Version',

  'common.close': 'Close',
  'common.cancel': 'Cancel',
  'common.confirm': 'OK',
  'common.none': '(none)',
  'common.warnings': 'Warnings',
  'common.errors': 'Errors',
}

const dicts: Record<Lang, Dict> = { 'zh-CN': zh, en }

export type Vars = Record<string, string | number>

/**
 * makeT 造一个翻译函数。
 *
 * 缺键时**回退到英文**而不是返回键名：用户看到一句英文，好过看到
 * `settings.hotkey`。这和后端 i18n.go 的 T() 是同一个决定。
 *
 * 占位符用 `{name}`，只做一次替换（不做嵌套展开）：字符串都在设计里
 * 定死了，不需要一个模板引擎。
 */
export function makeT(lang: Lang) {
  const d = dicts[lang] ?? en
  return function t(key: string, vars?: Vars): string {
    let s = d[key] ?? en[key] ?? key
    if (vars) {
      for (const k of Object.keys(vars)) {
        s = s.split(`{${k}}`).join(String(vars[k]))
      }
    }
    return s
  }
}

export type T = ReturnType<typeof makeT>

export function normalizeLang(v: unknown): Lang {
  return v === 'zh-CN' ? 'zh-CN' : 'en'
}
