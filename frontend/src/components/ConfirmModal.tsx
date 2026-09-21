// 不可恢复动作的统一确认框（草稿本彻底删除 / 回收站彻底删除 / 清空回收站 /
// 撤销上次导入）。
//
// # 为什么是自绘弹窗而不是 window.confirm
//
// 打包产物跑在 WKWebView 里，而 Wails v2.16 **只声明了 `WKUIDelegate` 的
// 遵循关系，一个 `runJavaScript*Panel` 方法都没实现**
// （wails/v2@v2.16.0/internal/frontend/desktop/darwin 全目录搜不到
// `runJavaScript`）。这套 UI 代理方法在 WebKit 里的语义就是"不实现 = 不弹"，
// `window.confirm()` 拿到的是一个被立刻回调的假值。于是
// `if (!window.confirm(...)) return` 这种写法会让按钮**按下去什么都不发生**
// ——回收站的"清空回收站"、导出页的"撤销上次导入"以前都是这个状态：
// 不是弹窗长相不对，而是那行代码之后的动作从来没被执行过。
//
// 所以本组件不只是"换个样子"，它是把这两个动作从"静默不执行"里救出来。
//
// # 为什么键盘要整段截住
//
// 面板那一层在 window 上挂着上下键（挪光标行）、回车（**直接粘贴**）、
// Backspace（删除）、⌘1..9（直贴第 N 条）。弹窗开着时这些都必须失效，
// 其中最要命的是回车：它会把内容贴进用户当时的前台应用并把面板收走，
// 而这恰好是"我正在确认一个破坏性动作"时最不该发生的事。
// 所以这里在**捕获**阶段拦下所有 keydown 并 stopPropagation——捕获先于
// 冒泡，而 window 上的捕获监听又是整条传播路径的第一个，面板与 App 挂在
// window 上的冒泡监听就都收不到了（App 的"Esc = 返回"同理）。
//
// 不用 preventDefault 一刀切：Tab 移动焦点、回车/空格激活当前按钮都是
// 浏览器的默认动作，截掉它们等于把键盘用户关在弹窗外。Tab 越界由 trapTab
// 拉回来，其余按键的默认动作放行。

import { useEffect, useId, useRef } from 'react'

export type ConfirmModalProps = {
  /** 一句问句，写明"要发生什么"（例如"彻底删除这 3 条记录？"）。 */
  title: string
  /** 后果说明，说清"能不能回来"。 */
  body: string
  /** 执行按钮上的字，必须是**动词**而不是"确定"：用户按的应该是他读到的那件事。 */
  confirmLabel: string
  cancelLabel: string
  onConfirm: () => void
  onCancel: () => void
}

export function ConfirmModal(p: ConfirmModalProps) {
  const uid = useId()
  const titleId = `${uid}-title`
  const bodyId = `${uid}-body`
  const cardRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      e.stopPropagation()
      if (e.key === 'Escape') {
        e.preventDefault()
        p.onCancel()
        return
      }
      if (e.key === 'Tab') trapTab(e, cardRef.current)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [p.onCancel])

  return (
    <div
      className="modal-mask"
      role="presentation"
      onMouseDown={(e) => {
        // 点遮罩空白 = 取消。按 mousedown 判定而不是 click：在弹窗里按下、
        // 拖到外面再松手时 click 的 target 会变成遮罩，那一下不该算"点了取消"。
        if (e.target === e.currentTarget) p.onCancel()
      }}
    >
      <div
        ref={cardRef}
        className="modal"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={bodyId}
      >
        <div className="modal-title" id={titleId}>
          {p.title}
        </div>
        <div className="modal-body" id={bodyId}>
          {p.body}
        </div>
        <div className="modal-acts">
          {/* 取消拿初始焦点：这个弹窗的默认动作必须是"不发生任何事"——
              无论用户是顺手按了回车，还是根本没看内容就敲了键盘。
              执行按钮放在取消的右边（危险动作不在第一位）。 */}
          <button type="button" className="btn btn-quiet" autoFocus onClick={p.onCancel}>
            {p.cancelLabel}
          </button>
          <button type="button" className="btn btn-danger" onClick={p.onConfirm}>
            {p.confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}

/**
 * trapTab 把 Tab 的落点限制在卡片内部。
 *
 * 不这么做的话，第二次 Tab 就会跑到遮罩后面的列表按钮上——那时焦点看不见
 * （被遮罩压着），但空格/回车照样能激活它：用户以为自己在弹窗里，实际按到
 * 的是背后那条记录的删除键。焦点一旦落在卡片外也拉回来，作为兜底。
 */
function trapTab(e: KeyboardEvent, card: HTMLElement | null) {
  if (!card) return
  const items = card.querySelectorAll<HTMLElement>('button:not([disabled])')
  if (items.length === 0) return
  const first = items[0]
  const last = items[items.length - 1]
  const cur = document.activeElement
  const inside = cur instanceof HTMLElement && card.contains(cur)
  const atEdge = e.shiftKey ? cur === first : cur === last
  if (!inside || atEdge) {
    e.preventDefault()
    ;(e.shiftKey ? last : first).focus()
  }
}
