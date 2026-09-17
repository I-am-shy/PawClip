// 面板主视图：搜索栏 + 分类树 + 列表 + 预览。
//
// 状态为什么都在这里而不是提到 App：面板的键盘操作（⌘1..9、上下键、回车）
// 必须作用在**它自己那份列表状态**上。提到 App 就得把 rows / cursor /
// activeId 全传下来，而切到设置页之后这些状态还得手动清理。
// 挂在组件上，切视图即卸载，天然干净。
//
// 面板事件（后端推来的"呼出/收起"）由 App 收到后通过 props 转发进来。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { T as TFn } from '../i18n'
import {
  call,
  defaultListOptions,
  type Cursor,
  type ListRow,
  type PasteResult,
  type SequenceState,
  type SettingsShape,
} from '../api'
import { useDebounce, useInterval } from '../hooks'
import { ItemList } from '../components/ItemList'
import { Preview } from '../components/Preview'
import { SearchBar, buildOpts } from '../components/SearchBar'
import { IconChevron, IconRestore, IconTrash } from '../components/Icons'

export type PanelProps = {
  t: TFn
  settings: SettingsShape | null
  /** 平台修饰键记号（⌘ / Ctrl+），由 App 按后端上报的平台决定。 */
  modLabel: string
  /**
   * 呼出热键的显示形式（⌘⇧V / Ctrl+Shift+V），空串 = 没设热键。
   * 由 App 从 ui.hotkey 现算，供搜索框的占位提示使用。
   */
  hotkeyLabel: string
  trashed: boolean
  onTrashed: (v: boolean) => void
  /** 面板要求隐藏（Esc 的兜底行为）。 */
  onHide: () => void
  onOpenView: (v: 'settings' | 'stats' | 'backup') => void
  onToast: (msg: string) => void
  /** 后端推来的"强制聚焦搜索框"信号，用递增的 nonce 表达。 */
  focusSearchNonce: number
}

const PAGE_LIMIT = 60
const DEBOUNCE_MS = 120

