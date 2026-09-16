// 统计视图（§11 P1「存储统计面板（条数 / 占用 / Top 来源应用）」）。
//
// 这里还承担一件**验收性质**的事：把实测内存与 WebKit 子进程数显示出来。
// DESIGN §14 第 10 条要求"面板销毁后实测 RSS"——当前 Wails 版本做不到
// 真销毁（见 PanelLifecycle.Note），所以这个面板必须**如实**显示
// "不支持销毁"以及当前实际占用，而不是给一个好看的假数字。
// 用户能在这里看到真相，比我们报告里写"已完成"重要得多。

import { useState } from 'react'
import type { T as TFn } from '../i18n'
import { call, type GCReport, type PanelLifecycleReport, type StatsSummary } from '../api'
import { formatBytes, formatDuration, kindLabel } from '../format'
import { useInterval } from '../hooks'

export type StatsProps = {
  t: TFn
  stats: StatsSummary | null
  lifecycle: PanelLifecycleReport | null
  onReload: () => void
  onToast: (msg: string) => void
  onBack: () => void
}

export function Stats({ t, stats, lifecycle, onReload, onToast, onBack }: StatsProps) {
  const [gcBusy, setGcBusy] = useState(false)
  const [lastGC, setLastGC] = useState<GCReport | null>(null)
  const [live, setLive] = useState<PanelLifecycleReport | null>(lifecycle)

  // 内存每 3 秒刷新一次。
  // 为什么需要定时刷：用户想看的是"面板收起之后到底降下来没有"，
  // 静态数字没法回答那个问题——他收起面板、再点进来，如果数字不重算，
  // 看到的还是旧的。
  useInterval(() => {
    void call('PanelLifecycle')
      .then(setLive)
      .catch(() => {})
  }, 3000)

  const runGC = async () => {
    setGcBusy(true)
    try {
      const rep = await call('RunGC')
      setLastGC(rep)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    } finally {
      setGcBusy(false)
    }
  }

  const gc = lastGC ?? stats?.lastGC ?? null
  const unknown = t('stats.unknown')

  return (
    <div className="view-body">
      <div className="view-head">
        <button type="button" className="linkbtn" onClick={onBack}>
          ← {t('nav.list')}
        </button>
        <h2>{t('stats.title')}</h2>
      </div>

      {!stats ? (
        <div className="dim">{t('list.loading')}</div>
      ) : (
        <>
          <section className="statgrid">
            <Card k={t('stats.alive')} v={String(stats.alive)} />
            <Card k={t('stats.trashed')} v={String(stats.trashed)} />
            <Card k={t('stats.all')} v={String(stats.all)} />
            <Card k={t('stats.events')} v={String(stats.events)} />
            <Card k={t('stats.aliveBytes')} v={formatBytes(stats.aliveBytes)} />
            <Card
              k={t('stats.diskBytes')}
              v={`${formatBytes(stats.diskBytes)} (${stats.diskFiles})`}
            />
          </section>

          <section className="statblock">
            <h3>{t('stats.kinds')}</h3>
            <BarList
              rows={(stats.kinds ?? []).map((k) => ({
                label: kindLabel(t, k.kind),
                value: k.count,
                extra: formatBytes(k.bytes),
              }))}
              empty={t('common.none')}
            />
          </section>

          <section className="statblock">
            <h3>{t('stats.topApps')}</h3>
            <BarList
              rows={(stats.topApps ?? []).map((a) => ({
                label: a.appName || a.appId || unknown,
                value: a.count,
                extra: '',
              }))}
              empty={t('common.none')}
            />
          </section>

          <section className="statblock">
            <h3>{t('stats.memory')}</h3>
            <ul className="kvlist">
              <li className="kv">
                <span className="kv-k">{t('stats.rss')}</span>
                <span className="kv-v">
                  {live && live.rssBytes > 0 ? formatBytes(live.rssBytes) : unknown}
                </span>
              </li>
              <li className="kv">
                <span className="kv-k">{t('stats.webContent')}</span>
                <span className="kv-v">{live ? String(live.webContentProcs) : unknown}</span>
              </li>
              <li className="kv">
                <span className="kv-k">{t('settings.idleDestroy')}</span>
                <span className="kv-v">
                  {live
                    ? live.destroySupported
                      ? t('stats.destroyYes')
                      : t('stats.destroyNo')
                    : unknown}
                </span>
              </li>
              {live && !live.destroySupported && (
                <li className="note">{live.note}</li>
              )}
            </ul>
          </section>

          <section className="statblock">
            <h3>{t('stats.gc')}</h3>
            <div className="formrow">
              <button type="button" className="btn" disabled={gcBusy || stats.paused} onClick={() => void runGC()}>
                {gcBusy ? t('stats.gcRunning') : t('stats.gcRunNow')}
              </button>
              {stats.paused && <span className="dim">{t('stats.paused')}</span>}
              <span className="dim">
                {t('stats.gcRuns')}: {stats.gcRuns}
              </span>
            </div>
            {gc && (
              <ul className="kvlist">
                <li className="kv">
                  <span className="kv-k">{t('stats.lastGC')}</span>
                  <span className="kv-v">{formatDuration(gc.tookMs)}</span>
                </li>
                <li className="kv">
                  <span className="kv-k">{t('settings.onExpire.trash')}</span>
                  <span className="kv-v">
                    {gc.trashed} / {gc.deleted} / {gc.archived}
                  </span>
                </li>
                <li className="kv">
                  <span className="kv-k">{t('action.purge')}</span>
                  <span className="kv-v">
                    {gc.purged} + {gc.evicted} ({formatBytes(gc.evictedBytes)})
                  </span>
                </li>
                <li className="kv">
                  <span className="kv-k">{t('stats.diskFiles')}</span>
                  <span className="kv-v">
                    {gc.orphansRemoved} ({formatBytes(gc.orphanBytes)})
                  </span>
                </li>
                <li className="kv">
                  <span className="kv-k">summary</span>
                  <span className="kv-v">{gc.summary}</span>
                </li>
                {gc.errors && gc.errors.length > 0 && (
                  <li className="errbox">
                    {t('common.errors')}: {gc.errors.join(' / ')}
                  </li>
                )}
              </ul>
            )}
          </section>
        </>
      )}
    </div>
  )
}

function Card({ k, v }: { k: string; v: string }) {
  return (
    <div className="statcard">
      <div className="statcard-v">{v}</div>
      <div className="statcard-k">{k}</div>
    </div>
  )
}

/** BarList 是一个极简的横向条形列表。 */
function BarList({
  rows,
  empty,
}: {
  rows: Array<{ label: string; value: number; extra: string }>
  empty: string
}) {
  if (rows.length === 0) return <div className="dim">{empty}</div>
  const max = Math.max(...rows.map((r) => r.value), 1)
  return (
    <ul className="barlist">
      {rows.map((r) => (
        <li key={r.label} className="barrow">
          <span className="barlabel" title={r.label}>
            {r.label}
          </span>
          <span className="bartrack">
            {/* 宽度按最大值归一。用 inline style 而不是 CSS 变量：
                值来自数据，没有静态类能表达。 */}
            <span className="barfill" style={{ width: `${Math.max(2, (r.value / max) * 100)}%` }} />
          </span>
          <span className="barvalue">
            {r.value}
            {r.extra ? ` · ${r.extra}` : ''}
          </span>
        </li>
      ))}
    </ul>
  )
}
