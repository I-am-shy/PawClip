// 与 Go 侧的绑定层（bindings.go）一一对应的类型 + 一个薄的调用封装。
//
// 两条自律：
//
//  1. **不生成、不导入 frontend/wailsjs/**。那个目录是 `wails build` 的产物
//     （已在 .gitignore 里），而 `tsc --noEmit` 在干净 clone 上跑不过它不存在的
//     情况。这里手写声明，代价是新增绑定方法要手动补一行类型——换来的是
//     "前端能在没跑过 wails build 的机器上通过类型检查"。
//  2. **类型只描述契约，不做校验。** 校验是后端的事；前端重复一遍只会让
//     改一处要动两处。

// ── 事件与生命周期 ──────────────────────────────────────────────

export type PanelEvent = {
  type: string
  at: number
  note?: string
}

export type PanelLifecycleReport = {
  idleDestroySec: number
  idleSec: number
  visible: boolean
  idleHides: number
  toggles: number
  rssBytes: number
  childProcs: number
  webContentProcs: number
  destroySupported: boolean
  note: string
}

// ── 列表 / 检索 ─────────────────────────────────────────────────

export type Cursor = { createdAt: number; id: number }

export type ListOptions = {
  text: string
  kinds: string[] | null
  // 分类 / 标签筛选：前端界面已下线（docs/DESIGN.md §11 P1），但 Go 绑定
  // 的 ListOptions 仍带这三个字段——跨进程契约保持镜像，所以这里保留，
  // 值始终是默认值（null / false）。
  categoryId: number | null
  uncategorized: boolean
  tagId: number | null
  sourceAppId: string
  pinnedOnly: boolean
  trashed: boolean
  since: number | null
  until: number | null
  cursor: Cursor | null
  limit: number
  includeTotal: boolean
}

export function defaultListOptions(): ListOptions {
  return {
    text: '',
    kinds: null,
    categoryId: null,
    uncategorized: false,
    tagId: null,
    sourceAppId: '',
    pinnedOnly: false,
    trashed: false,
    since: null,
    until: null,
    cursor: null,
    limit: 60,
    includeTotal: true,
  }
}

export type ListRow = {
  id: number
  kind: string
  preview: string
  fingerprint: string
  byteSize: number
  sourceAppId: string
  sourceAppName: string
  sourceUrl: string
  categoryId: number | null
  pinned: boolean
  firstSeenAt: number
  expiresAt: number | null
  ttlSource: string
  createdAt: number
  lastUsedAt: number | null
  useCount: number
  deletedAt: number | null
  textLen: number
  fileCount: number
  tagIds: number[] | null
  thumbUrl: string
  imageUrl: string
  hasHtml: boolean
}

export type PageResult = {
  rows: ListRow[] | null
  nextCursor: Cursor | null
  hasMore: boolean
  /** "fts" | "like" | "fts+like" | "none" —— 检索走的哪条路，用于诊断 */
  mode: string
  tookMs: number
  total: number
  totalValid: boolean
  ftsAvailable: boolean
}

export type ItemDetail = ListRow & {
  text: string
  html: string
  rtf: string
  filePaths: string[] | null
  imageWidth: number
  imageHeight: number
}

// ── 回写 ────────────────────────────────────────────────────────

export type PasteResult = {
  /** "clipboard" | "autoPaste" */
  mode: string
  pasted: boolean
  /** 降级原因。非空时界面**必须**显示出来（§13 风险表：不假装贴上了） */
  note: string
  restored: boolean
}

export type SequenceState = {
  active: boolean
  remaining: number
  total: number
}

// ── 内容转换器（docs/DESIGN.md §11 P2）───────────────────────────────────

export type TransformResult = {
  op: string
  text: string
  /** 结果与原文是否不同。相同不代表失败——"大小写转换"作用在纯中文上就是没变 */
  changed: boolean
  /**
   * 失败的原因种类（空串表示成功）。
   *
   * 这是一个**前后端契约**：后端给种类，前端翻成用户的语言
   * （见 i18n.ts 的 conv.err.*）。用种类而不是直接用后端那句话，
   * 是为了英文界面不弹中文、中文界面不弹英文。
   */
  errorKind?: string
  /** 兜底原文：遇到前端不认识的种类时才显示它 */
  errorText?: string
}

// ── 分类 / 标签 ─────────────────────────────────────────────────

export type Category = {
  id: number
  name: string
  color: string
  icon: string
  sortOrder: number
  itemCount: number
  ttlSeconds: number | null
}

export type Tag = {
  id: number
  name: string
  color: string
  itemCount: number
}

