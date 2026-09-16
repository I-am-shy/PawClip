// 分类管理。
//
// 只做"增 / 改名 / 改色 / 删"，不做自动归类规则的可视化编辑器——
// §1 P1 里"自动归类规则"是一个独立能力（要一个条件构造器），
// 塞进这个小视图会把它变成第二个界面。规则字段后端已支持（Category.rule），
// 这里**原样保留不覆盖**，否则改个颜色就把用户配的规则抹了。

import { useState } from 'react'
import type { T as TFn } from '../i18n'
import { call, type Category } from '../api'
import { IconPlus, IconTrash } from '../components/Icons'

export type CategoriesProps = {
  t: TFn
  categories: Category[]
  onReload: () => void
  onToast: (msg: string) => void
  onBack: () => void
}

/** 预设色板。不给取色器：它需要一堆代码，而这几个色已经够区分十几个分类。 */
const SWATCHES = ['', '#e5484d', '#f76808', '#ffb224', '#30a46c', '#0091ff', '#8e4ec6', '#8d8d8d']

export function Categories({ t, categories, onReload, onToast, onBack }: CategoriesProps) {
  const [busy, setBusy] = useState(false)
  const [draftName, setDraftName] = useState('')

  const save = async (c: Category) => {
    setBusy(true)
    try {
      const id = await call('SaveCategory', c)
      // 新建时把返回的 id 写回原对象，避免下一次保存又插一条。
      if (c.id === 0 && id > 0) c.id = id
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  const remove = async (c: Category) => {
    // 删除分类**不会**删条目（外键是 SET NULL），但要说清楚，否则用户不敢点。
    if (!window.confirm(`${t('action.delete')}: ${c.name}?`)) return
    setBusy(true)
    try {
      await call('DeleteCategory', c.id)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  const create = async () => {
    const name = draftName.trim()
    if (!name) return
    await save({ id: 0, name, color: '', icon: '', sortOrder: categories.length, itemCount: 0, ttlSeconds: null })
    setDraftName('')
  }

  return (
    <div className="view-body">
      <div className="view-head">
        <button type="button" className="linkbtn" onClick={onBack}>
          ← {t('nav.list')}
        </button>
        <h2>{t('nav.categories')}</h2>
      </div>

      <div className="creator">
        <input
          className="input"
          type="text"
          value={draftName}
          placeholder={t('nav.categories')}
          disabled={busy}
          onChange={(e) => setDraftName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void create()
          }}
        />
        <button type="button" className="btn" disabled={busy || !draftName.trim()} onClick={() => void create()}>
          <IconPlus size={13} /> {t('common.confirm')}
        </button>
      </div>

      <ul className="adminlist">
        {categories.map((c) => (
          <li key={c.id} className="adminrow">
            <div className="swatches">
              {SWATCHES.map((s) => (
                <button
                  key={s || 'none'}
                  type="button"
                  className={`swatch ${c.color === s ? 'swatch-on' : ''}`}
                  style={{ background: s || 'transparent' }}
                  disabled={busy}
                  aria-label={s || t('common.none')}
                  onClick={() => {
                    // 只换颜色，其余字段原样带回去——尤其是 rule。
                    void save({ ...c, color: s })
                  }}
                />
              ))}
            </div>

            <input
              className="input"
              type="text"
              defaultValue={c.name}
              disabled={busy}
              onBlur={(e) => {
                const v = e.target.value.trim()
                if (v && v !== c.name) void save({ ...c, name: v })
              }}
            />

            <span className="admincount">{c.itemCount}</span>

            <button
              type="button"
              className="iconbtn danger"
              title={t('action.delete')}
              aria-label={t('action.delete')}
              disabled={busy}
              onClick={() => void remove(c)}
            >
              <IconTrash size={14} />
            </button>
          </li>
        ))}
        {categories.length === 0 && <li className="adminempty">—</li>}
      </ul>
    </div>
  )
}

function msg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
