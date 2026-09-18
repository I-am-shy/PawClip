// 列表项的右键菜单。
//
// 为什么在页面里画一个菜单，而不是让后端弹原生 NSMenu：菜单项要表达"这条
// 内容是什么类型、能不能查看"这类**前端才知道**的状态，而它的动作（复制、
// 查看）本来就是前端的绑定调用。在页面里画，位置与内容都跟着数据走，
// 比为了一个菜单来回一趟原生直接。
//
// 关闭的四条路径都要盖住：点到别处、Esc、滚轮、窗口尺寸变化。少一条都会
// 留下一个"关不掉的菜单"——面板是浮在别人窗口上的小窗口，菜单留在那儿
// 比在普通页面里更刺眼。

import { useEffect, useLayoutEffect, useRef, useState } from 'react'

export type ContextMenuItem = {
  key: string
  label: string
  /** 不可用时的原因；同时作为 title，鼠标悬停能看到。 */
  hint?: string
  disabled?: boolean
  danger?: boolean
  onSelect: () => void
}

export type ContextMenuProps = {
  x: number
  y: number
  items: ContextMenuItem[]
  onClose: () => void
}

export function ContextMenu({ x, y, items, onClose }: ContextMenuProps) {
  const ref = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState({ x, y })

  // 先量一次真实尺寸再夹进视口：贴着窗口右下角弹出的菜单会有一半在窗口外，
  // 而它是个 fixed 浮层——用户没法把它拖回来，只能眼睁睁看着两项里少一项。
  // 用 layout effect 是为了赶在浏览器绘制之前完成，否则会看到菜单"跳一下"。
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const r = el.getBoundingClientRect()
    const pad = 6
    setPos({
      x: Math.max(pad, Math.min(x, window.innerWidth - r.width - pad)),
      y: Math.max(pad, Math.min(y, window.innerHeight - r.height - pad)),
    })
  }, [x, y, items.length])

  useEffect(() => {
    const close = () => onClose()
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      // 捕获阶段就吞掉：面板的 Esc 是"清搜索 → 收起面板"，
      // 菜单开着的时候用户想关的是菜单，那一级不该被触发。
      e.preventDefault()
      e.stopPropagation()
      onClose()
    }
    // ⚠️ mousedown 用**冒泡**阶段（不加第三个参数）。
    //
    // 用捕获阶段会出大问题：window 的捕获监听先于任何元素触发，于是"点菜单项"
    // 也会先被它当成"点到别处"而把菜单卸掉——React 还没来得及跑 onClick，
    // DOM 已经没了，菜单项**永远点不中**。冒泡阶段则让菜单项自己的
    // onMouseDown（stopPropagation）先把它拦住。
    window.addEventListener('mousedown', close)
    window.addEventListener('keydown', onKey, true)
    // 滚动就关：菜单固定在屏幕坐标上，列表滚走了它却还停在原地，
    // 用户会以为它指向的是滚过之后的那一行。
    window.addEventListener('wheel', close, { passive: true })
    window.addEventListener('resize', close)
    return () => {
      window.removeEventListener('mousedown', close)
      window.removeEventListener('keydown', onKey, true)
      window.removeEventListener('wheel', close)
      window.removeEventListener('resize', close)
    }
  }, [onClose])

  return (
    <div ref={ref} className="menu" style={{ left: pos.x, top: pos.y }} role="menu">
      {items.map((it) => (
        <button
          key={it.key}
          type="button"
          role="menuitem"
          className={`menu-item${it.danger ? ' menu-item-danger' : ''}`}
          disabled={it.disabled}
          title={it.hint ?? ''}
          // 菜单内部的按下不能触发上面那个"点哪儿都关"的监听。
          onMouseDown={(e) => e.stopPropagation()}
          onClick={() => {
            it.onSelect()
            onClose()
          }}
        >
          {it.label}
        </button>
      ))}
    </div>
  )
}
