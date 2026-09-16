// 标签管理。
//
// 与分类的关键差别：标签是**多对多**，一条内容可以同时属于多个标签，
// 所以这里没有"未打标签的东西去哪了"这类问题——删掉一个标签只是
// 把它的关联去掉，条目本身不受影响。

import { useState } from 'react'
import type { T as TFn } from '../i18n'
import { call, type Tag } from '../api'
import { IconPlus, IconTrash } from '../components/Icons'

export type TagsProps = {
  t: TFn
  tags: Tag[]
  onReload: () => void
  onToast: (msg: string) => void
  onBack: () => void
}

const SWATCHES = ['', '#e5484d', '#f76808', '#ffb224', '#30a46c', '#0091ff', '#8e4ec6', '#8d8d8d']

export function Tags({ t, tags, onReload, onToast, onBack }: TagsProps) {
  const [busy, setBusy] = useState(false)
  const [draft, setDraft] = useState('')

  const create = async () => {
    const name = draft.trim()
    if (!name) return
    setBusy(true)
    try {
      await call('SaveTag', 0, name, '')
      setDraft('')
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  const save = async (g: Tag, name: string, color: string) => {
    setBusy(true)
    try {
      await call('SaveTag', g.id, name, color)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  const remove = async (g: Tag) => {
    if (!window.confirm(`${t('action.delete')}: ${g.name}?`)) return
    setBusy(true)
    try {
      await call('DeleteTag', g.id)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="view-body">
      <div className="view-head">
        <button type="button" className="linkbtn" onClick={onBack}>
          ← {t('nav.list')}
        </button>
        <h2>{t('nav.tags')}</h2>
      </div>

      <div className="creator">
        <input
          className="input"
          type="text"
          value={draft}
          placeholder={t('nav.tags')}
          disabled={busy}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void create()
          }}
        />
        <button type="button" className="btn" disabled={busy || !draft.trim()} onClick={() => void create()}>
          <IconPlus size={13} /> {t('common.confirm')}
        </button>
      </div>

      <ul className="adminlist">
        {tags.map((g) => (
          <li key={g.id} className="adminrow">
            <div className="swatches">
              {SWATCHES.map((s) => (
                <button
                  key={s || 'none'}
                  type="button"
                  className={`swatch ${g.color === s ? 'swatch-on' : ''}`}
                  style={{ background: s || 'transparent' }}
                  disabled={busy}
                  aria-label={s || t('common.none')}
                  onClick={() => void save(g, g.name, s)}
                />
              ))}
            </div>

            <input
              className="input"
              type="text"
              defaultValue={g.name}
              disabled={busy}
              onBlur={(e) => {
                const v = e.target.value.trim()
                if (v && v !== g.name) void save(g, v, g.color)
              }}
            />

            <span className="admincount">{g.itemCount}</span>

            <button
              type="button"
              className="iconbtn danger"
              title={t('action.delete')}
              aria-label={t('action.delete')}
              disabled={busy}
              onClick={() => void remove(g)}
            >
              <IconTrash size={14} />
            </button>
          </li>
        ))}
        {tags.length === 0 && <li className="adminempty">—</li>}
      </ul>
    </div>
  )
}

function msg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
