// 左侧的分类 / 标签树。
//
// 它是一个**筛选项**列表，不是一个可编辑的树：编辑在「分类」「标签」两个
// 视图里做。分开的理由是面板要能一键呼出、一秒内可用，把增删改混进来
// 会让这个侧栏的动作语义变得不可预期（点一下到底是筛选还是重命名？）。

import type { T as TFn } from '../i18n'
import type { Category, Tag } from '../api'

export type CategoryTreeProps = {
  t: TFn
  categories: Category[]
  tags: Tag[]
  /** null = 不限分类 */
  selectedCategoryId: number | null
  /** true 时表示"只看未分类"（与 selectedCategoryId == null 是两回事） */
  uncategorized: boolean
  selectedTagId: number | null
  onSelectCategory: (id: number | null, uncategorized: boolean) => void
  onSelectTag: (id: number | null) => void
  onManageCategories: () => void
  onManageTags: () => void
}

export function CategoryTree(p: CategoryTreeProps) {
  const { t, categories, tags, selectedCategoryId, uncategorized, selectedTagId } = p

  return (
    <nav className="tree" aria-label={t('nav.list')}>
      <div className="tree-section">
        <button
          type="button"
          className={`treeitem ${selectedCategoryId == null && !uncategorized ? 'treeitem-on' : ''}`}
          onClick={() => p.onSelectCategory(null, false)}
        >
          <span className="treedot" style={{ background: 'transparent' }} />
          {t('list.all')}
        </button>
        <button
          type="button"
          className={`treeitem ${uncategorized ? 'treeitem-on' : ''}`}
          onClick={() => p.onSelectCategory(null, true)}
        >
          <span className="treedot" style={{ background: 'transparent' }} />
          {t('list.uncategorized')}
        </button>
      </div>

      <div className="tree-section">
        <div className="tree-head">
          <span>{t('nav.categories')}</span>
          <button type="button" className="linkbtn" onClick={p.onManageCategories}>
            {t('action.more')}
          </button>
        </div>
        {categories.length === 0 && <div className="tree-empty">—</div>}
        {categories.map((c) => (
          <button
            key={c.id}
            type="button"
            className={`treeitem ${selectedCategoryId === c.id ? 'treeitem-on' : ''}`}
            onClick={() => p.onSelectCategory(c.id, false)}
            title={c.name}
          >
            <span className="treedot" style={{ background: c.color || 'var(--muted)' }} />
            <span className="treename">{c.name}</span>
            {/* 计数为 0 时不显示 0：一屏十几个 "0" 只会造成噪音，
                而"没有"这件事本身已经由名字后面的留白表达了。 */}
            {c.itemCount > 0 && <span className="treecount">{c.itemCount}</span>}
          </button>
        ))}
      </div>

      {tags.length > 0 && (
        <div className="tree-section">
          <div className="tree-head">
            <span>{t('nav.tags')}</span>
            <button type="button" className="linkbtn" onClick={p.onManageTags}>
              {t('action.more')}
            </button>
          </div>
          {tags.map((g) => (
            <button
              key={g.id}
              type="button"
              className={`treeitem ${selectedTagId === g.id ? 'treeitem-on' : ''}`}
              onClick={() => p.onSelectTag(selectedTagId === g.id ? null : g.id)}
              title={g.name}
            >
              <span className="treedot" style={{ background: g.color || 'var(--muted)' }} />
              <span className="treename">{g.name}</span>
              {g.itemCount > 0 && <span className="treecount">{g.itemCount}</span>}
            </button>
          ))}
        </div>
      )}
    </nav>
  )
}