export type RestoreResult = { restored: number; conflict: number }

// ── 草稿本（docs/DESIGN.md §4.4）────────────────────────────────

/**
 * DraftRow 是目录里的一条：**没有正文全文**。
 *
 * 与 ListRow 同一条纪律（§14 第 7 条）：目录只需要标题 / 摘要 / 字数 / 时间。
 * 正文要单独调 Draft(id) 取——一次把两百条草稿的正文全拉进内存，
 * 是几十 MB 的无谓开销。
 */
export type DraftRow = {
  id: number
  title: string
  /** 默认名里的编号。0 表示没有编号（导入进来的）。 */
  seq: number
  snippet: string
  chars: number
  sortOrder: number
  createdAt: number
  updatedAt: number
  /** 非 null 表示在归档区（软删除）。 */
  archivedAt: number | null
}

export type Draft = {
  id: number
  title: string
  /** Markdown 源码。它是**唯一真源**，富文本只是编辑态的呈现。 */
  md: string
  seq: number
  sortOrder: number
  createdAt: number
  updatedAt: number
  archivedAt: number | null
}

export type DraftList = {
  items: DraftRow[]
  archived: DraftRow[]
  /** 归档保留期（秒）：归档的草稿超过它会被 GC 彻底删除（draft.archiveTtlSec）。 */
  archiveTtlSec: number
  /** 上次打开的草稿（ui.lastDraftId）：重进草稿本时回到它；可能已被删，须校验存在。 */
  lastDraftId: number
  /** 目录的收起状态（ui.draftTocCollapsed）。 */
  tocCollapsed: boolean
  maxImageBytes: number
  /** 上限的人读形式（"10 MB"）。后端给，避免两处各算一遍。 */
  maxImageLabel: string
  autoSaveDebounceMs: number
}

export type DraftSaveResult = {
  /** 落库时间（Unix 秒），界面据此显示"已保存 · HH:MM"。 */
  updatedAt: number
  chars: number
}

export type DraftImage = {
  /** 要插进正文的图片地址（相对 URL，形如 blob/9f/2a/…png）。 */
  url: string
  bytes: number
  /** 像素尺寸；非 PNG 时为 0，前端按"未知高度"处理。 */
  width: number
  height: number
}

// ── 统计 ────────────────────────────────────────────────────────

export type KindStat = { kind: string; count: number; bytes: number }
export type SourceAppStat = { appId: string; appName: string; count: number }

export type GCReport = {
  startedAt: string
  tookMs: number
  ttlApplied: number
  protected: number
  trashed: number
  deleted: number
  archived: number
  purged: number
  evicted: number
  evictedBytes: number
  orphansRemoved: number
  orphanBytes: number
  vacuumed: boolean
  alive: number
  totalBytes: number
  summary: string
  errors?: string[]
}

export type StatsSummary = {
  alive: number
  trashed: number
  all: number
  aliveBytes: number
  events: number
  diskBytes: number
  diskFiles: number
  kinds: KindStat[] | null
  topApps: SourceAppStat[] | null
  lastGC: GCReport | null
  gcRuns: number
  paused: boolean
}

// ── 导出 / 导入 ─────────────────────────────────────────────────

export type ExportOptions = {
  scope: string
  categoryId: number | null
  since: number | null
  until: number | null
  excludeKinds: string[] | null
  includeExpired: boolean
  manifestFormat: string
  embedFiles: boolean
  outputDir: string
}

export type ExportResult = {
  path: string
  bytes: number
  items: number
  categories: number
  tags: number
  // drafts 只在 scope=full 时非 0（草稿与置顶/分类/时间范围正交）。
  drafts: number
  blobBytes: number
  blobs: number
  missingBlobs: number
  warnings?: string[]
  tookMs: number
}

export type ImportOptions = {
  conflictPolicy: string
  expiryPolicy: string
  importExpired: boolean
  categoryPolicy: string
  importSettings: boolean
}

export type PrecheckResult = {
  path: string
  manifestName: string
  format: string
  formatVersion: number
  appVersion: string
  exportedAt: string
  platform: string
  scope: string
  total: number
  willImport: number
  skipDuplicate: number
  skipExpired: number
  invalid: number
  // 草稿没有指纹，所以"导入"对它是**新增**（没有 skipDuplicate 那一档）。
  totalDrafts: number
  willImportDrafts: number
  categoriesNew: string[] | null
  categoriesReuse: string[] | null
  tagsNew: number
  tagsReuse: number
  uncompressedBytes: number
  needBytes: number
  availableBytes: number
  blobCount: number
  warnings?: string[]
  alreadyImported: boolean
}

