// 草稿本（docs/DESIGN.md §4.4）。
//
// # 形态
//
// 单页：左边目录、右边编辑器。目录可收起——这不是交互偏好，而是**布局必需**：
// 面板宽 560（硬上限 760，panel/panel.go:149 的 ClampPanelSize），
// 180 的目录 + 编辑器在 560 下只剩 380，再窄的富文本没法用。
//
// # 谁是真源
//
// Markdown 是唯一真源，contenteditable 只是它的编辑态呈现（见 md.ts）。
// 所以这一层**不维护一份并行的 md 状态**：编辑期间 DOM 就是真相，
// 只在保存那一刻用 htmlToMd 把它读出来。维护两份必然分叉——浏览器的
// 每一次自动纠错（补 `<br>`、拆 `<b>`）都会让它们不一致，而 user 看不见
// 哪个才是"对"的。
//
// # 实时保存的三层
//
//  1. 输入防抖（draft.autoSaveDebounceMs，默认 500ms）：停下打字就存。
//  2. 5 秒强制落盘：连续打字时防抖永远不触发，靠这一条兜住。
//  3. 硬时机：切草稿前、编辑器失焦、组件卸载（切视图 / Esc 返回）、
//     beforeunload。
//
// 第 3 条里"卸载"那条要特别小心：React 卸载后 `ref.current` 会被置空，
// 所以这里用了一个**不会被 React 清空**的自有 ref 保住节点（见 nodeRef），
// 否则最后一段文字会在切视图时丢掉。顺带一提，面板"闲置收起"目前
// **不销毁** WebView（§14 第 10 条：当前 Wails 版本没有销毁 API），
// 所以隐藏本身不会丢数据——真正会丢的是进程退出，那由 beforeunload 兜。

import { useCallback, useEffect, useRef, useState } from 'react'
import type { T as TFn } from '../i18n'
import { call, type DraftList, type DraftRow } from '../api'
import { htmlToMd, mdToHtml, safeHref } from '../md'
import { formatDateTime, formatRelative, formatTime } from '../format'
import { useInterval } from '../hooks'
import {
  IconChevron,
  IconGrip,
  IconImage,
  IconLink,
  IconPlus,
  IconRestore,
  IconSidebar,
  IconTrash,
} from '../components/Icons'

export type DraftsProps = {
  t: TFn
  onToast: (msg: string) => void
  onBack: () => void
}

/** SaveState 是状态栏那一个字。 */
type SaveState = 'idle' | 'saving' | 'saved' | 'error'

