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
//
// # 跨实例的 I/O 顺序（draftIO / sequenced）
//
// 卸载 flush 是异步的：它的 SaveDraft 还在路上时，用户切回草稿本，
// 重新挂载的 Draft(id) 可能**先到**——Wails 绑定调用之间没有顺序保证。
// 读到旧正文 → 渲染 → 之后自动保存把旧正文写回去，最后一段编辑就真丢了。
// 所以本文件里所有草稿读写都走模块级的 sequenced()（见 import 之后）。

import { useCallback, useEffect, useRef, useState } from 'react'
import type { T as TFn } from '../i18n'
import { call, type DraftList, type DraftRow } from '../api'
import { IMG_MIN_W, externalHref, findAutoLinks, htmlToMd, mdToHtml, safeHref } from '../md'
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

// ── 跨实例的 I/O 顺序链 ─────────────────────────────────────────
//
// Wails 的绑定调用之间**没有顺序保证**（各走各的 goroutine）。切走视图时
// 卸载 flush 发出的 SaveDraft 还在路上，重新挂载后的 Draft(id) 完全可能
// 先到——读到旧正文，把旧内容渲染进编辑器；之后的自动保存再把
// "旧正文 + 新输入"写回去，最后一段编辑就真丢了（用户看到的正是
// "重新进草稿本，内容少了"）。把所有草稿读写串到一条**模块级**链上：
// 组件状态活不过卸载，链必须放在模块里才能跨"卸载 → 重挂"保住
// "先写后读"的顺序。
let draftIO: Promise<unknown> = Promise.resolve()

/** sequenced 把一次草稿读写排进链：前面的（哪怕失败）落地后才轮到它。 */
function sequenced<T>(fn: () => Promise<T>): Promise<T> {
  const run = draftIO.then(fn, fn)
  // 链只保证顺序、不传播错误——错误由调用方自己 await 拿。
  draftIO = run.then(
    () => undefined,
    () => undefined,
  )
  return run
}

const NODE_TEXT = 3

/** insideAnchor：这个节点是否躺在链接里（在链接里就不做自动识别）。 */
function insideAnchor(node: Node, root: Node): boolean {
  for (let n: Node | null = node; n != null && n !== root; n = n.parentNode) {
    if (n.nodeName === 'A') return true
  }
  return false
}

