// 纯展示用的小工具：字节、时间、类型名。
//
// 单独一个文件而不是散在各组件里：单位换算与"多久以前"的阈值判断
// 一旦不一致，界面同屏就会显示两种口径（这一条写"1.2 MB"、那一条写"1234 KB"）。

import type { T } from './i18n'

// ── 字节 ────────────────────────────────────────────────────────

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB']

/**
 * formatBytes 把字节数变成人能读的形式。
 *
 * 用 1024 进制（与 Finder / 资源管理器一致）。**不**用 1000 进制：
 * 用户拿这个数字跟系统显示的占用对比，两边不一致就会被当成 bug。
 *
 * 0 与负数返回 "0 B"（不返回 "0"）：界面上"0"看起来像是漏了单位。
 * 负数只可能来自后端算差值时的边界情况，显示成 0 比显示成 "-1 B" 好。
 */
export function formatBytes(n: number | null | undefined): string {
  if (n == null || !isFinite(n) || n <= 0) return '0 B'
  let v = n
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  // B 与 KB 不给小数（"1.0 B" 很怪），MB 以上给一位。
  const digits = i <= 1 ? 0 : 1
  return `${v.toFixed(digits)} ${UNITS[i]}`
}

// ── 时间 ────────────────────────────────────────────────────────

const MIN = 60
const HOUR = 3600
const DAY = 86400

/**
 * formatRelative 把 Unix 秒变成"多久以前"。
 *
 * 分档粗糙是刻意的：剪贴板历史的用户只关心"刚才 / 今天 / 前几天"，
 * 精确到分钟既没用又更容易算错。
 *
 * **文案来自词典**（t），不写死：写死的话英文界面会冒出"3 分钟前"。
 * now 参数默认取当前时间，但**保留可注入**——测试要断言固定输入，
 * 而 `Date.now()` 在测试里是不确定的。
 */
export function formatRelative(
  t: T,
  unixSec: number | null | undefined,
  now = Date.now() / 1000,
): string {
  if (!unixSec || unixSec <= 0) return ''
  const d = now - unixSec
  // 未来时间（时钟回拨、或库里存了脏值）按"刚刚"处理：
  // 显示"3 分钟后"会让用户以为程序坏了。
  if (d < 0 || d < MIN) return t('time.justNow')
  if (d < HOUR) return t('time.minutesAgo', { n: Math.floor(d / MIN) })
  if (d < DAY) return t('time.hoursAgo', { n: Math.floor(d / HOUR) })
  if (d < DAY * 30) return t('time.daysAgo', { n: Math.floor(d / DAY) })
  return formatDate(unixSec)
}

/** formatDate 给出绝对日期（超过一个月时用）。 */
export function formatDate(unixSec: number | null | undefined): string {
  if (!unixSec || unixSec <= 0) return ''
  const d = new Date(unixSec * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

/** formatDateTime 给出到分钟的绝对时间（用于过期时间、报告）。 */
export function formatDateTime(unixSec: number | null | undefined): string {
  if (!unixSec || unixSec <= 0) return ''
  const d = new Date(unixSec * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${formatDate(unixSec)} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** formatDuration 把毫秒变成 "12 ms" / "1.2 s"。 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

// ── 类型 ────────────────────────────────────────────────────────

/**
 * kindLabel 把 items.kind 映射成界面文案。
 *
 * 认不出来的 kind 原样返回：后端将来加了新类型时，
 * 显示一个生名字好过显示空白（空白会让人以为这条数据坏了）。
 */
export function kindLabel(t: T, kind: string): string {
  const k = `kind.${kind}`
  const s = t(k)
  return s === k ? kind : s
}

/** kindGroup 把 kind 归到用于筛选的三类。 */
export function kindGroup(kind: string): 'text' | 'image' | 'files' | 'other' {
  switch (kind) {
    case 'text':
    case 'html':
    case 'rtf':
      return 'text'
    case 'image':
      return 'image'
    case 'files':
      return 'files'
    default:
      return 'other'
  }
}
