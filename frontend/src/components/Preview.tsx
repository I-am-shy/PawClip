// 预览面板（docs/DESIGN.md §11 P1「图片缩略图 + 空格快速预览」）。
//
// 只在需要时向后端要全文：列表给的是 preview（截断过的），
// 全文走 Get(id)。§14 第 7 条明确要求列表查询**不取 text_content**，
// 因为一页 50 条、每条几 MB 的文本会把 IPC 撑爆。
//
// 全屏图片用 original（imageUrl）而不是缩略图：预览的唯一目的就是看清它。

import { useEffect, useState } from 'react'
import type { T as TFn } from '../i18n'
import { blobSrc, call, type ItemDetail, type TransformResult } from '../api'
import { formatBytes, formatDateTime, formatRelative, kindLabel } from '../format'
import { TTLBadge } from './TTLBadge'
import { IconCopy, IconX } from './Icons'

// PLAIN_OP 是"去格式贴纯文本"在选择器里的取值。
//
// 它**不是**一个真实的转换 op（后端注册表里没有它）：它不是"对文本做变换"，
// 而是"只取这条的纯文本表示"（丢掉 HTML / RTF）。放在同一个下拉里是因为
// 用户心里的分类是"我想换个样子用这条内容"，不是"这两件事的实现不同"。
const PLAIN_OP = '__plain'

export type PreviewProps = {
  t: TFn
  id: number | null
  now: number
  /** 回写方式（来自设置 ui.pasteMode）；由 Panel 决定，Preview 只管执行。 */
  autoPaste: boolean
  onClose: () => void
  onCopy: (id: number) => void
  onRevealFile: (path: string) => void
  onToast: (msg: string) => void
}

