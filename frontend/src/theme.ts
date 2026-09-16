// 主题解析：把 ui.theme（system / light / dark）落到 <html data-theme> 上。
//
// 为什么用 data-theme 属性 + CSS 变量，而不是给每个组件传主题：
//   - 组件不用关心主题，样式表一处定义两套变量；
//   - 不引入任何状态库（§14 第 12 条）。
//
// "跟随系统"用 matchMedia('(prefers-color-scheme: dark)')，并且**监听它的
// 变化**：用户在用 PawClip 的过程中切换系统外观是常见操作（日落时开夜览），
// 不监听的话界面会一直停在旧主题直到下次启动。

import { useEffect, useState } from 'react'

export type ThemePref = 'system' | 'light' | 'dark'
export type Resolved = 'light' | 'dark'

const MQ = '(prefers-color-scheme: dark)'

export function resolveTheme(pref: ThemePref, systemDark: boolean): Resolved {
  if (pref === 'dark') return 'dark'
  if (pref === 'light') return 'light'
  return systemDark ? 'dark' : 'light'
}

/** useSystemDark 跟随系统的明暗偏好。 */
export function useSystemDark(): boolean {
  const [dark, setDark] = useState(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return false
    return window.matchMedia(MQ).matches
  })

  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return
    const mq = window.matchMedia(MQ)
    const onChange = () => setDark(mq.matches)
    // addEventListener 在旧 WebKit 上可能是 addListener；两者都试一次。
    if (mq.addEventListener) {
      mq.addEventListener('change', onChange)
      return () => mq.removeEventListener('change', onChange)
    }
    mq.addListener(onChange)
    return () => mq.removeListener(onChange)
  }, [])

  return dark
}

/** applyTheme 把解析结果写到 <html data-theme>。 */
export function applyTheme(resolved: Resolved): void {
  const el = document.documentElement
  if (el.getAttribute('data-theme') !== resolved) {
    el.setAttribute('data-theme', resolved)
  }
}