/** appendTextWithLinks 把一段文本（可能含裸 URL）变成节点塞进 frag。 */
function appendTextWithLinks(frag: DocumentFragment, text: string): void {
  const ms = findAutoLinks(text)
  let pos = 0
  for (const m of ms) {
    if (m.start > pos) frag.appendChild(document.createTextNode(text.slice(pos, m.start)))
    const a = document.createElement('a')
    a.setAttribute('href', m.url)
    a.textContent = m.text
    frag.appendChild(a)
    pos = m.end
  }
  if (pos < text.length) frag.appendChild(document.createTextNode(text.slice(pos)))
}

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
  // 链接的显示文本（别名）。空串 = 沿用（选中文字 / 原文字 / 地址）。
  const [linkText, setLinkText] = useState('')
  const [marks, setMarks] = useState({ bold: false, italic: false, underline: false })
  const [dragFrom, setDragFrom] = useState<number | null>(null)
  const [dragOver, setDragOver] = useState<number | null>(null)
  // 被选中的图片与它右下角缩放把手的位置（相对 .draft-editwrap 的左上角）。
  // 把手的位置要现算而不是跟着图片走：它是一个绝对定位的**兄弟**节点，
  // 见下面 imgHandle 那段的说明。
  const [imgSel, setImgSel] = useState<HTMLImageElement | null>(null)
  const [imgBox, setImgBox] = useState<{ left: number; top: number } | null>(null)

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
  const pendingTitleRef = useRef<string | null>(null)
  const fileRef = useRef<HTMLInputElement | null>(null)
  // savedRange 是点工具栏时"用户最后一次在正文里的选区"。
  // 点按钮会让编辑区失焦，不记下来的话链接/图片只能插到末尾。
  const savedRangeRef = useRef<Range | null>(null)
  // linkEditRef 非空表示链接浮层正在"编辑这条已有的链接"而不是新建。
  const linkEditRef = useRef<HTMLAnchorElement | null>(null)
  // 编辑区的包裹层：缩放把手挂在它上面（不能挂进 contenteditable，见 imgHandle）。
  const wrapRef = useRef<HTMLDivElement | null>(null)
  // imgSel 的镜像。事件回调（mousemove / scroll / resize）不在 React 的
  // 渲染闭包里，读 state 只会读到登记那一刻的值。
  const imgSelRef = useRef<HTMLImageElement | null>(null)

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
        // 走模块级顺序链：与"重挂载后的读取"保住先后（见文件头）。
        const r = await sequenced(() => call('SaveDraft', id, md))
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

  /**
   * syncEmpty 同步"正文是否为空"的开关（占位提示靠它显隐）。
   *
   * 贴图、插链接这类**程序化插入**不触发 input 事件，必须在这里补一次，
   * 否则占位提示要等用户敲下一个键才消失——看起来就像"图没插上"。
   */
  const syncEmpty = useCallback(() => {
    const node = nodeRef.current
    if (node) setBodyEmpty(node.textContent === '' && node.querySelector('img') == null)
  }, [])

  // ── 图片缩放 ──────────────────────────────────────────────────
  //
  // 宽度存在 img 的 `width` **属性**上，也就是 md 里 `![alt|300](…)` 的那个
  // 300（见 md.ts 的 splitImgAlt）。为什么不用内联 style：style 是浏览器与
  // 用户代理随时会改写的东西，让它参与存储等于让"存下来的宽度"取决于
  // 渲染环境。
  //
  // 把手是一个绝对定位的**兄弟**节点，挂在 .draft-editwrap 上，而不是
  // 塞进 contenteditable 里。两条理由，任一条都足够：
  //   · contenteditable 的子树会被 htmlToMd 逐节点读一遍，往里加节点等于
  //     往存储格式里加东西；
  //   · 那片子树是"React 不管、我们手改 innerHTML"的，React 的 children
  //     变更会去 patch 一个已经被改乱的 DOM 树。
  // 代价是图片滚动 / 重排后位置要自己重算（placeImgHandle）。
  //
  // 这一块定义在 openDraft 之前：openDraft 的依赖数组里要引用 selectImg，
  // 而依赖数组是在渲染时**立即求值**的，放到后面就是一次 TDZ 报错。

  /** placeImgHandle 把把手摆到当前选中图片的右下角。 */
  const placeImgHandle = useCallback(() => {
    const img = imgSelRef.current
    const wrap = wrapRef.current
    if (!img || !wrap || !img.isConnected) {
      setImgBox(null)
      return
    }
    const r = img.getBoundingClientRect()
    const b = wrap.getBoundingClientRect()
    setImgBox({ left: r.right - b.left, top: r.bottom - b.top })
  }, [])

  /**
   * refreshImgSel 在内容变化后重新校准：图片被删掉（选中它按删除键）或被
   * 重排（上方文字增删、窗口改宽）时，把手要么消失、要么跟上去。
   */
  const refreshImgSel = useCallback(() => {
    if (imgSelRef.current && !imgSelRef.current.isConnected) {
      imgSelRef.current = null
      setImgSel(null)
      setImgBox(null)
      return
    }
    placeImgHandle()
  }, [placeImgHandle])

  /** selectImg 记住选中的图片，并在它身上打一个标记供 CSS 描边。 */
  const selectImg = useCallback(
    (img: HTMLImageElement | null) => {
      const prev = imgSelRef.current
      if (prev && prev !== img) prev.removeAttribute('data-sel')
      img?.setAttribute('data-sel', '1')
      imgSelRef.current = img
      setImgSel(img)
      if (img) placeImgHandle()
      else setImgBox(null)
    },
    [placeImgHandle],
  )

  const onEditorMouseDown = (e: React.MouseEvent<HTMLDivElement>) => {
    const el = e.target instanceof HTMLImageElement ? e.target : null
    selectImg(el)
    if (el) {
      // 位置要等浏览器把这一轮的布局做完。这里**不** preventDefault——
      // 拦住的话图片拿不到浏览器的选中态，接着按删除键就删不掉它。
      requestAnimationFrame(placeImgHandle)
    }
  }

  /** 双击图片清掉显式宽度，回到"按容器宽自适应"。 */
  const onEditorDoubleClick = (e: React.MouseEvent<HTMLDivElement>) => {
    const el = e.target
    if (!(el instanceof HTMLImageElement) || !el.hasAttribute('width')) return
    e.preventDefault()
    el.removeAttribute('width')
    placeImgHandle()
    markDirty()
    void flush()
  }

  /**
   * onEditorClick 让正文里的链接真的能点开。
   *
   * contenteditable 里的 `<a>` 点不动是浏览器的既定行为（编辑区把点击当成
   * 选区操作），所以要自己接管。而**绝不能放它导航**：这个 WebView 就是应用
   * 界面本身，跟着链接走一趟等于把整个面板换成网页，用户回不来。于是这里
   * 一律 preventDefault，把地址交给后端用系统浏览器打开。
   *
   * 想改链接内容时不必绕路：光标已经落在链接里，点工具栏的链接按钮就是
   * "编辑这条链接"（见 applyLink 的两种形态）。
   */
  const onEditorClick = (e: React.MouseEvent<HTMLDivElement>) => {
    const el = e.target instanceof Element ? e.target.closest('a') : null
    if (!el) return
    // 正在拖选文字（从链接里拖出去）不算"想打开它"。
    const sel = window.getSelection()
    if (sel && !sel.isCollapsed) return
    const href = externalHref(el.getAttribute('href') ?? '')
    // 无论能不能打开，都不许 WebView 自己跟着走。
    e.preventDefault()
    if (href === '') return
    // 先落盘，再把地址交出去：打开浏览器会让后端**先把面板收起**
    // （见 dialogs.go 的 hideBeforeReveal），而"收起"这条路上前端不 flush
    // （App.tsx 的 'hide' 分支是空的，草稿视图也不卸载）——最后那几百毫秒的
    // 输入会留在 DOM 里，浏览器开着、字没了。
    void (async () => {
      try {
        await flush({ force: true })
        await call('OpenURL', href)
      } catch (err: unknown) {
        onToast(t('err.generic', { err: err instanceof Error ? err.message : String(err) }))
      }
    })()
  }

  /**
   * startImgResize 拖动右下角把手改宽度。
   *
   * 上限取编辑区的可视宽度：拖过边界的话图片会被 CSS 的 `max-width: 100%`
   * 压回来，而存下来的那个宽度是用户永远看不到的数。
   */
  const startImgResize = (e: React.MouseEvent<HTMLSpanElement>) => {
    const img = imgSelRef.current
    const ed = nodeRef.current
    if (!img || !ed) return
    e.preventDefault()
    e.stopPropagation()
    selectImg(img)
    const startX = e.clientX
    const startW = img.getBoundingClientRect().width
    const maxW = Math.max(IMG_MIN_W, Math.round(ed.clientWidth) - 8)
    const move = (ev: MouseEvent) => {
      const w = Math.max(IMG_MIN_W, Math.min(maxW, Math.round(startW + (ev.clientX - startX))))
      img.setAttribute('width', String(w))
      placeImgHandle()
    }
    const up = () => {
      window.removeEventListener('mousemove', move)
      window.removeEventListener('mouseup', up)
      markDirty()
      void flush()
    }
    window.addEventListener('mousemove', move)
    window.addEventListener('mouseup', up)
  }

  // 面板宽度可以拖（§4.4.5），图片会跟着重排。
  useEffect(() => {
    window.addEventListener('resize', refreshImgSel)
    return () => window.removeEventListener('resize', refreshImgSel)
  }, [refreshImgSel])

  // 彻底删除确认框里的 Esc = 取消。
  //
  // 必须用**捕获**阶段：App 那一层也在 window 上听 Esc（非面板视图 = 返回），
  // 不先把它截下来的话，用户按 Esc 会"连同弹窗一起"退回历史页——弹窗关了、
  // 草稿本也关了，像是按了一次按钮。
  useEffect(() => {
    if (confirmPurge == null) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopPropagation()
      setConfirmPurge(null)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [confirmPurge])

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

  /** fireTitle 立刻把待写的标题落库（切草稿前必须做，见 onTitleChange）。 */
  const fireTitle = useCallback(() => {
    if (titleTimerRef.current != null) {
      window.clearTimeout(titleTimerRef.current)
      titleTimerRef.current = null
    }
    const v = pendingTitleRef.current
    pendingTitleRef.current = null
    if (v == null) return
    const id = selectedIdRef.current
    if (id == null) return
    void sequenced(() => call('RenameDraft', id, v))
      .then(() => {
        setItems((prev) => prev.map((it) => (it.id === id ? { ...it, title: v } : it)))
      })
      .catch((e: unknown) => {
        onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
      })
  }, [onToast, t])

  const openDraft = useCallback(
    async (id: number) => {
      // 先存旧的。DOM 此刻还是旧草稿（innerHTML 在下面才换），
      // 顺序反了就会把新草稿的内容写成旧草稿的。
      fireTitle()
      await flush({ force: true })
      try {
        const d = await sequenced(() => call('Draft', id))
        selectedIdRef.current = id
        setSelectedId(id)
        setTitle(d.title)
        setSavedAt(d.updatedAt)
        setChars(d.md.length)
        setSaveState('idle')
        setConfirmPurge(null)
        // 换了一篇正文，之前选中的那张图已经不在树上了。
        selectImg(null)
        lastSavedRef.current = d.md
        dirtyRef.current = false
        if (nodeRef.current) {
          nodeRef.current.innerHTML = mdToHtml(d.md)
          setBodyEmpty(d.md.trim() === '')
        }
        // 记住"上次打开的草稿"（值相同则后端不写库）。
        void sequenced(() => call('SetLastDraft', id)).catch(() => {})
      } catch (e: unknown) {
        onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
      }
    },
    [flush, onToast, t, fireTitle, selectImg],
  )

  const refresh = useCallback(async (): Promise<DraftList | null> => {
    try {
      const l = await sequenced(() => call('Drafts'))
      setList(l)
      setItems(l.items)
      setArchived(l.archived)
      return l
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
      return null
    }
  }, [onToast, t])

  // 挂载：取目录。回到**上次打开的那条**草稿（而不是固定第一条）——
  // 多条草稿时固定回第一条，看起来就像"我刚写的东西没了"。
  // 上次那条已被删除时回退到第一条。目录的收起状态也从设置里恢复。
  useEffect(() => {
    void (async () => {
      const l = await refresh()
      if (!l) return
      setTocOpen(!l.tocCollapsed)
      const target = l.items.find((it) => it.id === l.lastDraftId) ?? l.items[0]
      if (target) await openDraft(target.id)
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
    fireTitle()
    await flush({ force: true })
    try {
      const d = await sequenced(() => call('CreateDraft'))
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
        // 先落盘（标题 + 正文）再软删，并把选中清掉：不清的话紧接着的
        // openDraft 会带着一个已被软删的 id 去 force 保存，往归档区里写内容。
        fireTitle()
        await flush({ force: true })
        await sequenced(() => call('ArchiveDraft', id))
        selectedIdRef.current = null
        setSelectedId(null)
        dirtyRef.current = false
      } else {
        await sequenced(() => call('ArchiveDraft', id))
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
      await sequenced(() => call('RestoreDraft', id))
      await refresh()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  const purgeDraft = async (id: number) => {
    try {
      await sequenced(() => call('PurgeDraft', id))
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
      await sequenced(() => call('ReorderDrafts', next.map((it) => it.id)))
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    }
  }

  // 目录的收起状态是**布局必需**（面板只有 560 逻辑点），所以要落库，
  // 活过重启。值相同 SetSetting 不会重复写（键值层去重）。
  const toggleToc = () => {
    setTocOpen((v) => {
      const nv = !v
      void call('SetSetting', 'ui.draftTocCollapsed', String(nv)).catch(() => {})
      return nv
    })
  }

  // ── 标题 ──────────────────────────────────────────────────────
  //
  // 防抖期间把值挂在 pendingTitleRef 上：切草稿 / 新建 / 软删时由 fireTitle
  // **立刻**结算。不清算的话，旧草稿的标题定时器会在选中已经换到新草稿之后
  // 触发，把旧标题写到新草稿上（切得越快越容易撞上）。

  const onTitleChange = (v: string) => {
    setTitle(v)
    pendingTitleRef.current = v
    if (titleTimerRef.current != null) window.clearTimeout(titleTimerRef.current)
    titleTimerRef.current = window.setTimeout(() => {
      fireTitle()
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
      // 程序化插入不触发 input 事件，占位提示的开关要在这里补一次。
      syncEmpty()
    },
    [markDirty, syncEmpty],
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

  /**
   * anchorAt 找选区所在的链接（在编辑区内部才算）。
   *
   * 用于链接浮层的"编辑"形态：光标落在链接里再点链接按钮，
   * 预填那条链接的地址与文字，应用时改它而不是再包一层。
   */
  const anchorAt = useCallback((r: Range | null): HTMLAnchorElement | null => {
    const node = nodeRef.current
    if (!r || !node) return null
    for (let n: Node | null = r.startContainer; n != null && n !== node; n = n.parentNode) {
      if (n instanceof HTMLAnchorElement) return n
    }
    return null
  }, [])

  /**
   * applyLink 应用链接浮层。
   *
   * 两种形态（参考飞书）：
   *   · 编辑——浮层打开时光标在链接里：改那条链接的 href，
   *     给了"显示文本"就同时替换链接文字（别名）；
   *   · 新建——选了文字就包住它；没选文字就插入一条以地址为文字的链接。
   * "显示文本"留空表示沿用（选中文字 / 原有文字 / 地址）。
   */
  const applyLink = () => {
    const href = safeHref(linkValue)
    if (href === '') {
      onToast(t('draft.linkInvalid'))
      return
    }
    const alias = linkText.trim()
    const editing = linkEditRef.current
    if (editing) {
      editing.setAttribute('href', href)
      if (alias !== '') editing.textContent = alias
      linkEditRef.current = null
      setLinkOpen(false)
      setLinkValue('')
      setLinkText('')
      markDirty()
      syncEmpty()
      return
    }
    const range = currentRange()
    if (!range) return
    const a = document.createElement('a')
    a.setAttribute('href', href)
    if (range.collapsed) {
      a.textContent = alias !== '' ? alias : href
      range.insertNode(a)
    } else if (alias !== '') {
      // 显式给了别名：用别名替换选中文字。
      a.textContent = alias
      range.deleteContents()
      range.insertNode(a)
    } else {
      a.appendChild(range.extractContents())
      range.insertNode(a)
    }
    linkEditRef.current = null
    setLinkOpen(false)
    setLinkValue('')
    setLinkText('')
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
    // 一次插入整段：先把各行（连同链接识别的结果）拼进一个 fragment，
    // 再 insertNode 一次。逐节点 insertNode 的插入点始终在 range 起点，
    // 多次插入的相对顺序没有保障——多行文本会倒过来。
    const frag = document.createDocumentFragment()
    const lines = text.replace(/\r\n?/g, '\n').split('\n')
    lines.forEach((line, i) => {
      if (i > 0) frag.appendChild(document.createElement('br'))
      if (line !== '') appendTextWithLinks(frag, line)
    })
    const last = frag.lastChild
    range.insertNode(frag)
    if (last) afterInsert(range, last)
  }

  /**
   * linkifyTextNode 把一个文本节点里可识别成链接的片段换成 `<a>`。
   *
   * caret 恰好在这个节点里时按"逻辑文本偏移"映射回拆出来的新节点
   * （[前文字][<a>][后文字]）——不恢复的话光标会跳到节点外。
   */
  const linkifyTextNode = (tn: Text): boolean => {
    const text = tn.data
    const ms = findAutoLinks(text)
    if (ms.length === 0) return false
    const sel = window.getSelection()
    const caretHere = !!sel && sel.rangeCount > 0 && sel.anchorNode === tn
    const caretOff = caretHere ? sel.anchorOffset : -1

    const frag = document.createDocumentFragment()
    let pos = 0
    for (const m of ms) {
      if (m.start > pos) frag.appendChild(document.createTextNode(text.slice(pos, m.start)))
      const a = document.createElement('a')
      a.setAttribute('href', m.url)
      a.textContent = m.text
      frag.appendChild(a)
      pos = m.end
    }
    if (pos < text.length) frag.appendChild(document.createTextNode(text.slice(pos)))
    tn.replaceWith(frag)

    if (caretHere && sel) {
      let acc = 0
      let placed = false
      const place = (n: Node, off: number) => {
        const r = document.createRange()
        r.setStart(n, off)
        r.collapse(true)
        sel.removeAllRanges()
        sel.addRange(r)
        savedRangeRef.current = r.cloneRange()
        placed = true
      }
      for (const k of Array.from(frag.childNodes)) {
        if (k.nodeType === NODE_TEXT) {
          const len = k.textContent?.length ?? 0
          if (caretOff <= acc + len) {
            place(k, caretOff - acc)
            break
          }
          acc += len
        } else {
          const inner = k.firstChild
          const len = k.textContent?.length ?? 0
          if (inner && caretOff > acc && caretOff < acc + len) {
            place(inner, caretOff - acc)
            break
          }
          acc += len
        }
      }
      if (!placed) {
        const last = frag.lastChild
        if (last && last.nodeType === NODE_TEXT) place(last, last.textContent?.length ?? 0)
      }
    }
    return true
  }

  /**
   * autolinkAll 扫编辑器里所有**不在链接里**的文本节点做识别。
   *
   * 只在"敲下空格 / 回车"时触发：那是"URL 写完了"的自然信号，
   * 每次按键都扫会和正在输入的 URL 打架（参考飞书的触发时机）。
   */
  const autolinkAll = useCallback(() => {
    const node = nodeRef.current
    if (!node) return
    let changed = false
    const walk = (n: Node): void => {
      for (const child of Array.from(n.childNodes)) {
        if (child.nodeType === NODE_TEXT) {
          if (!insideAnchor(child, node)) changed = linkifyTextNode(child as Text) || changed
        } else if (child.nodeName !== 'A') {
          walk(child)
        }
      }
    }
    walk(node)
    if (changed) markDirty()
  }, [markDirty])

  const onInput = (e: React.FormEvent<HTMLDivElement>) => {
    syncEmpty()
    // 文字增删会把图片挤到别处，把手要跟上去（没有选中图片时这一步是空转）。
    refreshImgSel()
    // 触发链接识别：敲下空格 / 回车，且不在输入法组词中——
    // 组词里的空格是选字动作，不是分隔符。
    const ne = e.nativeEvent
    const isComposing = ne instanceof InputEvent && ne.isComposing
    const data = ne instanceof InputEvent ? ne.data : null
    const inputType = ne instanceof InputEvent ? ne.inputType : ''
    if (
      !isComposing &&
      (data === ' ' || inputType.includes('Paragraph') || inputType.includes('LineBreak'))
    ) {
      autolinkAll()
    }
    markDirty()
  }

  // ── 渲染 ──────────────────────────────────────────────────────

  const ttlDays = list ? Math.max(1, Math.round(list.archiveTtlSec / 86400)) : 0
  // 待确认彻底删除的那一条（null = 没有弹窗）。存整条而不是 id：弹窗要写清
  // "删的是哪一条"——归档区里可能躺着好几条标题相似的草稿。
  const purgeTarget =
    confirmPurge == null ? null : (archived.find((it) => it.id === confirmPurge) ?? null)

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
          onClick={toggleToc}
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
                    <span className="draft-toc-acts">
                      <button
                        type="button"
                        className="iconbtn"
                        title={t('draft.archiveOne')}
                        aria-label={t('draft.archiveOne')}
                        onClick={(e) => {
                          // 不然点击会先触发 li 的 openDraft。
                          e.stopPropagation()
                          void archiveDraft(it.id)
                        }}
                      >
                        <IconTrash size={12} />
                      </button>
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
                              className="iconbtn"
                              title={t('action.purge')}
                              aria-label={t('action.purge')}
                              onClick={() => setConfirmPurge(it.id)}
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
                    // 光标在链接里 → 打开成"编辑这条链接"（预填地址与文字）；
                    // 选了文字 → 预填成显示文本；什么都没有 → 全新链接。
                    const anchor = anchorAt(savedRangeRef.current)
                    linkEditRef.current = anchor
                    if (anchor) {
                      setLinkValue(anchor.getAttribute('href') ?? '')
                      setLinkText(anchor.textContent ?? '')
                    } else {
                      const r = savedRangeRef.current
                      setLinkValue('')
                      setLinkText(r && !r.collapsed ? r.toString() : '')
                    }
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
                        linkEditRef.current = null
                      }
                    }}
                  />
                  <input
                    className="draft-linkinput"
                    value={linkText}
                    placeholder={t('draft.linkTextPrompt')}
                    onChange={(e) => setLinkText(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault()
                        applyLink()
                      } else if (e.key === 'Escape') {
                        e.preventDefault()
                        e.stopPropagation()
                        setLinkOpen(false)
                        linkEditRef.current = null
                      }
                    }}
                  />
                  <button type="button" className="btn btn-quiet" onClick={applyLink}>
                    {t('draft.linkApply')}
                  </button>
                </div>
              )}

              {/* 编辑区外面包一层，只为放图片的缩放把手（见 placeImgHandle）。 */}
              <div className="draft-editwrap" ref={wrapRef}>
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
                  onMouseDown={onEditorMouseDown}
                  onClick={onEditorClick}
                  onDoubleClick={onEditorDoubleClick}
                  onScroll={refreshImgSel}
                  onFocus={rememberSelection}
                />
                {imgSel && imgBox && (
                  <span
                    className="draft-imgresize"
                    style={{ left: imgBox.left, top: imgBox.top }}
                    title={t('draft.imageResize')}
                    aria-hidden="true"
                    onMouseDown={startImgResize}
                  />
                )}
              </div>

              <div className="draft-foot dim">{t('draft.chars', { n: chars })}</div>
            </>
          )}
        </section>
      </div>

      {/* 彻底删除的确认框。
          用模态而不是"就地替换掉那一条目录"：这是全应用唯一不可恢复的动作，
          而侧栏总共 180 px，一句完整的话在那里会被切成省略号——最要紧的那半句
          （删的是哪一条）恰好是被切掉的那半句。 */}
      {purgeTarget && (
        <div
          className="modal-mask"
          role="presentation"
          onMouseDown={(e) => {
            // 点遮罩 = 取消。用 mousedown 而不是 click，是为了让"在弹窗里按下、
            // 拖到外面松手"这种误操作不会把它关掉。
            if (e.target === e.currentTarget) setConfirmPurge(null)
          }}
        >
          <div
            className="modal"
            role="alertdialog"
            aria-modal="true"
            aria-labelledby="draft-purge-title"
            aria-describedby="draft-purge-body"
          >
            <div className="modal-title" id="draft-purge-title">
              {t('draft.purgeTitle')}
            </div>
            <div className="modal-body" id="draft-purge-body">
              {t('draft.purgeBody', { name: purgeTarget.title || t('draft.untitled') })}
            </div>
            <div className="modal-acts">
              {/* 取消拿到焦点：这个弹窗的默认动作必须是"不发生任何事"。 */}
              <button
                type="button"
                className="btn btn-quiet"
                autoFocus
                onClick={() => setConfirmPurge(null)}
              >
                {t('common.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-danger"
                onClick={() => void purgeDraft(purgeTarget.id)}
              >
                {t('action.purge')}
              </button>
            </div>
          </div>
        </div>
      )}
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