export type ImportResult = {
  importId: number
  imported: number
  skipped: number
  failed: number
  merged: number
  overwritten: number
  categoriesMade: number
  tagsMade: number
  // 草稿段的结果：只有"成了几条 / 失败几条"两档（没有 merge/overwrite）。
  draftsImported: number
  draftsFailed: number
  blobsWritten: number
  thumbsMade: number
  status: string
  errors?: string[]
  warnings?: string[]
  tookMs: number
  inserted: number
}

export type ImportRow = {
  id: number
  startedAt: number
  finishedAt: number | null
  path: string
  manifestHash: string
  imported: number
  skipped: number
  failed: number
  status: string
  rollbackPossible: boolean
}

// ── 设置 ────────────────────────────────────────────────────────

/**
 * SettingsShape 与 store/settings.go 的 Settings 一一对应。
 *
 * 刻意**不是** Record<string, unknown>：设置页要按分区渲染控件，
 * 有具体类型才能在编译期发现"后端改了字段名、前端还在读旧的"。
 * 每个字段的 JSON 名与 Go 的 tag 一致。
 */
export type SettingsShape = {
  capture: {
    enabled: boolean
    types: string[]
    imageMaxBytes: number
    textMaxChars: number
    pollIntervalActiveMs: number
    pollIntervalIdleMs: number
    idleThresholdSec: number
    debounceMs: number
  }
  exclude: {
    apps: string[]
    privateTypes: boolean
  }
  retention: {
    defaultTtlSec: number
    onExpire: string
    trashTtlSec: number
    maxItems: number
    maxDiskBytes: number
    gcIntervalSec: number
  }
  ui: {
    hotkey: string
    pasteMode: string
    restoreClipboard: boolean
    restoreDelayMs: number
    windowIdleDestroySec: number
    quickPasteCount: number
    language: string
    theme: string
    /**
     * 点到面板以外的任何地方（别的 App、桌面、别的窗口）时自动收起。
     *
     * 判据是**面板丢掉键盘焦点**，不是"应用失去激活"：面板是
     * NonactivatingPanel，呼出时它自己拿键盘但不会把应用切到前台。
     */
    closeOnBlur: boolean
    /**
     * 面板尺寸（逻辑点）。
     *
     * 不是给用户填的参数，而是**用户拖出来的结果**：面板边缘可拉伸，
     * 收起/退出时把当前 frame 写回这两项，下次启动照原样打开。
     * 设置页只读展示，不需要可编辑控件。
     */
    panelWidth: number
    panelHeight: number
    /**
     * 上次停在哪一页（"panel" | "drafts" | "stats" | "backup" | "settings"）。
     *
     * 只有**草稿本**会被粘滞地恢复（见 App.tsx 的 stickyView）：
     * 其余视图在下次呼出面板时都回到历史页。后端有 NormalizeLastView
     * 兜住未知值，所以这里给 string 就够，不必做成联合类型。
     */
    lastView: string
    /** 上次打开的草稿（0 = 未记录）。重进草稿本时回到它。 */
    lastDraftId: number
    /** 草稿本目录的收起状态。 */
    draftTocCollapsed: boolean
  }
  storage: {
    cleanShutdownMarker: boolean
    walCheckpointEvery: number
  }
  backup: {
    manifestFormat: string
    includeExpired: boolean
  }
  /**
   * 草稿本的参数（draft.*）。设置页有对应控件（归档保留期 / 防抖 / 贴图上限）。
   */
  draft: {
    /** 实时保存的防抖窗口（毫秒）。 */
    autoSaveDebounceMs: number
    /** 单张草稿贴图的上限（字节）。 */
    imageMaxBytes: number
    /** 归档草稿的保留期（秒），超过后 GC 硬删。 */
    archiveTtlSec: number
  }
}

// ── 诊断 ────────────────────────────────────────────────────────

export type Health = {
  version: string
  platform: string
  initError: string
  /**
   * 启动期警告（已由后端按当前语言渲染好）。
   *
   * 目前只有一种：config.toml 读不了，本次运行改用默认设置。
   * 它**不是**错误——程序照常工作；但它必须可见，否则用户改了配置
   * 却不生效时会一头雾水。后端负责措辞，前端只显示。
   */
  bootWarning: string
  dbPath: string
  schemaVersion: number
  ftsAvailable: boolean
  integrityNote: string
  aliveItems: number
  trashedItems: number
  allItems: number
  totalAliveBytes: number
  capture: {
    ticks: number
    reads: number
    accepted: number
    enqueueErr: number
    readErrs: number
    empty: number
    busy: number
    flapping: number
    drops: Record<string, number> | null
    lastError: string
  }
  writer: {
    enqueued: number
    rejected: number
    items: number
    batches: number
    errors: number
    checkpoints: number
    lastFlushMs: number
  }
}

