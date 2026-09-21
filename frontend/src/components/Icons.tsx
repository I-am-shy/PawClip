// 内联 SVG 图标。
//
// 为什么不用图标库：docs/DESIGN.md §0.4 冻结的技术栈里没有 UI 库，§14 第 12 条要求
// 前端产物压到 150 KB gzip 以内。一个图标库动辄几十 KB，而这里总共只需要
// 十来个 24×24 的轮廓图——手写一遍更小，也没有"引入后被 tree-shaking 漏掉"
// 的风险。
//
// 统一约定：24×24 viewBox、stroke 用 currentColor、描边宽 1.6。
// 颜色靠 CSS 的 color 继承，所以图标本身不出现任何硬编码颜色。

import type { ReactNode } from 'react'

type IconProps = {
  size?: number
  className?: string
}

function Svg({ size = 16, className, children }: IconProps & { children: ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
      focusable="false"
    >
      {children}
    </svg>
  )
}

export function IconSearch(p: IconProps) {
  return (
    <Svg {...p}>
      <circle cx="11" cy="11" r="6.5" />
      <path d="M16 16l4.5 4.5" />
    </Svg>
  )
}

export function IconX(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M6 6l12 12M18 6L6 18" />
    </Svg>
  )
}

export function IconPin(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M9.5 3.5h5l-.8 5.2 3.3 3.3H6.9l3.4-3.3z" />
      <path d="M12 12v8.5" />
    </Svg>
  )
}

export function IconCopy(p: IconProps) {
  return (
    <Svg {...p}>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M15 5.5A2.5 2.5 0 0 0 12.5 3H6a2 2 0 0 0-2 2v7.5A2.5 2.5 0 0 0 6.5 15" />
    </Svg>
  )
}

export function IconTrash(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M4 6.5h16M9.5 6.5V4.5h5v2" />
      <path d="M6.5 6.5l1 13h9l1-13" />
      <path d="M10.5 10.5v5.5M13.5 10.5v5.5" />
    </Svg>
  )
}

export function IconRestore(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M4 10.5A8 8 0 1 1 6.4 16.6" />
      <path d="M4 4.5v6h6" />
    </Svg>
  )
}

export function IconImage(p: IconProps) {
  return (
    <Svg {...p}>
      <rect x="3.5" y="4.5" width="17" height="15" rx="2" />
      <circle cx="9" cy="10" r="1.6" />
      <path d="M4 16.5l4.5-4 3.5 3 3-2.5 5 4" />
    </Svg>
  )
}

export function IconFile(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M6 3.5h7l5 5v12H6z" />
      <path d="M13 3.5v5h5" />
    </Svg>
  )
}

export function IconText(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M5 6.5h14M5 11h14M5 15.5h9" />
    </Svg>
  )
}

export function IconChevron(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M9 5.5l6.5 6.5L9 18.5" />
    </Svg>
  )
}

export function IconSettings(p: IconProps) {
  return (
    <Svg {...p}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a1.9 1.9 0 1 1-2.7 2.7l-.1-.1a1.6 1.6 0 0 0-2.7 1.1v.2a1.9 1.9 0 1 1-3.8 0v-.1a1.6 1.6 0 0 0-2.8-1.1l-.1.1a1.9 1.9 0 1 1-2.7-2.7l.1-.1A1.6 1.6 0 0 0 3.9 15a1.9 1.9 0 0 1 0-3.8h.2A1.6 1.6 0 0 0 5.2 8.4L5 8.3a1.9 1.9 0 1 1 2.7-2.7l.1.1A1.6 1.6 0 0 0 10.6 4.6V4.4a1.9 1.9 0 1 1 3.8 0v.2a1.6 1.6 0 0 0 2.7 1.1l.1-.1a1.9 1.9 0 1 1 2.7 2.7l-.1.1a1.6 1.6 0 0 0 1.1 2.7h.2a1.9 1.9 0 0 1 0 3.8h-.2" />
    </Svg>
  )
}

export function IconChart(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M4 20h16" />
      <path d="M7 20V11M12 20V5M17 20v-6" />
    </Svg>
  )
}

export function IconArchive(p: IconProps) {
  return (
    <Svg {...p}>
      <rect x="3.5" y="4.5" width="17" height="4" rx="1" />
      <path d="M5 8.5v10a1.5 1.5 0 0 0 1.5 1.5h11a1.5 1.5 0 0 0 1.5-1.5v-10" />
      <path d="M10 12h4" />
    </Svg>
  )
}

export function IconAlert(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M12 4.5l8.5 15h-17z" />
      <path d="M12 10v4M12 16.6v.1" />
    </Svg>
  )
}

export function IconCheck(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M5 12.5l4.5 4.5L19 7" />
    </Svg>
  )
}

export function IconPlus(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M12 5v14M5 12h14" />
    </Svg>
  )
}

export function IconLink(p: IconProps) {
  return (
    <Svg {...p}>
      <path d="M10 13.6a3.6 3.6 0 0 0 5.1 0l2.9-2.9a3.6 3.6 0 1 0-5.1-5.1l-1 1" />
      <path d="M14 10.4a3.6 3.6 0 0 0-5.1 0l-2.9 2.9a3.6 3.6 0 1 0 5.1 5.1l1-1" />
    </Svg>
  )
}

/** IconSidebar 是"目录可收起"那个开关：一个带分栏的方框。 */
export function IconSidebar(p: IconProps) {
  return (
    <Svg {...p}>
      <rect x="3.5" y="4.5" width="17" height="15" rx="2" />
      <path d="M9.5 4.5v15" />
    </Svg>
  )
}

/** IconGrip 是拖拽手柄：两列圆点。 */
export function IconGrip(p: IconProps) {
  return (
    <Svg {...p}>
      <circle cx="9.5" cy="7" r="0.9" />
      <circle cx="9.5" cy="12" r="0.9" />
      <circle cx="9.5" cy="17" r="0.9" />
      <circle cx="14.5" cy="7" r="0.9" />
      <circle cx="14.5" cy="12" r="0.9" />
      <circle cx="14.5" cy="17" r="0.9" />
    </Svg>
  )
}
