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
  /** 当前正在筛选的类型组；null = 不限 */
  kinds: string[] | null
  onKinds: (k: string[] | null) => void
  pinnedOnly: boolean
  onPinnedOnly: (v: boolean) => void
  trashed: boolean
  onTrashed: (v: boolean) => void
  total: number
  totalValid: boolean
  loading: boolean
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

/** 三个类型组：把用户的直觉分组映射到 `items.kind` 的具体取值。 */
const KINDS: Array<{ key: string; labelKey: string; kinds: string[] }> = [
  { key: 'text', labelKey: 'kind.text', kinds: ['text', 'html', 'rtf'] },
  // 'mixed' 归到"图片"：它几乎总是"浏览器里复制图片"产生的
  // （image + text 同时在剪贴板上）。归到"文本"会让筛选图片时漏掉它们，
  // 而归到"文件"更没道理。
  { key: 'image', labelKey: 'kind.image', kinds: ['image', 'mixed'] },
  { key: 'files', labelKey: 'kind.files', kinds: ['files'] },
]

export function SearchBar(p: SearchBarProps) {
  const {
    t, text, onText, kinds, onKinds,
    pinnedOnly, onPinnedOnly, trashed, onTrashed,
    total, totalValid, loading, mode, ftsAvailable, showHint, onToggleHint, inputRef,
  } = p

  const toggleKind = (key: string) => {
    const cur = kinds ?? []
    const next = cur.includes(key) ? cur.filter((k) => k !== key) : [...cur, key]
    // 全不选 = 不限，而不是"什么都不显示"。后者会让用户以为库空了。
    onKinds(next.length === 0 ? null : next)
  }

  return (
    <div className="searchbar">
      <div className="searchbar-row">
        <span className="searchbar-icon"><IconSearch size={15} /></span>
        <input
          ref={inputRef}
          className="searchbar-input"
          type="search"
          value={text}
          placeholder={t('search.placeholder')}
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
        {KINDS.map((k) => (
          <button
            key={k.key}
            type="button"
            className={`chip ${(kinds ?? []).includes(k.key) ? 'chip-on' : ''}`}
            onClick={() => toggleKind(k.key)}
          >
            {t(k.labelKey)}
          </button>
        ))}

        <button
          type="button"
          className={`chip ${pinnedOnly ? 'chip-on' : ''}`}
          onClick={() => onPinnedOnly(!pinnedOnly)}
        >
          {t('action.pin')}
        </button>

        <button
          type="button"
          className={`chip ${trashed ? 'chip-on' : ''}`}
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
 */
export function buildOpts(
  base: ListOptions,
  text: string,
  groups: string[] | null,
  pinnedOnly: boolean,
  trashed: boolean,
): ListOptions {
  let kinds: string[] | null = null
  if (groups && groups.length > 0) {
    const set = new Set<string>()
    for (const g of groups) {
      const def = KINDS.find((k) => k.key === g)
      // 认不出的组原样带上：后端将来加了分组、前端还没跟上时，
      // 至少请求是它要的，而不是被这里静默丢掉。
      if (def) for (const k of def.kinds) set.add(k)
      else set.add(g)
    }
    kinds = [...set]
  }

  return {
    ...base,
    text,
    // 空集合传 null：后端把空切片与 null 都当作"不限"，
    // 但 null 更明确，也少一次序列化。
    kinds: kinds && kinds.length > 0 ? kinds : null,
    pinnedOnly,
    trashed,
  }
}