// ── 调用封装 ────────────────────────────────────────────────────

/** Bindings 描述 window.go.main.App 上的全部方法。 */
export type Bindings = {
  List(opts: ListOptions): Promise<PageResult>
  Get(id: number): Promise<ItemDetail | null>
  Delete(ids: number[]): Promise<number>
  Restore(ids: number[]): Promise<RestoreResult>
  Purge(ids: number[]): Promise<number>
  EmptyTrash(): Promise<number>
  Pin(ids: number[], pinned: boolean): Promise<void>
  SetCategory(ids: number[], categoryId: number | null): Promise<void>
  SetExpiry(ids: number[], expiresAt: number | null): Promise<void>

  Categories(): Promise<Category[] | null>
  SaveCategory(c: Category): Promise<number>
  DeleteCategory(id: number): Promise<void>
  Tags(): Promise<Tag[] | null>
  SaveTag(id: number, name: string, color: string): Promise<number>
  DeleteTag(id: number): Promise<void>
  SetItemTags(itemId: number, tagIds: number[]): Promise<void>

  /** 目录 + 归档区 + 草稿本的几个设置值，一次给全（避免三次 IPC 往返）。 */
  Drafts(): Promise<DraftList>
  /** 取一条草稿（含正文）。已删除时抛错，而不是返回 null。 */
  Draft(id: number): Promise<Draft>
  /** 新建一条，默认名由后端按当前语言渲染并写进库。返回新草稿。 */
  CreateDraft(): Promise<Draft>
  /**
   * 写正文——实时保存的**唯一**入口。
   *
   * 刻意不接收 title：标题走 RenameDraft。两条写路径分开，才不会出现
   * "自动保存顺手把用户正在改的标题覆盖回去"。
   */
  SaveDraft(id: number, md: string): Promise<DraftSaveResult>
  RenameDraft(id: number, title: string): Promise<void>
  /** 软删除（进归档区，保留期内可恢复）。 */
  ArchiveDraft(id: number): Promise<void>
  RestoreDraft(id: number): Promise<void>
  /** 彻底删除，返回被删掉的字符数。 */
  PurgeDraft(id: number): Promise<number>
  /** 按给定顺序重写目录顺序（拖拽排序的落库入口）。 */
  ReorderDrafts(ids: number[]): Promise<void>
  /**
   * 把一张图片落成 blob，返回可插进正文的 URL。
   *
   * 入参是 base64（可带 `data:image/png;base64,` 前缀），不是文件路径：
   * 前端从剪贴板或文件框拿到的就是这个形式，没有别的东西可传。
   */
  ImportDraftImage(mime: string, dataBase64: string): Promise<DraftImage>

  Paste(id: number, autoPaste: boolean): Promise<PasteResult>
  CopyOnly(id: number): Promise<PasteResult>
  PasteText(text: string): Promise<void>
  PastePlain(id: number, autoPaste: boolean): Promise<PasteResult>
  TransformOpList(): Promise<string[] | null>
  TransformItem(id: number, op: string): Promise<TransformResult>
  TransformText(text: string, op: string): Promise<TransformResult>
  PasteTransformed(id: number, op: string, autoPaste: boolean): Promise<PasteResult>
  StartSequence(ids: number[]): Promise<number>
  NextInSequence(): Promise<PasteResult>
  SequenceState(): Promise<SequenceState>
  ClearSequence(): Promise<void>

  ShowPanel(): Promise<void>
  HidePanel(): Promise<void>
  /**
   * 记住"当前停在哪一页"（ui.lastView）。
   *
   * 只有草稿本是**粘滞**的：其余视图在下次呼出面板时都回到历史页
   * （见 App.tsx 里那条 show 事件的处理）。所以这个调用只在进入/离开
   * 草稿本时发生，不是每次切视图都发。
   */
  SetLastView(view: string): Promise<void>
  /**
   * 记住"上次打开的草稿"（ui.lastDraftId）。
   *
   * 重进草稿本回到它而不是固定回第一条——用户有多条草稿时，
   * 固定回第一条看起来就像"刚写的东西没了"。与当前值相同时后端不写库。
   */
  SetLastDraft(id: number): Promise<void>
  /**
   * 热键输入态：让出 / 收回全局热键。
   *
   * 系统级热键在事件分发之前就把按键吃掉了，WebView 收不到那一次 keydown——
   * 录制热键时必须先让出，否则"按下当前组合"在界面上毫无反应。
   * 两者都是幂等的，且只动注册、不动设置。
   */
  SuspendHotkey(): Promise<void>
  ResumeHotkey(): Promise<void>
  /** 在标题栏空白处按下鼠标时调用：交给原生拖动循环移动窗口。 */
  DragPanel(): Promise<void>
  PanelVisible(): Promise<boolean>
  PanelDiag(): Promise<string>
  AutoPasteAvailable(): Promise<boolean>
  RequestAutoPaste(): Promise<void>
  PanelLifecycle(): Promise<PanelLifecycleReport>
  TakeEvents(): Promise<PanelEvent[] | null>

  StatsOverview(): Promise<StatsSummary>
  RunGC(): Promise<GCReport | null>

  Export(opts: ExportOptions): Promise<ExportResult>
  PrecheckBackup(pkgPath: string, opts: ImportOptions): Promise<PrecheckResult>
  ImportBackup(pkgPath: string, opts: ImportOptions, pc: PrecheckResult): Promise<ImportResult>
  LastImport(): Promise<ImportRow | null>
  RollbackImport(importId: number): Promise<number>

  Health(): Promise<Health>
  Settings(): Promise<SettingsShape | null>
  SetSetting(key: string, value: string): Promise<void>
  SettingKeys(): Promise<string[] | null>
  Flush(): Promise<void>
  Version(): Promise<string>
  ConfigPath(): Promise<string>
  DataDir(): Promise<string>
  Lang(): Promise<string>
  IsAutoStart(): Promise<boolean>
  SetAutoStart(enabled: boolean): Promise<void>
  AutoStartDiag(): Promise<string>
  AutoStartSupported(): boolean

  /** 弹原生目录选择框。返回空串 = 用户取消。 */
  PickExportDir(): Promise<string>
  /** 弹原生文件选择框（.clipbak）。返回空串 = 用户取消。 */
  PickBackupFile(): Promise<string>
  /** 在系统文件管理器里打开数据目录。 */
  RevealDataDir(): Promise<void>
  /** 在文件管理器里定位数据目录内的某个路径。 */
  RevealPath(path: string): Promise<void>
  /**
   * 用系统默认程序打开一条外部链接（草稿正文里点一条链接）。
   *
   * 必须由后端打开：这个 WebView 就是应用界面本身，跟着 `<a>` 导航一趟
   * 等于把面板换成网页。后端还会再筛一次协议（只放 http / https / mailto）。
   */
  OpenURL(url: string): Promise<void>
}

