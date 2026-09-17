// 搜索栏：输入框 + 类型筛选 + 清空。
//
// 关键交互都在 App 里做（防抖、取消），这里只管"把用户意图转成 props 回调"。
// 这样这个组件是纯受控的，能在没有后端的情况下单独渲染。

import type { T as TFn } from '../i18n'
import type { ListOptions } from '../api'
import { IconSearch, IconX } from './Icons'

export type SearchBarProps = {
  t: TFn
  text: string
  onText: (v: string) => void
  /** 当前选中的类型组；null = 全部（不限类型）。**单选**，见 KIND_FILTERS */
  kind: string | null
  onKind: (k: string | null) => void
  pinnedOnly: boolean
  onPinnedOnly: (v: boolean) => void
  trashed: boolean
  onTrashed: (v: boolean) => void
  total: number
  totalValid: boolean
  loading: boolean
  /**
   * 呼出面板的热键，**已经按平台渲染好**（macOS 是 ⌘⇧V，其余是 Ctrl+Shift+V）。
   *
   * 由 App 从 ui.hotkey 现算出来传进来，空串表示用户把热键清掉了。
   * 这里刻意只接受"渲染好的字符串"而不是原始组合：占位符是给人读的，
   * 不该在组件里再拼一次 ⌘/Ctrl 的映射（那正是提示与设置对不上的老毛病）。
   */
  hotkeyLabel: string
  /** 检索走的哪条路（fts / like / …），用于诊断提示 */
  mode: string
  ftsAvailable: boolean
  showHint: boolean
  onToggleHint: () => void
  /**
   * 输入框的 ref，供 App 做「聚焦并全选」。
   *
   * 类型是 `RefObject<HTMLInputElement>`（React 18 语义下 `current` 本身
   * 可空），**不能**写成 `RefObject<HTMLInputElement | null>`：那样
   * `current` 的类型里就叠了一层 null，不再满足 `ref` 属性要求的
   * `LegacyRef<HTMLInputElement>`，TS 会在 `ref={inputRef}` 处报
   * 「Type 'RefObject<HTMLInputElement | null>' is not assignable」。
   * 对应的创建侧写法是 `useRef<HTMLInputElement>(null)`。
   */
  inputRef: React.RefObject<HTMLInputElement>
}

/**
 * 三个类型组：把用户的直觉分组映射到 `items.kind` 的具体取值。
 *
 * 数组本身是"单选"的候选项——界面上同时只有一个能选中（`kind === key`）。
 * 分组是多对一的，一个组对应后端好几个 kind 值（见下面的对应关系），
 * 所以真正发给后端的是展开后的 `kinds`，不是这里的 `key`。
 */
const KIND_FILTERS: Array<{ key: string; labelKey: string; kinds: string[] }> = [
  { key: 'text', labelKey: 'kind.text', kinds: ['text', 'html', 'rtf'] },
  // 'mixed' 归到"图片"：它几乎总是"浏览器里复制图片"产生的
  // （image + text 同时在剪贴板上）。归到"文本"会让筛选图片时漏掉它们，
  // 而归到"文件"更没道理。
  { key: 'image', labelKey: 'kind.image', kinds: ['image', 'mixed'] },
  { key: 'files', labelKey: 'kind.files', kinds: ['files'] },
]

