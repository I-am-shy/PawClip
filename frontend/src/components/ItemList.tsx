// 历史列表。
//
// 设计上的两条约束落在实现里：
//
//  1. **不做虚拟滚动。** 一页 60 条，DOM 最多也就 60 行 × 几个节点。
//     引入虚拟列表要多几百行代码和一套测量逻辑，换来的收益在这一页的
//     体量下看不出来（§14 第 12 条要求不引额外的库也是这个取向）。
//  2. **图片用缩略图，且用 `<img loading="lazy">`。** 面板刚亮起来时如果
//     一次性请求 60 张大图，首屏会明显卡顿；缩略图本来就小，加上 lazy
//     之后只有视口内的才发请求。

import type { MouseEvent as ReactMouseEvent } from 'react'
import type { T as TFn } from '../i18n'
import type { ListRow } from '../api'
import { blobSrc } from '../api'
import { formatBytes, formatRelative, kindLabel } from '../format'
import { TTLBadge } from './TTLBadge'
import { IconFile, IconImage, IconPin, IconText } from './Icons'

export type ItemListProps = {
  t: TFn
  rows: ListRow[]
  /** 当前键盘光标所在的行（不是"选中"）。 */
  activeId: number | null
  /** 多选集合（批量操作的目标）。 */
  selectedIds: Set<number>
  /** ⌘/Ctrl + 1..N 直贴的条数上界；前 N 行会显示数字标记。 */
  quickPasteCount: number
  /**
   * 显示直贴提示时要用的修饰键记号。
   *
   * 由调用方按平台传入（macOS 用 '⌘'、其余用 'Ctrl+'）。
   * **不在组件里判断平台**：那需要读 navigator.platform 或后端字段，
   * 而这里只是一个文案细节，让知道平台的人传进来最省事。
   */
  modLabel: string
  now: number
  trashed: boolean
  loading: boolean
  hasMore: boolean
  onActivate: (id: number) => void
  onSelectionChange: (ids: Set<number>) => void
  /**
   * 在某一项上按下了右键。
   *
   * 坐标是**视口坐标**（clientX/Y）：菜单用 fixed 定位画在页面里，
   * 而面板本身可以被拖到屏幕任何位置，用文档坐标会飘。
   */
  onContextMenu: (id: number, x: number, y: number) => void
  onPaste: (id: number) => void
  onTogglePin: (id: number, pinned: boolean) => void
  onDelete: (id: number) => void
  onRestore: (id: number) => void
  onPurge: (id: number) => void
  onLoadMore: () => void
}

