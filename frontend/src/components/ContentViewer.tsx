// 查看浮层：把一条内容**临时**放大看清楚（只读）。
//
// 与右侧预览栏（Preview.tsx）的分工：
//   · 那个是"光标停在这条时顺便看看"，跟着上下键变化，占列表旁边一格；
//   · 这里是"我要专门看这一条"——盖住整个面板、只读、能缩放，关掉即消失。
//
// 为什么需要它：剪贴板里的东西经常"在列表那一行里根本看不出是什么"——
// 一屏代码只看得到第一行、截图看不清细节。列表行的高度是为扫读设计的，
// 不是为看清设计的。
//
// 只读的含义很具体：这里没有任何修改内容的入口。能做的动作只有"看"和
// "复制"（复制走后端 CopyOnly，与列表里的复制是同一条路径）。
//
// 缩放的几条交互约定（按图片查看器的通行做法）：
//   · 滚轮**以指针为中心**缩放，不是以窗口中心：放大的总是你指着的那个地方，
//     否则每滚一下都要把目标重新拖回中央。
//   · 大图打开时自动"适应窗口"，先给全貌；小图保持 1:1，不无故放大。
//   · 拖动平移用 pointer capture：指针移出窗口再回来，拖动不会断。
//   · 双击复位——忘记自己缩到哪了的时候，这是最快回到正常的方式。

import { useCallback, useEffect, useRef, useState } from 'react'
import type { T as TFn } from '../i18n'
import { blobSrc, call, type ItemDetail } from '../api'
import { formatBytes, formatDateTime, formatRelative, kindLabel } from '../format'
import { TTLBadge } from './TTLBadge'
import { IconCopy, IconX } from './Icons'

export type ContentViewerProps = {
  t: TFn
  id: number | null
  now: number
  onClose: () => void
  onCopy: (id: number) => void
  onRevealFile: (path: string) => void
}

/** 图片缩放的上下界：0.1× 看得全一张巨图，8× 看得清像素。 */
const MIN_SCALE = 0.1
const MAX_SCALE = 8
/** 文本字号的上下界（倍率，基准字号见 .viewer-text）。 */
const MIN_FONT = 0.7
const MAX_FONT = 3

/**
 * SWALLOWED 是浮层里要吞掉的单键。
 *
 * 这些键在面板里都另有用途（空格开预览栏、↑↓ 移光标、回车粘贴、Backspace
 * 删除），浮层开着时按它们不该顺手把面板的动作也做了。
 * 带 ⌘/Ctrl 的组合**不吞**：⌘C 复制选中的文本是这里最需要的操作。
 */
const SWALLOWED = new Set([
  ' ',
  'Enter',
  'ArrowUp',
  'ArrowDown',
  'ArrowLeft',
  'ArrowRight',
  'Backspace',
  'Delete',
])