export function SearchBar(p: SearchBarProps) {
  const {
    t, text, onText, kind, onKind,
    pinnedOnly, onPinnedOnly, trashed, onTrashed,
    total, totalValid, loading, mode, ftsAvailable, showHint, onToggleHint, inputRef,
    hotkeyLabel,
  } = p

  // 提示与设置绑定：有热键才提热键，清掉了就只说"搜索历史"。
  // 之前这里是写死的 "搜索历史…（⌘⇧V 呼出）"：用户把热键换成 ⌥Space、
  // 或者干脆清空，提示都还在说 ⌘⇧V —— 一句永远不对的话。
  const placeholder = hotkeyLabel
    ? t('search.placeholder.summon', { key: hotkeyLabel })
    : t('search.placeholder')

  // 单选：点中的那一个成为唯一选中项。再点一次已选中的等于取消，回到「全部」
  // ——与点「全部」同义，但符合"点一下切换"的直觉，也顺手修掉"选了之后
  // 怎么回全部"这个疑问。
  const selectKind = (key: string) => onKind(kind === key ? null : key)

  return (
    <div className="searchbar">
      <div className="searchbar-row">
        <span className="searchbar-icon"><IconSearch size={15} /></span>
        <input
          ref={inputRef}
          className="searchbar-input"
          type="search"
          value={text}
          placeholder={placeholder}
          spellCheck={false}
          autoComplete="off"
          onChange={(e) => onText(e.target.value)}
          onKeyDown={(e) => {
            // Esc 的语义分两级，由 App 决定（先清搜索、再收起面板），
            // 这里只是别让浏览器的"清空 <input type=search>"抢先。
            if (e.key === 'Escape') e.preventDefault()
          }}
        />
        {text !== '' && (
          <button
            type="button"
            className="iconbtn"
            title={t('search.clear')}
            aria-label={t('search.clear')}
            onClick={() => onText('')}
          >
            <IconX size={14} />
          </button>
        )}
        <button
          type="button"
          className="hintbtn"
          onClick={onToggleHint}
          title={t('search.hint.title')}
          aria-expanded={showHint}
        >
          ?
        </button>
      </div>

      <div className="searchbar-filters">
        {/*
          类型是**单选**：「全部」是一个真正的选项，而不是"一个都没选"。
          之前是多选，于是"文本 + 图片"这种状态既不像全部也不像某一类，
          列表里混着两类内容，跟用户点这个 chip 时的预期不符。
        */}
        <button
          type="button"
          className={`chip ${kind === null ? 'chip-on' : ''}`}
          aria-pressed={kind === null}
          onClick={() => onKind(null)}
        >
          {t('list.all')}
        </button>

        {KIND_FILTERS.map((k) => (
          <button
            key={k.key}
            type="button"
            className={`chip ${kind === k.key ? 'chip-on' : ''}`}
            aria-pressed={kind === k.key}
            onClick={() => selectKind(k.key)}
          >
            {t(k.labelKey)}
          </button>
        ))}

        {/* 分隔线：左边是"三选一"，右边两个是各自独立的开关，
            不加这条线它们看起来像同一组，会让人以为只能选一个。 */}
        <span className="chip-sep" aria-hidden="true" />

        <button
          type="button"
          className={`chip ${pinnedOnly ? 'chip-on' : ''}`}
          aria-pressed={pinnedOnly}
          onClick={() => onPinnedOnly(!pinnedOnly)}
        >
          {t('action.pin')}
        </button>

        <button
          type="button"
          className={`chip ${trashed ? 'chip-on' : ''}`}
          aria-pressed={trashed}
          onClick={() => onTrashed(!trashed)}
        >
          {t('list.trash')}
        </button>

        <span className="searchbar-count">
          {loading
            ? t('list.loading')
            : totalValid
              ? t('list.count', { n: total })
              : t('list.countApprox', { n: total })}
        </span>
      </div>

      {showHint && (
        <div className="hintpanel">
          <div className="hintpanel-title">{t('search.hint.title')}</div>
          <div className="hintpanel-line">
            {ftsAvailable ? t('search.hint.fts') : t('search.hint.like')}
          </div>
          <div className="hintpanel-line dim">
            {t('search.hint.mode')}: <code>{modeLabel(mode)}</code>
          </div>
        </div>
      )}
    </div>
  )
}

/** modeLabel 把搜索模式转成一个短记号（诊断用，不必翻译）。 */
export function modeLabel(mode: string): string {
  switch (mode) {
    case 'fts':
      return 'FTS5 trigram'
    case 'like':
      return 'LIKE'
    case 'fts+like':
      return 'FTS5 → LIKE'
    case 'none':
      return '—'
    default:
      return mode || '—'
  }
}

/**
 * buildOpts 把界面的筛选状态转成后端入参。
 *
 * ⚠️ 这里必须把"组"展开成具体的 `items.kind` 值再发给后端。
 * 直接把 'text' 发下去会**只命中 kind='text'**，把 kind='html'（网页复制的
 * 纯文本）与 'rtf' 全漏掉——而那两类恰恰是"文本"这个筛选词最该覆盖的。
 * 这个错法不会报错，只是结果少得莫名其妙，所以在本文件里就展开掉。
 *
 * `group` 是**单个**组键（界面是单选），null = 不限类型。认不出的组原样带上：
 * 后端将来加了分组、前端还没跟上时，至少请求是它要的，而不是被这里静默丢掉。
 */
export function buildOpts(
  base: ListOptions,
  text: string,
  group: string | null,
  pinnedOnly: boolean,
  trashed: boolean,
): ListOptions {
  let kinds: string[] | null = null
  if (group) {
    const def = KIND_FILTERS.find((k) => k.key === group)
    kinds = def ? [...def.kinds] : [group]
  }

  return {
    ...base,
    text,
    // null = 不限。后端把空切片与 null 都当作"不限"，但 null 更明确。
    kinds,
    pinnedOnly,
    trashed,
  }
}