export function Panel(p: PanelProps) {
  const { t, settings, trashed } = p

  // ── 检索与列表状态 ─────────────────────────────────────────────
  const [text, setText] = useState('')
  // 类型筛选是**单选**：null = 全部，否则是 SearchBar 里某个组的 key。
  // 用单值而不是数组，是为了让"同时选中文本和图片"这种状态在类型上
  // 就不可能出现——它是界面上的非法状态，不该靠约定去避免。
  const [kind, setKind] = useState<string | null>(null)
  const [pinnedOnly, setPinnedOnly] = useState(false)

  const [rows, setRows] = useState<ListRow[]>([])
  const [cursor, setCursor] = useState<Cursor | null>(null)
  const [hasMore, setHasMore] = useState(false)
  const [total, setTotal] = useState(0)
  const [totalValid, setTotalValid] = useState(false)
  const [mode, setMode] = useState('none')
  const [ftsAvailable, setFtsAvailable] = useState(true)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [seq, setSeq] = useState<SequenceState | null>(null)
  const [activeId, setActiveId] = useState<number | null>(null)
  const [selectedIds, setSelectedIds] = useState<Set<number>>(new Set())
  const [previewId, setPreviewId] = useState<number | null>(null)
  const [showHint, setShowHint] = useState(false)

  const inputRef = useRef<HTMLInputElement>(null)
  // 请求序号：防抖之后仍可能有多个请求在飞（键入快 + 后端慢），
  // 只用"最后一个"的结果，否则会出现"列表显示的是上一次查询的结果"。
  const reqSeq = useRef(0)

  const debouncedText = useDebounce(text, DEBOUNCE_MS)[0]
  const quickPasteCount = settings?.ui?.quickPasteCount ?? 9
  const modLabel = p.modLabel

  /**
   * nonce 是"用同一组条件再查一次"的开关。
   *
   * 删/恢复/清空回收站之后列表内容变了，但筛选条件没变——不加这一位
   * 就没有任何东西能让查询重跑，界面会停在旧结果上（用户以为操作没生效）。
   * 用递增整数而不是布尔，是为了避免"置 true 之后必须记得复位"。
   */
  const [nonce, setNonce] = useState(0)
  const refresh = useCallback(() => setNonce((n) => n + 1), [])

  // 每次列表在"筛选条件"变化时重置到第一页。
  // nonce 也在里面：它表达"条件没变，但请再查一次"。
  const filterKey = useMemo(
    () => JSON.stringify({ debouncedText, kind, pinnedOnly, trashed, nonce }),
    [debouncedText, kind, pinnedOnly, trashed, nonce],
  )

  useEffect(() => {
    const seq = ++reqSeq.current
    setLoading(true)
    const base = defaultListOptions()
    base.limit = PAGE_LIMIT
    base.includeTotal = true
    const opts = buildOpts(base, debouncedText, kind, pinnedOnly, trashed)

    call('List', opts)
      .then((pg) => {
        if (seq !== reqSeq.current) return
        setRows(pg.rows ?? [])
        setCursor(pg.nextCursor ?? null)
        setHasMore(!!pg.hasMore)
        setTotal(pg.total ?? 0)
        setTotalValid(!!pg.totalValid)
        setMode(pg.mode || 'none')
        setFtsAvailable(!!pg.ftsAvailable)
        setError('')
        // 光标移到第一条但**不自动选中**：自动选中会让"按回车"变得危险
        // （用户可能只是想看一眼列表）。
        setActiveId((prev) => {
          const list = pg.rows ?? []
          if (list.length === 0) return null
          return list.some((r) => r.id === prev) ? prev : list[0].id
        })
        setSelectedIds(new Set())
      })
      .catch((e: unknown) => {
        if (seq !== reqSeq.current) return
        setRows([])
        setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (seq === reqSeq.current) setLoading(false)
      })
    // filterKey 已经把上面四项打包成一个字符串，这里刻意只依赖它。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filterKey])

  // 载入更多：走 keyset 游标，不重排已有行。
  const loadMore = useCallback(() => {
    if (!cursor || loading) return
    setLoading(true)
    const base = defaultListOptions()
    base.limit = PAGE_LIMIT
    base.includeTotal = false
    base.cursor = cursor
    const opts = buildOpts(base, debouncedText, kind, pinnedOnly, trashed)
    call('List', opts)
      .then((pg) => {
        setRows((prev) => [...prev, ...(pg.rows ?? [])])
        setCursor(pg.nextCursor ?? null)
        setHasMore(!!pg.hasMore)
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false))
  }, [cursor, loading, debouncedText, kind, pinnedOnly, trashed])

  // ── 连续粘贴（§11 P2）─────────────────────────────────────────
  //
  // 队列本身在后端（Writeback.pasteQueue）：它必须与"剪贴板里现在是什么"
  // 保持一致，放在前端会在面板重建后丢掉进度。前端这里只镜像它的状态。
  const refreshSeq = useCallback(() => {
    call('SequenceState')
      .then((v) => setSeq(v))
      .catch(() => {
        /* 后端没就绪时不打扰用户 */
      })
  }, [])

  // 面板每次被呼出都同步一次：队列可能在"面板收起期间"被消费过
  // （用户在我们的面板之外连着贴了几条）。
  useEffect(() => {
    refreshSeq()
  }, [refreshSeq, p.focusSearchNonce])

  const startSequence = useCallback(
    async (ids: number[]) => {
      try {
        await call('StartSequence', ids)
        setSelectedIds(new Set())
        refreshSeq()
        // 第一条已经贴出去了，收起面板让用户回到目标 App 里。
        // 不收起的话，用户会以为"点了没反应"（内容进了剪贴板，但面板挡住了目标窗口）。
        p.onHide()
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t, refreshSeq],
  )

  const nextInSequence = useCallback(async () => {
    try {
      const res = await call('NextInSequence')
      if (res.note) p.onToast(res.note)
      refreshSeq()
    } catch {
      // 队列空了会走到这里：这不是错误，是"贴完了"。进度提示已经由
      // 后端通过事件推过来了（seq.finished），所以这里只刷新状态。
      refreshSeq()
    }
  }, [p, refreshSeq])

  const clearSequence = useCallback(async () => {
    try {
      await call('ClearSequence')
    } finally {
      refreshSeq()
    }
  }, [refreshSeq])

  // ── 操作 ──────────────────────────────────────────────────────

  const paste = useCallback(
    async (id: number) => {
      try {
        const autoPaste = (settings?.ui?.pasteMode ?? 'clipboard') === 'autoPaste'
        const res: PasteResult = await call('Paste', id, autoPaste)
        // 降级必须如实告知：§13 风险表要求"不假装贴上了"。
        if (res.note) p.onToast(res.note)
        p.onHide()
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [settings, p, t],
  )

  const copyOnly = useCallback(
    async (id: number) => {
      try {
        const res = await call('CopyOnly', id)
        if (res.note) p.onToast(res.note)
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t],
  )

  const togglePin = useCallback(
    async (id: number, pinned: boolean) => {
      try {
        await call('Pin', [id], pinned)
        setRows((prev) => prev.map((r) => (r.id === id ? { ...r, pinned } : r)))
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t],
  )

  const remove = useCallback(
    async (id: number) => {
      try {
        await call('Delete', [id])
        setRows((prev) => prev.filter((r) => r.id !== id))
        setSelectedIds((prev) => {
          const n = new Set(prev)
          n.delete(id)
          return n
        })
        if (previewId === id) setPreviewId(null)
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t, previewId],
  )

  const restore = useCallback(
    async (ids: number[]) => {
      try {
        const res = await call('Restore', ids)
        // conflict 必须说出来：用户需要知道"为什么那几条没回来"。
        if (res && res.conflict > 0) p.onToast(t('trash.conflict', { n: res.conflict }))
        else if (res) p.onToast(t('trash.restored', { n: res.restored }))
        refresh()
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t, refresh],
  )

  const purge = useCallback(
    async (ids: number[]) => {
      try {
        await call('Purge', ids)
        setRows((prev) => prev.filter((r) => !ids.includes(r.id)))
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t],
  )

  const emptyTrash = useCallback(async () => {
    if (!window.confirm(t('trash.emptyConfirm'))) return
    try {
      const n = await call('EmptyTrash')
      p.onToast(t('trash.restored', { n }))
      refresh()
    } catch (e: unknown) {
      p.onToast(t('err.generic', { err: msg(e) }))
    }
  }, [p, t, refresh])

  const removeMany = useCallback(
    async (ids: number[]) => {
      try {
        await call('Delete', ids)
        setRows((prev) => prev.filter((r) => !ids.includes(r.id)))
        setSelectedIds(new Set())
      } catch (e: unknown) {
        p.onToast(t('err.generic', { err: msg(e) }))
      }
    },
    [p, t],
  )

  const revealFile = useCallback(async (path: string) => {
    try {
      const app = window.go?.main?.App as { RevealPath?: (p: string) => Promise<void> } | undefined
      await app?.RevealPath?.(path)
    } catch {
      // 定位文件失败不是致命问题（可能是文件已被移走），静默即可。
    }
  }, [])

  // ── 键盘 ──────────────────────────────────────────────────────

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey

      // ⌘/Ctrl + 1..9：直贴第 N 条。必须在最前面判，因为它要吞掉默认行为
      // （某些浏览器/WebView 把 ⌘1 当"切标签页"）。
      if (mod && /^[1-9]$/.test(e.key)) {
        const idx = Number(e.key) - 1
        if (idx < quickPasteCount && idx < rows.length) {
          e.preventDefault()
          void paste(rows[idx].id)
          return
        }
      }

      // ⌘/Ctrl + Enter：取连续粘贴队列的下一项。
      //
      // 为什么不复用 Enter：Enter 是"贴光标那一行"，是最高频、最危险
      // （一旦误触就把内容贴进目标 App）的动作。队列消费是**第二步**动作，
      // 必须与它分开，否则用户在队列激活时按 Enter 会拿到意料之外的东西。
      if (mod && (e.key === 'Enter' || e.key === 'NumpadEnter')) {
        if (seq?.active) {
          e.preventDefault()
          void nextInSequence()
          return
        }
      }

      // Esc：第一级清搜索/选择，第二级收起面板。
      if (e.key === 'Escape') {
        e.preventDefault()
        if (previewId != null) {
          setPreviewId(null)
        } else if (text !== '' || selectedIds.size > 0) {
          setText('')
          setSelectedIds(new Set())
        } else {
          p.onHide()
        }
        return
      }

      // 焦点在输入框里时，除了"上下/回车/空格"，其余交给输入框自己处理。
      const inInput = document.activeElement === inputRef.current

      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault()
        if (rows.length === 0) return
        const cur = rows.findIndex((r) => r.id === activeId)
        const next = e.key === 'ArrowDown' ? Math.min(cur + 1, rows.length - 1) : Math.max(cur - 1, 0)
        const row = rows[next < 0 ? 0 : next]
        setActiveId(row.id)
        setSelectedIds(new Set([row.id]))
        // 让光标行进入视口。用 scrollIntoView({block:'nearest'})：
        // 'center' 会在每条切换时都滚动，视觉上很跳。
        document.querySelector(`[data-id="${row.id}"]`)?.scrollIntoView({ block: 'nearest' })
        return
      }

      if (e.key === 'Enter') {
        if (activeId == null) return
        e.preventDefault()
        void paste(activeId)
        return
      }

      if (e.key === ' ' && !inInput) {
        // 空格预览：只在焦点不在输入框时生效，否则打字就废了。
        if (activeId == null) return
        e.preventDefault()
        setPreviewId(activeId)
        return
      }

      if ((e.key === 'Backspace' || e.key === 'Delete') && !inInput) {
        if (activeId == null) return
        e.preventDefault()
        if (trashed) void purge([activeId])
        else void remove(activeId)
        return
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [
    rows, activeId, text, selectedIds, previewId, quickPasteCount,
    paste, remove, purge, removeMany, trashed, p,
    seq, nextInSequence,
  ])

  // 面板被呼出时把焦点给搜索框：这是"呼出即可打字"的关键。
  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [p.focusSearchNonce])

  // 每 30 秒重算一次相对时间与过期徽标。
  // 只在"面板可见且没有预览打开"时跑（有预览时用户在看具体内容，
  // 时间戳跳动反而干扰）。
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  useInterval(() => setNow(Math.floor(Date.now() / 1000)), previewId == null ? 30_000 : null)

  const selected = [...selectedIds]

  return (
    <div className="panel">
      <div className="panel-main">
        <SearchBar
          t={t}
          text={text}
          onText={setText}
          kind={kind}
          onKind={setKind}
          pinnedOnly={pinnedOnly}
          onPinnedOnly={setPinnedOnly}
          trashed={trashed}
          onTrashed={p.onTrashed}
          total={total}
          totalValid={totalValid}
          loading={loading}
          hotkeyLabel={p.hotkeyLabel}
          mode={mode}
          ftsAvailable={ftsAvailable}
          showHint={showHint}
          onToggleHint={() => setShowHint((v) => !v)}
          inputRef={inputRef}
        />

        {error && <div className="errbox">{t('err.generic', { err: error })}</div>}

        <div className="panel-grid">
          <div className="panel-listwrap">
            <ItemList
              t={t}
              rows={rows}
              activeId={activeId}
              selectedIds={selectedIds}
              quickPasteCount={quickPasteCount}
              modLabel={modLabel}
              now={now}
              trashed={trashed}
              loading={loading}
              hasMore={hasMore}
              onActivate={setActiveId}
              onSelectionChange={setSelectedIds}
              onPaste={paste}
              onTogglePin={togglePin}
              onDelete={remove}
              onRestore={(id) => void restore([id])}
              onPurge={(id) => void purge([id])}
              onLoadMore={loadMore}
            />

            {selected.length > 1 && (
              <div className="batchbar">
                <span>{t('list.count', { n: selected.length })}</span>
                {trashed ? (
                  <>
                    <button type="button" className="btn" onClick={() => void restore(selected)}>
                      <IconRestore size={13} /> {t('action.restore')}
                    </button>
                    <button type="button" className="btn danger" onClick={() => void purge(selected)}>
                      <IconTrash size={13} /> {t('action.purge')}
                    </button>
                  </>
                ) : (
                  <>
                    <button
                      type="button"
                      className="btn"
                      onClick={async () => {
                        await call('Pin', selected, true)
                        refresh()
                      }}
                    >
                      {t('action.pin')}
                    </button>
                    <button
                      type="button"
                      className="btn"
                      title={t('seq.startHint', { n: selected.length })}
                      onClick={() => void startSequence(selected)}
                    >
                      {t('seq.start')}
                    </button>
                    <button type="button" className="btn danger" onClick={() => void removeMany(selected)}>
                      <IconTrash size={13} /> {t('action.delete')}
                    </button>
                  </>
                )}
                <button type="button" className="btn btn-quiet" onClick={() => setSelectedIds(new Set())}>
                  {t('common.cancel')}
                </button>
              </div>
            )}
          </div>

          {previewId != null && (
            <Preview
              t={t}
              id={previewId}
              now={now}
              autoPaste={(settings?.ui?.pasteMode ?? 'clipboard') === 'autoPaste'}
              onClose={() => setPreviewId(null)}
              onCopy={(id) => void copyOnly(id)}
              onRevealFile={(path) => void revealFile(path)}
              onToast={p.onToast}
            />
          )}
        </div>
      </div>

      {seq?.active && (
        <div className="seqstrip">
          <span className="seqstrip-label">
            {t('seq.remaining', { n: seq.remaining, total: seq.total })}
          </span>
          <button type="button" className="btn" onClick={() => void nextInSequence()}>
            {t('seq.next')} <span className="dim">{t('seq.keyHint', { mod: modKey(modLabel) })}</span>
          </button>
          <button type="button" className="btn btn-quiet" onClick={() => void clearSequence()}>
            {t('seq.clear')}
          </button>
        </div>
      )}

      <footer className="panel-foot">
        <div className="panel-foot-left">
          {trashed ? (
            <>
              <span className="dim">{t('list.trash')}</span>
              <button type="button" className="linkbtn danger" onClick={() => void emptyTrash()}>
                {t('trash.empty')}
              </button>
            </>
          ) : (
            <span className="dim">
              {t('list.count', { n: totalValid ? total : rows.length })}
            </span>
          )}
        </div>
        <div className="panel-foot-right">
          <button type="button" className="linkbtn" onClick={() => p.onOpenView('stats')}>
            {t('nav.stats')} <IconChevron size={11} />
          </button>
          <button type="button" className="linkbtn" onClick={() => p.onOpenView('settings')}>
            {t('nav.settings')} <IconChevron size={11} />
          </button>
        </div>
      </footer>
    </div>
  )

}

/**
 * modKey 把"⌘ / Ctrl+"这样的修饰键记号变成快捷键提示里的那一个字符。
 *
 * modLabel 是后端按平台给的（"⌘" / "Ctrl+"），这里只需要把它与 "⏎" 拼成
 * 一个看起来像快捷键的串。单列一个函数是为了让键盘提示只有一处写法。
 */
function modKey(modLabel: string): string {
  return modLabel.endsWith('+') ? modLabel.slice(0, -1) : modLabel
}

/** msg 把 unknown 错误转成一句话。 */
function msg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