export function Preview({ t, id, now, autoPaste, onClose, onCopy, onRevealFile, onToast }: PreviewProps) {
  const [detail, setDetail] = useState<ItemDetail | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (id == null) {
      setDetail(null)
      setError('')
      return
    }
    let alive = true
    setLoading(true)
    setError('')
    call('Get', id)
      .then((d) => {
        if (alive) setDetail(d)
      })
      .catch((e: unknown) => {
        if (!alive) return
        setDetail(null)
        setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [id])

  if (id == null) return null

  return (
    <aside className="preview" aria-label={t('action.preview')}>
      <header className="preview-head">
        <span className="preview-title">
          {detail ? kindLabel(t, detail.kind) : t('action.preview')}
        </span>
        <div className="preview-head-actions">
          <button
            type="button"
            className="iconbtn"
            title={t('action.copy')}
            aria-label={t('action.copy')}
            onClick={() => onCopy(id)}
          >
            <IconCopy size={14} />
          </button>
          <button
            type="button"
            className="iconbtn"
            title={t('common.close')}
            aria-label={t('common.close')}
            onClick={onClose}
          >
            <IconX size={14} />
          </button>
        </div>
      </header>

      <div className="preview-body">
        {loading && <div className="dim">{t('list.loading')}</div>}
        {error && <div className="errbox">{t('err.generic', { err: error })}</div>}
        {detail && !loading && <PreviewBody t={t} item={detail} onRevealFile={onRevealFile} />}
      </div>

      {detail && !loading && (
        <Converter t={t} item={detail} autoPaste={autoPaste} onToast={onToast} />
      )}

      {detail && !loading && (
        <footer className="preview-foot">
          <div className="preview-meta">
            <span>{formatRelative(t, detail.createdAt, now)}</span>
            <span className="dot">·</span>
            <span>{formatBytes(detail.byteSize)}</span>
            {detail.sourceAppName && (
              <>
                <span className="dot">·</span>
                <span>{detail.sourceAppName}</span>
              </>
            )}
            {detail.imageWidth > 0 && (
              <>
                <span className="dot">·</span>
                <span>
                  {detail.imageWidth}×{detail.imageHeight}
                </span>
              </>
            )}
            {detail.lastUsedAt ? (
              <>
                <span className="dot">·</span>
                <span>{t('item.usedTimes', { n: detail.useCount })}</span>
              </>
            ) : null}
          </div>
          <div className="preview-meta">
            <TTLBadge t={t} expiresAt={detail.expiresAt} ttlSource={detail.ttlSource ?? ''} now={now} />
            {detail.expiresAt ? <span className="dim">{formatDateTime(detail.expiresAt)}</span> : null}
          </div>
        </footer>
      )}
    </aside>
  )
}

/**
 * Converter 是内容转换器的入口（docs/DESIGN.md §11 P2）。
 *
 * 三个决定：
 *
 *  1. **结果就地显示**，而不是"转换完直接贴出去"。用户点"JSON 美化"想先
 *     看一眼对不对——直接贴出去等于替他做了决定，而美化失败时更是灾难。
 *     所以流程是"选 → 转换 → 看结果 → 再决定复制/粘贴"。
 *  2. **失败不抛异常**：后端把失败当成一次正常返回（带 errorKind），
 *     前端按 kind 翻文案。粘了一段不是 JSON 的东西是最常见的情况，
 *     不该看到一个红色的技术报错。
 *  3. **结果里带空格的原文直接用 pre 显示**，不截断：用户要看的就是它。
 */
function Converter({
  t,
  item,
  autoPaste,
  onToast,
}: {
  t: TFn
  item: ItemDetail
  autoPaste: boolean
  onToast: (m: string) => void
}) {
  const [ops, setOps] = useState<string[]>([])
  const [op, setOp] = useState(PLAIN_OP)
  const [result, setResult] = useState<TransformResult | null>(null)
  const [busy, setBusy] = useState(false)

  // op 列表来自后端（唯一真源）：后端加了新转换，这里自动出现。
  // 拉不到（旧后端 / 测试环境）时只剩"去格式贴纯文本"，不强求。
  useEffect(() => {
    let alive = true
    call('TransformOpList')
      .then((v) => {
        if (alive && v) setOps(v)
      })
      .catch(() => {
        /* 拿不到就当只有 plain，不打扰用户 */
      })
    return () => {
      alive = false
    }
  }, [])

  // 换条目 / 换 op 都要把上一次的结果清掉：留着它会让用户以为
  // "这条转换出来就是这个"，而实际上显示的是上一条的结果。
  useEffect(() => {
    setResult(null)
  }, [item.id, op])

  if (!item.text) return null

  const apply = async () => {
    setBusy(true)
    try {
      const r = await call('TransformItem', item.id, op)
      setResult(r)
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    } finally {
      setBusy(false)
    }
  }

  const pastePlain = async () => {
    setBusy(true)
    try {
      const r = await call('PastePlain', item.id, autoPaste)
      if (r.note) onToast(r.note)
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    } finally {
      setBusy(false)
    }
  }

  const sendResult = async (asPaste: boolean) => {
    if (!result || result.errorKind) return
    setBusy(true)
    try {
      const r = await call('PasteTransformed', item.id, result.op, asPaste && autoPaste)
      if (r.note) onToast(r.note)
    } catch (e: unknown) {
      onToast(t('err.generic', { err: e instanceof Error ? e.message : String(e) }))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="convbar" aria-label={t('conv.title')}>
      <div className="convbar-row">
        <select
          className="conv-select"
          value={op}
          disabled={busy}
          onChange={(e) => setOp(e.target.value)}
          title={t('conv.title')}
        >
          <option value={PLAIN_OP}>{t('conv.plainText')}</option>
          {ops.map((id) => (
            <option key={id} value={id}>
              {opLabel(t, id)}
            </option>
          ))}
        </select>
        {op === PLAIN_OP ? (
          <button type="button" className="btn" disabled={busy} onClick={() => void pastePlain()}>
            {t('conv.pastePlain')}
          </button>
        ) : (
          <button type="button" className="btn" disabled={busy} onClick={() => void apply()}>
            {t('conv.apply')}
          </button>
        )}
      </div>

      {result?.errorKind && (
        <div className="conv-err">{convErrText(t, result)}</div>
      )}

      {result && !result.errorKind && (
        <div className="conv-result">
          <pre className="conv-text" tabIndex={0}>
            {result.text}
          </pre>
          <div className="conv-actions">
            <button type="button" className="btn" disabled={busy} onClick={() => void sendResult(false)}>
              {t('conv.copyResult')}
            </button>
            <button type="button" className="btn" disabled={busy} onClick={() => void sendResult(true)}>
              {t('conv.pasteResult')}
            </button>
          </div>
        </div>
      )}
    </section>
  )
}

/** opLabel 把后端的 op ID 翻成用户语言；不认识的 ID 原样显示。 */
function opLabel(t: TFn, id: string): string {
  const key = `conv.${id}`
  const s = t(key)
  // makeT 缺键时会回退成键名本身，这里据此判断"没有文案"。
  return s === key ? id : s
}

/** convErrText 把后端的失败种类翻成文案；不认识就显示后端原文。 */
function convErrText(t: TFn, r: TransformResult): string {
  const key = `conv.err.${r.errorKind}`
  const s = t(key)
  if (s !== key) return s
  return r.errorText || t('conv.err.failed')
}

function PreviewBody({
  t,
  item,
  onRevealFile,
}: {
  t: TFn
  item: ItemDetail
  onRevealFile: (p: string) => void
}) {
  // 图片优先（有 imageUrl 就一定是图片类）
  if (item.imageUrl) {
    return (
      <div className="preview-image">
        <img src={blobSrc(item.imageUrl)} alt={item.preview} draggable={false} />
      </div>
    )
  }

  if (item.filePaths && item.filePaths.length > 0) {
    return (
      <ul className="preview-files">
        {item.filePaths.map((f) => (
          <li key={f}>
            <button type="button" className="filelink" onClick={() => onRevealFile(f)} title={f}>
              {f}
            </button>
          </li>
        ))}
      </ul>
    )
  }

  if (item.text) {
    return (
      <pre className="preview-text" tabIndex={0}>
        {item.text}
      </pre>
    )
  }

  return <div className="dim">{t('search.filterEmpty')}</div>
}