export function ItemList(p: ItemListProps) {
  const { t, rows, activeId, selectedIds, quickPasteCount, modLabel, now, trashed, loading, hasMore } = p

  if (rows.length === 0) {
    return (
      <div className="list-empty">
        {loading ? t('list.loading') : trashed ? t('list.emptyTrashed') : t('list.empty')}
      </div>
    )
  }

  const handleClick = (e: ReactMouseEvent, id: number) => {
    // ⌘/Ctrl 点击 = 切换多选；Shift 点击暂时不做范围选择
    // （跨页的范围选择在 keyset 分页下没有稳定的"序号"可用，
    //  做出来会有"选中的和我看到的不一致"这种难解释的行为）。
    if (e.metaKey || e.ctrlKey) {
      const next = new Set(selectedIds)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      p.onSelectionChange(next)
      return
    }
    p.onSelectionChange(new Set([id]))
    p.onActivate(id)
  }

  return (
    <ul className="itemlist" role="listbox" aria-label={trashed ? t('list.trash') : t('nav.list')}>
      {rows.map((r, i) => {
        const isActive = r.id === activeId
        const isSelected = selectedIds.has(r.id)
        const quickKey = i < quickPasteCount ? i + 1 : 0
        return (
          <li
            key={r.id}
            className={[
              'item',
              isActive ? 'item-active' : '',
              isSelected ? 'item-selected' : '',
              r.pinned ? 'item-pinned' : '',
            ]
              .filter(Boolean)
              .join(' ')}
            role="option"
            aria-selected={isSelected}
            data-id={r.id}
            onClick={(e) => handleClick(e, r.id)}
            onDoubleClick={() => p.onPaste(r.id)}
            onContextMenu={(e) => {
              e.preventDefault()
              // 右键把光标落到这一行：菜单里的动作作用在它身上，光标停在别处
              // 会让"我点的那条"和"要操作的那条"看起来不是一回事。
              // 多选在这里被收成单选，同样是为了这个一致性。
              p.onSelectionChange(new Set([r.id]))
              p.onActivate(r.id)
              p.onContextMenu(r.id, e.clientX, e.clientY)
            }}
          >
            <div className="item-leading">
              <KindIcon t={t} kind={r.kind} thumbUrl={r.thumbUrl} />
            </div>

            <div className="item-body">
              <div className="item-preview">{r.preview || '—'}</div>
              <div className="item-meta">
                <span className="item-app" title={r.sourceAppId || r.sourceAppName}>
                  {r.sourceAppName || r.sourceAppId || '—'}
                </span>
                <span className="dot">·</span>
                <span>{formatRelative(t, r.createdAt, now)}</span>
                {r.fileCount > 0 && (
                  <>
                    <span className="dot">·</span>
                    <span>{t('item.files', { n: r.fileCount })}</span>
                  </>
                )}
                {r.kind !== 'image' && r.textLen > 0 && (
                  <>
                    <span className="dot">·</span>
                    <span>{t('item.chars', { n: r.textLen })}</span>
                  </>
                )}
                {r.byteSize > 0 && (
                  <>
                    <span className="dot">·</span>
                    <span>{formatBytes(r.byteSize)}</span>
                  </>
                )}
                {r.useCount > 1 && (
                  <>
                    <span className="dot">·</span>
                    <span>{t('item.usedTimes', { n: r.useCount })}</span>
                  </>
                )}
              </div>
            </div>

            <div className="item-trailing">
              {r.pinned && (
                <span className="badge badge-pin" title={t('item.pinned')}>
                  <IconPin size={11} />
                </span>
              )}
              <TTLBadge t={t} expiresAt={r.expiresAt} ttlSource={r.ttlSource ?? ''} now={now} />
              {quickKey > 0 && !trashed && (
                <span className="quickkey" title={t('action.paste')}>
                  {modLabel}
                  {quickKey}
                </span>
              )}
            </div>

            <div className="item-actions">
              {trashed ? (
                <>
                  <button
                    type="button"
                    className="iconbtn"
                    title={t('action.restore')}
                    aria-label={t('action.restore')}
                    onClick={(e) => {
                      e.stopPropagation()
                      p.onRestore(r.id)
                    }}
                  >
                    ⟲
                  </button>
                  <button
                    type="button"
                    className="iconbtn danger"
                    title={t('action.purge')}
                    aria-label={t('action.purge')}
                    onClick={(e) => {
                      e.stopPropagation()
                      p.onPurge(r.id)
                    }}
                  >
                    ×
                  </button>
                </>
              ) : (
                <>
                  <button
                    type="button"
                    className="iconbtn"
                    title={r.pinned ? t('action.unpin') : t('action.pin')}
                    aria-label={r.pinned ? t('action.unpin') : t('action.pin')}
                    onClick={(e) => {
                      e.stopPropagation()
                      p.onTogglePin(r.id, !r.pinned)
                    }}
                  >
                    <IconPin size={13} />
                  </button>
                  <button
                    type="button"
                    className="iconbtn danger"
                    title={t('action.delete')}
                    aria-label={t('action.delete')}
                    onClick={(e) => {
                      e.stopPropagation()
                      p.onDelete(r.id)
                    }}
                  >
                    ×
                  </button>
                </>
              )}
            </div>
          </li>
        )
      })}

      {hasMore && (
        <li className="list-more">
          <button type="button" className="btn btn-quiet" onClick={p.onLoadMore} disabled={loading}>
            {loading ? t('list.loading') : t('list.loadMore')}
          </button>
        </li>
      )}
      {!hasMore && rows.length > 0 && <li className="list-end">{t('list.end')}</li>}
    </ul>
  )
}

/** KindIcon 按类型给出图标；图片类型优先显示真实缩略图。 */
function KindIcon({ t, kind, thumbUrl }: { t: TFn; kind: string; thumbUrl: string }) {
  // 有缩略图就一定要用它：剪贴板历史里"图片"这一类的可辨识度完全
  // 来自画面本身，靠一个通用图标用户得逐条点开看。
  if (thumbUrl) {
    return (
      <img
        className="thumb"
        src={blobSrc(thumbUrl)}
        alt={kindLabel(t, kind)}
        loading="lazy"
        decoding="async"
        draggable={false}
      />
    )
  }
  switch (kind) {
    case 'image':
      return <span className="kindicon"><IconImage size={16} /></span>
    case 'files':
      return <span className="kindicon"><IconFile size={16} /></span>
    default:
      return <span className="kindicon"><IconText size={16} /></span>
  }
}