export function ContentViewer({ t, id, now, onClose, onCopy, onRevealFile }: ContentViewerProps) {
  const [detail, setDetail] = useState<ItemDetail | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  // 图片视图：倍数 + 平移量。
  const [scale, setScale] = useState(1)
  const [offset, setOffset] = useState({ x: 0, y: 0 })
  // 文本视图：字号倍数。
  const [font, setFont] = useState(1)

  const stageRef = useRef<HTMLDivElement>(null)
  const imgRef = useRef<HTMLImageElement>(null)
  // 缩放/平移的"当前值"镜像。缩放是"按当前值算下一个值"，而事件处理器里的
  // state 是上一次渲染的旧值（连续滚轮时尤其明显：每一下都从同一个基准算）。
  const scaleRef = useRef(1)
  const fontRef = useRef(1)
  const dragRef = useRef<{ x: number; y: number; ox: number; oy: number } | null>(null)
  const fittedRef = useRef(false)

  useEffect(() => {
    if (id == null) {
      setDetail(null)
      setError('')
      return
    }
    let alive = true
    setLoading(true)
    setError('')
    // 全文走 Get(id)：列表给的 preview 是截断过的，而"查看"要的正是全文。
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

  // 换条目就把缩放复位：留着上一条的比例，用户看到的是一个莫名其妙的倍数，
  // 而他并不知道这个数字是从哪来的。
  useEffect(() => {
    scaleRef.current = 1
    fontRef.current = 1
    fittedRef.current = false
    setScale(1)
    setOffset({ x: 0, y: 0 })
    setFont(1)
  }, [id])

  // Esc 关闭 + 屏蔽面板的单键。
  // 用**捕获**阶段：面板与 App 的键盘监听都在 window 的冒泡阶段，
  // 这里先一步 stopPropagation，它们就收不到这次按键了。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose()
        return
      }
      if (e.metaKey || e.ctrlKey) return
      if (SWALLOWED.has(e.key)) {
        e.preventDefault()
        e.stopPropagation()
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])

  const isImage = !!detail?.imageUrl
  const isFiles = !isImage && (detail?.filePaths?.length ?? 0) > 0
  const stage = isImage ? 'image' : isFiles ? 'files' : 'text'

  /** applyScale 缩放，并让锚点处的内容停在原地（锚点相对内容区中心）。 */
  const applyScale = useCallback((next: number, anchorX = 0, anchorY = 0) => {
    const prev = scaleRef.current
    const target = clamp(next, MIN_SCALE, MAX_SCALE)
    if (target === prev) return
    const ratio = target / prev
    scaleRef.current = target
    setScale(target)
    setOffset((o) => ({ x: anchorX + (o.x - anchorX) * ratio, y: anchorY + (o.y - anchorY) * ratio }))
  }, [])

  const resetView = useCallback(() => {
    scaleRef.current = 1
    setScale(1)
    setOffset({ x: 0, y: 0 })
  }, [])

  /** fitToWindow 按容器尺寸把整张图缩到看得全；小图不放大。 */
  const fitToWindow = useCallback(() => {
    const el = stageRef.current
    const img = imgRef.current
    if (!el || !img || !img.naturalWidth || !img.naturalHeight) return
    const r = el.getBoundingClientRect()
    const pad = 24
    const s = Math.min((r.width - pad) / img.naturalWidth, (r.height - pad) / img.naturalHeight)
    const target = clamp(Math.min(s, 1), MIN_SCALE, MAX_SCALE)
    scaleRef.current = target
    setScale(target)
    setOffset({ x: 0, y: 0 })
  }, [])

  const zoomFont = useCallback((factor: number) => {
    const target = clamp(fontRef.current * factor, MIN_FONT, MAX_FONT)
    fontRef.current = target
    setFont(target)
  }, [])

  const resetFont = useCallback(() => {
    fontRef.current = 1
    setFont(1)
  }, [])

  // 滚轮：图片缩到指针处；文本要配合 ⌘/Ctrl（否则滚轮是滚动，这是文本的常识）。
  //
  // ⚠️ 用原生 addEventListener 而不是 onWheel：React 把 wheel 注册成
  // passive 的，在处理函数里 preventDefault 不生效（还会在控制台警告），
  // 于是"缩图的同时页面/列表也跟着滚"。
  useEffect(() => {
    const el = stageRef.current
    if (!el || id == null) return
    const onWheel = (e: WheelEvent) => {
      if (stage === 'image') {
        e.preventDefault()
        const r = el.getBoundingClientRect()
        applyScale(
          scaleRef.current * Math.exp(-e.deltaY / 320),
          e.clientX - r.left - r.width / 2,
          e.clientY - r.top - r.height / 2,
        )
        return
      }
      if (stage === 'text' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        zoomFont(Math.exp(-e.deltaY / 320))
      }
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [stage, id, applyScale, zoomFont])

  // 图片的拖动平移。
  const onPointerDown = (e: React.PointerEvent) => {
    if (stage !== 'image' || e.button !== 0) return
    ;(e.currentTarget as Element).setPointerCapture(e.pointerId)
    dragRef.current = { x: e.clientX, y: e.clientY, ox: offset.x, oy: offset.y }
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const d = dragRef.current
    if (!d) return
    setOffset({ x: d.ox + (e.clientX - d.x), y: d.oy + (e.clientY - d.y) })
  }
  const onPointerUp = (e: React.PointerEvent) => {
    dragRef.current = null
    ;(e.currentTarget as Element).releasePointerCapture?.(e.pointerId)
  }

  if (id == null) return null

  return (
    <div className="viewer" role="dialog" aria-modal="true" aria-label={t('viewer.title')}>
      <header className="viewer-head">
        <span className="viewer-title">
          {detail ? kindLabel(t, detail.kind) : t('viewer.title')}
          {detail && (
            <span className="viewer-dims">
              {/* 副标题按类型给：图片给像素、文件给个数、文本给字数。
                  "文件 / 0 字"这种组合没有任何信息量。 */}
              {isImage
                ? `${detail.imageWidth}×${detail.imageHeight}`
                : isFiles
                  ? t('item.files', { n: detail.filePaths?.length ?? 0 })
                  : t('item.chars', { n: detail.textLen })}
            </span>
          )}
        </span>

        <div className="viewer-actions">
          {stage === 'image' && (
            <ZoomBar
              t={t}
              label={`${Math.round(scale * 100)}%`}
              onOut={() => applyScale(scaleRef.current / 1.25)}
              onIn={() => applyScale(scaleRef.current * 1.25)}
              onReset={resetView}
              onFit={fitToWindow}
            />
          )}
          {stage === 'text' && (
            <ZoomBar
              t={t}
              label={`${Math.round(font * 100)}%`}
              onOut={() => zoomFont(1 / 1.25)}
              onIn={() => zoomFont(1.25)}
              onReset={resetFont}
            />
          )}
          <button type="button" className="btn" onClick={() => onCopy(id)} title={t('action.copy')}>
            <IconCopy size={13} />
            {t('action.copy')}
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

      <div
        ref={stageRef}
        className={`viewer-body viewer-body-${stage}`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onDoubleClick={stage === 'image' ? resetView : undefined}
      >
        {loading && <div className="dim">{t('list.loading')}</div>}
        {error && <div className="errbox">{t('err.generic', { err: error })}</div>}

        {detail && !loading && isImage && (
          <img
            ref={imgRef}
            className="viewer-img"
            src={blobSrc(detail.imageUrl)}
            alt={detail.preview || t('viewer.title')}
            draggable={false}
            style={{ transform: `translate(${offset.x}px, ${offset.y}px) scale(${scale})` }}
            onLoad={() => {
              // 只自动适应一次：用户自己缩过之后再触发的 load（换图/重排）
              // 不该把他调好的视图重置掉。
              if (fittedRef.current) return
              fittedRef.current = true
              fitToWindow()
            }}
          />
        )}

        {detail && !loading && isFiles && (
          <ul className="viewer-files">
            {(detail.filePaths ?? []).map((f) => (
              <li key={f}>
                <button type="button" className="filelink" onClick={() => onRevealFile(f)} title={f}>
                  {f}
                </button>
              </li>
            ))}
          </ul>
        )}

        {detail && !loading && stage === 'text' && (
          <pre className="viewer-text" style={{ fontSize: `${13 * font}px` }} tabIndex={0}>
            {detail.text || t('search.filterEmpty')}
          </pre>
        )}
      </div>

      <footer className="viewer-foot">
        {detail && (
          <div className="viewer-meta">
            <span>{formatRelative(t, detail.createdAt, now)}</span>
            <span className="dot">·</span>
            <span>{formatBytes(detail.byteSize)}</span>
            {detail.sourceAppName && (
              <>
                <span className="dot">·</span>
                <span>{detail.sourceAppName}</span>
              </>
            )}
            <TTLBadge t={t} expiresAt={detail.expiresAt} ttlSource={detail.ttlSource ?? ''} now={now} />
            {detail.expiresAt ? <span className="dim">{formatDateTime(detail.expiresAt)}</span> : null}
          </div>
        )}
        <span className="viewer-hint">
          {stage === 'image' ? t('viewer.hintImage') : t('viewer.hintText')}
        </span>
      </footer>
    </div>
  )
}

/** ZoomBar 是顶栏那一排缩放控件（图片与文本共用，只是刻度含义不同）。 */
function ZoomBar({
  t,
  label,
  onOut,
  onIn,
  onReset,
  onFit,
}: {
  t: TFn
  label: string
  onOut: () => void
  onIn: () => void
  onReset: () => void
  onFit?: () => void
}) {
  return (
    <div className="zoombar">
      <button type="button" className="iconbtn" title={t('viewer.zoomOut')} aria-label={t('viewer.zoomOut')} onClick={onOut}>
        −
      </button>
      <button type="button" className="zoomval" title={t('viewer.zoomReset')} onClick={onReset}>
        {label}
      </button>
      <button type="button" className="iconbtn" title={t('viewer.zoomIn')} aria-label={t('viewer.zoomIn')} onClick={onIn}>
        ＋
      </button>
      {onFit && (
        <button type="button" className="btn btn-quiet" onClick={onFit}>
          {t('viewer.fit')}
        </button>
      )}
    </div>
  )
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v
}
