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
  }
  storage: {
    cleanShutdownMarker: boolean
    walCheckpointEvery: number
  }
  backup: {
    manifestFormat: string
    includeExpired: boolean
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
