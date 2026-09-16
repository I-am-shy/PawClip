// TTL 徽标：显示一条内容的过期状态。
//
// 存在的理由是"用户需要能一眼看出哪些东西快没了"。§5 的三级 TTL 里
// `ttl_source` 区分了"全局默认算出来的"与"用户显式设为永不"，
// 两者都表现为 `expires_at` 为空或非空，光看时间分不出来——
// 所以永不主动过期的条目要给一个明确的正向标识，而不是什么都不显示
// （不显示会让用户以为"这条没被纳入保留策略"）。

import type { T as TFn } from '../i18n'
import { formatDateTime } from '../format'

/**
 * TTL_NEVER 与 store/mutate.go 的 TTLSourceNever 必须一致。
 *
 * 这个字符串跨越了 Go 与 TS 两层，没法用类型系统约束，所以只能在这里
 * 集中定义一次、并且**只在组件之外的常量里出现**——组件里再写字面量
 * 就失去了"改一处即可"的意义。
 */
const TTL_NEVER = 'never'

export type TTLBadgeProps = {
  t: TFn
  expiresAt: number | null
  /** 后端给的 ttl_source：'' | 'global' | 'item' | 'never' 等 */
  ttlSource: string
  /** 当前时间（Unix 秒）。由调用方传，便于把"同一屏用同一个 now"。 */
  now: number
}

export function TTLBadge({ t, expiresAt, ttlSource, now }: TTLBadgeProps) {
  // 显式的"永不"：正向标注。这是用户主动做的决定，值得显示出来。
  //
  // 只看 ttlSource 而不看 expiresAt：库里理论上可能出现
  // "ttl_source=never 且 expires_at 非空" 的组合（手改库、或旧版本迁移），
  // 那种情况下 never 是更强的意图，也该显示成永不。
  if (ttlSource === TTL_NEVER) {
    return <span className="badge badge-neutral">{t('item.noExpiry')}</span>
  }

  if (!expiresAt || expiresAt <= 0) {
    return null
  }

  const remain = expiresAt - now
  // 剩余不足一成（相对总时长）时加重：默认 30 天的条目剩 2 天就该显眼。
  // 这里拿不到"总时长"，所以用绝对档位近似——24 小时内 = 紧急。
  const urgent = remain <= 24 * 3600
  const warn = !urgent && remain <= 3 * 24 * 3600
  const cls = urgent ? 'badge-danger' : warn ? 'badge-warn' : 'badge-neutral'

  return (
    <span className={`badge ${cls}`} title={formatDateTime(expiresAt)}>
      {t('item.expiresAt', { t: shortRemain(remain) })}
    </span>
  )
}

/**
 * shortRemain 把剩余秒数压成一个短标签。
 *
 * 特意不做 i18n：它是"3d" / "5h" 这种国际通用的短记号，翻成
 * "3 天" 反而会把徽标撑宽，挤掉预览文本。
 */
function shortRemain(sec: number): string {
  if (sec <= 0) return '0'
  if (sec < 3600) return `${Math.max(1, Math.floor(sec / 60))}m`
  if (sec < 86400) return `${Math.floor(sec / 3600)}h`
  return `${Math.floor(sec / 86400)}d`
}