declare global {
  interface Window {
    go?: { main?: { App?: Partial<Bindings> } }
  }
}

/** bridge 返回绑定表；还没注入时返回 undefined。 */
export function bridge(): Partial<Bindings> | undefined {
  return window.go?.main?.App
}

/**
 * call 调一个绑定方法。
 *
 * ⚠️ 这里**不**吞掉"绑定还没就绪"：那是一种真实的失败状态（页面被单独
 * 用浏览器打开、或 Wails 注入失败），静默返回空数据会让界面显示"没有历史"，
 * 而用户会以为记录丢了。调用方应当把它显示成错误。
 */
export async function call<K extends keyof Bindings>(
  name: K,
  ...args: Parameters<Bindings[K]>
): Promise<Awaited<ReturnType<Bindings[K]>>> {
  const fn = bridge()?.[name] as ((...a: unknown[]) => Promise<unknown>) | undefined
  if (!fn) {
    throw new Error(`绑定 ${String(name)} 尚未就绪（页面可能不是由 PawClip 打开的）`)
  }
  return (await fn(...args)) as Awaited<ReturnType<Bindings[K]>>
}

/** bindingsReady 报告绑定是否已经注入。 */
export function bindingsReady(): boolean {
  return bridge() != null
}

// ── blob URL ────────────────────────────────────────────────────

/**
 * blobSrc 把后端给的相对 URL 变成可以直接塞进 <img src> 的形式。
 *
 * 为什么**不加**前导 `/`：后端返回的是 `blob/9f/2a/x.thumb.png`，交给
 * 相对解析会得到 `wails://wails/blob/...`，那正是 AssetServer 的用户
 * handler 能接到的形式（它只在"内嵌资源里找不到"时才转发给我们）。
 * 加一个前导斜杠看似更"标准"，实际会让它变成绝对路径、绕过那层转发。
 *
 * 空串返回空串：前端据此显示占位图，而不是去请求一个必然被拒的 URL。
 */
export function blobSrc(rel: string): string {
  return rel || ''
}