export function Drafts({ t, onToast, onBack }: DraftsProps) {
  const [list, setList] = useState<DraftList | null>(null)
  const [items, setItems] = useState<DraftRow[]>([])
  const [archived, setArchived] = useState<DraftRow[]>([])
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [title, setTitle] = useState('')
  const [saveState, setSaveState] = useState<SaveState>('idle')
  const [savedAt, setSavedAt] = useState<number | null>(null)
  const [chars, setChars] = useState(0)
  const [tocOpen, setTocOpen] = useState(true)
  const [archivedOpen, setArchivedOpen] = useState(false)
  const [confirmPurge, setConfirmPurge] = useState<number | null>(null)
  const [bodyEmpty, setBodyEmpty] = useState(true)
  const [linkOpen, setLinkOpen] = useState(false)
  const [linkValue, setLinkValue] = useState('')
  const [marks, setMarks] = useState({ bold: false, italic: false, underline: false })
  const [dragFrom, setDragFrom] = useState<number | null>(null)
  const [dragOver, setDragOver] = useState<number | null>(null)

  // ── refs ──────────────────────────────────────────────────────
  //
  // nodeRef 刻意**不用** React 的 ref 回调直接存：卸载时 React 会把它置空，
  // 而这个组件卸载时正好要读最后一次内容去落盘（见文件头第 3 条）。
  // 所以只接受非空赋值，卸载后仍指着那个已脱离文档的节点——
  // htmlToMd 只读节点树、不需要布局，脱离文档照样能读。
  const nodeRef = useRef<HTMLDivElement | null>(null)
  const attachNode = useCallback((el: HTMLDivElement | null) => {
    if (el) nodeRef.current = el
  }, [])

  const selectedIdRef = useRef<number | null>(null)
  const lastSavedRef = useRef('')
  const dirtyRef = useRef(false)
  const saveTimerRef = useRef<number | null>(null)
  const titleTimerRef = useRef<number | null>(null)
  const fileRef = useRef<HTMLInputElement | null>(null)
  // savedRange 是点工具栏时"用户最后一次在正文里的选区"。
  // 点按钮会让编辑区失焦，不记下来的话链接/图片只能插到末尾。
  const savedRangeRef = useRef<Range | null>(null)

  const debounceMs = list?.autoSaveDebounceMs ?? 500
  const maxImageLabel = list?.maxImageLabel ?? ''

  // ── 保存 ──────────────────────────────────────────────────────

  /**
   * flush 把正文写回库。
   *
   * force 的两层含义要分清：**跳过"内容没变就不写"的短路**（切草稿时
   * 需要它），以及**在节点已脱离文档时也能读**（卸载时走这条）。
   * 它不改变"写什么"——写的永远是当前 DOM。
   */
  const flush = useCallback(
    async (opts?: { force?: boolean }): Promise<void> => {
      if (saveTimerRef.current != null) {
        window.clearTimeout(saveTimerRef.current)
        saveTimerRef.current = null
      }
      const id = selectedIdRef.current
      const node = nodeRef.current
      if (id == null || !node) return

      const md = htmlToMd(node)
      if (!opts?.force && md === lastSavedRef.current) {
        dirtyRef.current = false
        return
      }
      try {
        const r = await call('SaveDraft', id, md)
        // 期间切走了就不认这次结果：否则会把新草稿的状态显示成旧草稿的。
        if (selectedIdRef.current !== id) return
        lastSavedRef.current = md
        dirtyRef.current = false
        setSavedAt(r.updatedAt)
        setChars(r.chars)
        setSaveState('saved')
        // 目录里的摘要与字数跟着变，但**不重排目录**：排序键是创建顺序，
        // 不是 updated_at（实时保存会不停改它，正在编辑的那条会一直往上跳）。
        setItems((prev) =>
          prev.map((it) => (it.id === id ? { ...it, updatedAt: r.updatedAt, chars: r.chars } : it)),
        )
      } catch (e: unknown) {
        if (selectedIdRef.current !== id) return
        dirtyRef.current = true
        setSaveState('error')
        const msg = e instanceof Error ? e.message : String(e)
        // 草稿在编辑期间被删掉了：这句要说清"改动没保存"，
        // 而不是抛一句看不出所以然的后端诊断。
        onToast(msg.includes('删除') || msg.includes('deleted') ? t('draft.gone') : t('draft.saveFailed', { err: msg }))
      }
    },
    [onToast, t],
  )

  /** markDirty 在每一次内容变化后调用：置脏、排防抖。 */
  const markDirty = useCallback(() => {
    dirtyRef.current = true
    setSaveState('saving')
    if (saveTimerRef.current != null) window.clearTimeout(saveTimerRef.current)
    saveTimerRef.current = window.setTimeout(() => {
      void flush()
    }, debounceMs)
  }, [flush, debounceMs])

  // 第 2 层：连续打字时防抖永不触发，靠这个 5 秒的 tick 强制落盘。
  useInterval(() => {
    if (dirtyRef.current) void flush()
  }, 5000)

  // 第 3 层：卸载（切视图 / Esc 返回）与进程退出。
  useEffect(() => {
    const onUnload = () => {
      void flush({ force: true })
    }
    window.addEventListener('beforeunload', onUnload)
    return () => {
      window.removeEventListener('beforeunload', onUnload)
    }
  }, [flush])

  useEffect(() => {
    return () => {
      void flush({ force: true })
    }
  }, [flush])

  // ── 目录 ──────────────────────────────────────────────────────

  const openDraft = useCallback(
    async (id: number) => {
      // 先存旧的。DOM 此刻还是旧草稿（innerHTML 在下面才换），
      // 顺序反了就会把新草稿的内容写成旧草稿的。
      await flush({ force: true })
      try {
        const d = await call('Draft', id)
        selectedIdRef.current = id
        setSelectedId(id)
        setTitle(d.title)
        setSavedAt(d.updatedAt)
        setChars(d.md.length)
        setSaveState('idle')
        setConfirmPurge(null)
        lastSavedRef.current = d.md
        dirtyRef.current = false
        if (nodeRef.current) {
          nodeRef.current.innerHTML = mdToHtml(d.md)
          setBodyEmpty(d.md.trim() === '')
        }
      } catch (e: unknown) {
        onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
      }
    },
    [flush, onToast, t],
  )

  const refresh = useCallback(async (): Promise<DraftList | null> => {
    try {
      const l = await call('Drafts')
      setList(l)
      setItems(l.items)
      setArchived(l.archived)
      return l
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
      return null
    }
  }, [onToast, t])

  // 挂载：取目录。有草稿就打开第一条——目录已经排在左边，
  // 用户第一眼就能看到"有哪些"，不需要额外再点一下。
  useEffect(() => {
    void (async () => {
      const l = await refresh()
      if (l && l.items.length > 0) await openDraft(l.items[0].id)
    })()
    // 只跑一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 选中的那条要滚进视野：新建的草稿按创建顺序排在**最后**，
  // 不滚的话用户点完"新建"会看不出发生了什么。
  useEffect(() => {
    if (selectedId == null || !tocOpen) return
    const el = document.querySelector(`[data-draft-id="${selectedId}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  }, [selectedId, tocOpen])

  const createDraft = async () => {
    await flush({ force: true })
    try {
      const d = await call('CreateDraft')
      const l = await refresh()
      if (!l) return
      await openDraft(d.id)
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  const archiveDraft = async (id: number) => {
    try {
      if (selectedIdRef.current === id) {
        // 先落盘再软删，并把选中清掉：不清的话紧接着的 openDraft
        // 会带着一个已被软删的 id 去 force 保存，往归档区里写内容。
        await flush({ force: true })
        await call('ArchiveDraft', id)
        selectedIdRef.current = null
        setSelectedId(null)
        dirtyRef.current = false
      } else {
        await call('ArchiveDraft', id)
      }
      const l = await refresh()
      if (l && selectedIdRef.current == null && l.items.length > 0) {
        await openDraft(l.items[0].id)
      }
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  const restoreDraft = async (id: number) => {
    try {
      await call('RestoreDraft', id)
      await refresh()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  const purgeDraft = async (id: number) => {
    try {
      await call('PurgeDraft', id)
      setConfirmPurge(null)
      await refresh()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  /**
   * moveItem 把目录里第 from 项挪到第 to 项，并落库。
   *
   * 先改本地再发请求：拖拽的手感取决于"松手瞬间就到位"，而 IPC 往返
   * 有几十毫秒的可见延迟。落库失败也不回滚——目录顺序不是数据，
   * 它错了顶多是下次打开时顺序不对，而回滚会让画面跳一下。
   */
  const moveItem = async (from: number, to: number) => {
    if (from === to || from < 0 || to < 0) return
    const next = items.slice()
    const [moved] = next.splice(from, 1)
    next.splice(to, 0, moved)
    setItems(next)
    try {
      await call('ReorderDrafts', next.map((it) => it.id))
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  // ── 标题 ──────────────────────────────────────────────────────

  const onTitleChange = (v: string) => {
    setTitle(v)
    if (titleTimerRef.current != null) window.clearTimeout(titleTimerRef.current)
    titleTimerRef.current = window.setTimeout(() => {
      const id = selectedIdRef.current
      if (id == null) return
      void call('RenameDraft', id, v)
        .then(() => {
          setItems((prev) => prev.map((it) => (it.id === id ? { ...it, title: v } : it)))
        })
        .catch((e: unknown) => {
          onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
        })
    }, debounceMs)
  }

  // ── 编辑区 ────────────────────────────────────────────────────

  const rememberSelection = useCallback(() => {
    const sel = window.getSelection()
    const node = nodeRef.current
    if (!sel || sel.rangeCount === 0 || !node) return
    if (!node.contains(sel.anchorNode)) return
    savedRangeRef.current = sel.getRangeAt(0).cloneRange()
  }, [])

  const currentRange = useCallback((): Range | null => {
    const node = nodeRef.current
    if (!node) return null
    node.focus()
    const sel = window.getSelection()
    if (sel && sel.rangeCount > 0 && node.contains(sel.anchorNode)) {
      return sel.getRangeAt(0)
    }
    const r = savedRangeRef.current
    if (r && node.contains(r.commonAncestorContainer)) {
      sel?.removeAllRanges()
      sel?.addRange(r)
      return r
    }
    // 没有可信的选区时落到末尾：比什么都不发生好——用户按了按钮，
    // 总得看到点反应。
    const r2 = document.createRange()
    r2.selectNodeContents(node)
    r2.collapse(false)
    sel?.removeAllRanges()
    sel?.addRange(r2)
    return r2
  }, [])

  const afterInsert = useCallback(
    (range: Range, node: Node) => {
      range.setStartAfter(node)
      range.collapse(true)
      const sel = window.getSelection()
      sel?.removeAllRanges()
      sel?.addRange(range)
      savedRangeRef.current = range.cloneRange()
      markDirty()
    },
    [markDirty],
  )

  const exec = (cmd: string) => {
    currentRange()
    document.execCommand(cmd, false)
    markDirty()
    syncMarks()
  }

  /** syncMarks 让工具栏按钮显示"当前是否生效"。 */
  const syncMarks = useCallback(() => {
    const node = nodeRef.current
    const sel = window.getSelection()
    if (!node || !sel || sel.rangeCount === 0 || !node.contains(sel.anchorNode)) return
    setMarks({
      bold: document.queryCommandState('bold'),
      italic: document.queryCommandState('italic'),
      underline: document.queryCommandState('underline'),
    })
  }, [])

  useEffect(() => {
    document.addEventListener('selectionchange', syncMarks)
    return () => document.removeEventListener('selectionchange', syncMarks)
  }, [syncMarks])

  const applyLink = () => {
    const href = safeHref(linkValue)
    if (href === '') {
      onToast(t('draft.linkInvalid'))
      return
    }
    const range = currentRange()
    if (!range) return
    const a = document.createElement('a')
    a.setAttribute('href', href)
    if (range.collapsed) {
      a.textContent = href
      range.insertNode(a)
    } else {
      a.appendChild(range.extractContents())
      range.insertNode(a)
    }
    setLinkOpen(false)
    setLinkValue('')
    afterInsert(range, a)
  }

  const insertImage = async (file: File) => {
    const lim = list?.maxImageBytes ?? 0
    if (lim > 0 && file.size > lim) {
      onToast(t('draft.imageLimit', { limit: maxImageLabel }))
      return
    }
    try {
      const dataUrl = await readAsDataURL(file)
      const img = await call('ImportDraftImage', file.type, dataUrl)
      const range = currentRange()
      if (!range) return
      const el = document.createElement('img')
      el.setAttribute('src', img.url)
      el.setAttribute('alt', '')
      range.deleteContents()
      range.insertNode(el)
      // 图片后面补一个换行：不然光标会贴在图片上，接着打字是"在图片里"。
      const br = document.createElement('br')
      el.after(br)
      afterInsert(range, br)
    } catch (e: unknown) {
      onToast(t('draft.imageFailed', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  /**
   * onPaste 把粘进来的东西一律转成**我们自己的**节点。
   *
   * 这是整个草稿本的安全边界。直接让浏览器插入剪贴板里的 HTML 有两个问题：
   * 一是带着 `<script>`、`onerror`、内联样式的碎片会真的进 DOM（在
   * contenteditable 里插入 HTML 是会执行脚本的），而草稿的文案再被
   * md.ts 读一遍写进库；二是那些碎片会被 htmlToMd 当"未知内联元素"
   * 穿透，于是库里多出一堆来源不明的标记。所以这里只取 text/plain，
   * 换行转成 `<br>`（见 md.ts 的段落模型：段内换行才是 `<br>`）。
   */
  const onPaste = (e: React.ClipboardEvent) => {
    const dt = e.clipboardData
    if (!dt) return
    const file = Array.from(dt.items ?? []).find(
      (it) => it.kind === 'file' && it.type.startsWith('image/'),
    )
    e.preventDefault()
    if (file) {
      const f = file.getAsFile()
      if (f) void insertImage(f)
      return
    }
    const text = dt.getData('text/plain')
    if (text === '') return
    const range = currentRange()
    if (!range) return
    range.deleteContents()
    const lines = text.replace(/\r\n?/g, '\n').split('\n')
    let last: Node | null = null
    lines.forEach((line, i) => {
      if (i > 0) {
        last = document.createElement('br')
        range.insertNode(last)
      }
      if (line !== '') {
        const tn = document.createTextNode(line)
        range.insertNode(tn)
        last = tn
      }
    })
    if (last) afterInsert(range, last)
  }

  const onInput = () => {
    const node = nodeRef.current
    if (node) setBodyEmpty(node.textContent === '' && node.querySelector('img') == null)
    markDirty()
  }

  // ── 渲染 ──────────────────────────────────────────────────────

  const ttlDays = list ? Math.max(1, Math.round(list.trashTtlSec / 86400)) : 0

  return (
    <div className="view-body drafts">
      <div className="view-head">
        <button type="button" className="linkbtn" onClick={onBack}>
          ← {t('nav.list')}
        </button>
        <h2>{t('nav.drafts')}</h2>
        <button
          type="button"
          className={`iconbtn ${tocOpen ? 'iconbtn-on' : ''}`}
          title={tocOpen ? t('draft.toc.collapse') : t('draft.toc.expand')}
          aria-label={tocOpen ? t('draft.toc.collapse') : t('draft.toc.expand')}
          onClick={() => setTocOpen((v) => !v)}
        >
          <IconSidebar size={14} />
        </button>
        <button type="button" className="btn btn-quiet" onClick={() => void createDraft()}>
          <IconPlus size={13} /> {t('draft.new')}
        </button>
      </div>

      <div className={`draft-split ${tocOpen ? '' : 'draft-toc-hidden'}`}>
        {tocOpen && (
          <aside className="draft-toc" aria-label={t('draft.toc')}>
            {items.length === 0 ? (
              <div className="draft-toc-empty dim">{t('draft.empty')}</div>
            ) : (
              <ul className="draft-toc-list">
                {items.map((it, i) => (
                  <li
                    key={it.id}
                    data-draft-id={it.id}
                    className={[
                      'draft-toc-item',
                      it.id === selectedId ? 'draft-toc-on' : '',
                      dragOver === i && dragFrom != null && dragFrom !== i ? 'draft-toc-over' : '',
                    ]
                      .filter(Boolean)
                      .join(' ')}
                    draggable
                    onDragStart={() => setDragFrom(i)}
                    onDragOver={(e) => {
                      e.preventDefault()
                      setDragOver(i)
                    }}
                    onDrop={(e) => {
                      e.preventDefault()
                      const from = dragFrom
                      setDragFrom(null)
                      setDragOver(null)
                      if (from != null) void moveItem(from, i)
                    }}
                    onDragEnd={() => {
                      setDragFrom(null)
                      setDragOver(null)
                    }}
                    onClick={() => {
                      if (it.id !== selectedId) void openDraft(it.id)
                    }}
                  >
                    <span className="draft-grip" title={t('draft.reorderHint')}>
                      <IconGrip size={12} />
                    </span>
                    <span className="draft-toc-main">
                      <span className="draft-toc-title">{it.title || t('draft.untitled')}</span>
                      <span className="draft-toc-sub">
                        {it.snippet || formatRelative(t, it.updatedAt)}
                      </span>
                    </span>
                  </li>
                ))}
              </ul>
            )}

            {/* 归档区。默认收起：它是"我去哪儿找回来"的地方，不是日常要看的。 */}
            <div className="draft-arch-head">
              <button
                type="button"
                className="linkbtn"
                onClick={() => setArchivedOpen((v) => !v)}
                aria-expanded={archivedOpen}
              >
                <IconChevron size={12} className={archivedOpen ? 'chev-open' : ''} />
                {t('draft.archived')}
                {archived.length > 0 ? ` (${archived.length})` : ''}
              </button>
            </div>
            {archivedOpen && (
              <div className="draft-arch">
                {archived.length === 0 ? (
                  <div className="dim draft-note">{t('draft.archivedEmpty')}</div>
                ) : (
                  <>
                    <div className="dim draft-note">{t('draft.archivedNote', { d: ttlDays })}</div>
                    <ul className="draft-toc-list">
                      {archived.map((it) => (
                        <li key={it.id} className="draft-toc-item draft-toc-arch">
                          <span className="draft-toc-main">
                            <span className="draft-toc-title">{it.title || t('draft.untitled')}</span>
                            <span className="draft-toc-sub">
                              {formatDateTime(it.archivedAt ?? it.updatedAt)}
                            </span>
                          </span>
                          <span className="draft-arch-acts">
                            <button
                              type="button"
                              className="iconbtn"
                              title={t('action.restore')}
                              aria-label={t('action.restore')}
                              onClick={() => void restoreDraft(it.id)}
                            >
                              <IconRestore size={13} />
                            </button>
                            <button
                              type="button"
                              className={`iconbtn ${confirmPurge === it.id ? 'iconbtn-danger' : ''}`}
                              title={confirmPurge === it.id ? t('draft.purgeConfirm') : t('action.purge')}
                              aria-label={confirmPurge === it.id ? t('draft.purgeConfirm') : t('action.purge')}
                              onClick={() => {
                                // 两步确认而不是弹原生 confirm()：WKWebView 里
                                // 原生对话框要后端实现代理才能出来，而这只是
                                // 一个"再点一下"的动作。
                                if (confirmPurge === it.id) void purgeDraft(it.id)
                                else setConfirmPurge(it.id)
                              }}
                            >
                              <IconTrash size={13} />
                            </button>
                          </span>
                        </li>
                      ))}
                    </ul>
                  </>
                )}
              </div>
            )}
          </aside>
        )}

        <section className="draft-main">
          {selectedId == null ? (
            <div className="draft-empty">
              <div className="draft-empty-t">{t('draft.empty')}</div>
              <div className="dim">{t('draft.emptyHint')}</div>
              <button type="button" className="btn" onClick={() => void createDraft()}>
                <IconPlus size={13} /> {t('draft.new')}
              </button>
            </div>
          ) : (
            <>
              <div className="draft-bar">
                <input
                  className="draft-title"
                  value={title}
                  placeholder={t('draft.titlePlaceholder')}
                  onChange={(e) => onTitleChange(e.target.value)}
                />
                <span className={`draft-status ${saveState === 'error' ? 'draft-status-err' : ''}`}>
                  {saveState === 'saving'
                    ? t('draft.saving')
                    : saveState === 'saved'
                      ? t('draft.savedAt', { t: formatTime(savedAt) })
                      : saveState === 'error'
                        ? t('draft.saveFailed', { err: '' })
                        : savedAt
                          ? t('draft.savedAt', { t: formatTime(savedAt) })
                          : t('draft.saved')}
                </span>
                <button
                  type="button"
                  className="iconbtn"
                  title={t('action.delete')}
                  aria-label={t('action.delete')}
                  onClick={() => void archiveDraft(selectedId)}
                >
                  <IconTrash size={13} />
                </button>
              </div>

              <div className="draft-tools">
                <button
                  type="button"
                  className={`draft-tool draft-tool-b ${marks.bold ? 'draft-tool-on' : ''}`}
                  title={t('draft.bold')}
                  aria-label={t('draft.bold')}
                  aria-pressed={marks.bold}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => exec('bold')}
                >
                  B
                </button>
                <button
                  type="button"
                  className={`draft-tool draft-tool-i ${marks.italic ? 'draft-tool-on' : ''}`}
                  title={t('draft.italic')}
                  aria-label={t('draft.italic')}
                  aria-pressed={marks.italic}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => exec('italic')}
                >
                  I
                </button>
                <button
                  type="button"
                  className={`draft-tool draft-tool-u ${marks.underline ? 'draft-tool-on' : ''}`}
                  title={t('draft.underline')}
                  aria-label={t('draft.underline')}
                  aria-pressed={marks.underline}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => exec('underline')}
                >
                  U
                </button>
                <span className="draft-tool-sep" />
                <button
                  type="button"
                  className={`iconbtn ${linkOpen ? 'iconbtn-on' : ''}`}
                  title={t('draft.link')}
                  aria-label={t('draft.link')}
                  onMouseDown={(e) => {
                    // 先记住选区，再开输入框：点按钮本身会改选区。
                    e.preventDefault()
                    rememberSelection()
                  }}
                  onClick={() => {
                    setLinkValue('')
                    setLinkOpen((v) => !v)
                  }}
                >
                  <IconLink size={14} />
                </button>
                <button
                  type="button"
                  className="iconbtn"
                  title={t('draft.image')}
                  aria-label={t('draft.image')}
                  onMouseDown={(e) => {
                    e.preventDefault()
                    rememberSelection()
                  }}
                  onClick={() => fileRef.current?.click()}
                >
                  <IconImage size={14} />
                </button>
                <input
                  ref={fileRef}
                  type="file"
                  accept="image/*"
                  className="draft-file"
                  onChange={(e) => {
                    const f = e.target.files?.[0]
                    e.target.value = ''
                    if (f) void insertImage(f)
                  }}
                />
                {maxImageLabel && (
                  <span className="dim draft-note draft-limit">
                    {t('draft.imageLimit', { limit: maxImageLabel })}
                  </span>
                )}
              </div>

              {linkOpen && (
                <div className="draft-linkpop">
                  <input
                    autoFocus
                    className="draft-linkinput"
                    value={linkValue}
                    placeholder={t('draft.linkPrompt')}
                    onChange={(e) => setLinkValue(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault()
                        applyLink()
                      } else if (e.key === 'Escape') {
                        // 先关这个浮层，别让 Esc 直接跳回历史页——
                        // 那是 App 那一层的处理（顶层键盘）。
                        e.preventDefault()
                        e.stopPropagation()
                        setLinkOpen(false)
                      }
                    }}
                  />
                  <button type="button" className="btn btn-quiet" onClick={applyLink}>
                    {t('draft.linkApply')}
                  </button>
                </div>
              )}

              <div
                ref={attachNode}
                className="draft-editor"
                contentEditable
                suppressContentEditableWarning
                spellCheck={false}
                role="textbox"
                aria-multiline="true"
                aria-label={t('draft.bodyPlaceholder')}
                data-placeholder={t('draft.bodyPlaceholder')}
                data-empty={bodyEmpty ? '1' : '0'}
                onInput={onInput}
                onPaste={onPaste}
                onBlur={() => void flush()}
                onKeyUp={syncMarks}
                onMouseUp={rememberSelection}
                onFocus={rememberSelection}
              />

              <div className="draft-foot dim">{t('draft.chars', { n: chars })}</div>
            </>
          )}
        </section>
      </div>
    </div>
  )
}

/** readAsDataURL 读成 `data:…;base64,…`（后端会自己剥前缀）。 */
function readAsDataURL(f: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onerror = () => reject(new Error('read failed'))
    r.onload = () => resolve(String(r.result ?? ''))
    r.readAsDataURL(f)
  })
}
