// 应用外壳：语言 / 主题 / 设置 / 视图切换 / 原生事件。
//
// 状态都集中在这里（没有状态库，§0.4 的冻结栈）：
// 语言、主题、设置、当前视图、分类与标签的缓存、toast。
// 面板自己的列表状态在 views/Panel.tsx 里，切视图即卸载。
//
// 一个必须说清的取舍：**面板窗口是整个应用唯一那个窗口**。
// Wails v2 是单窗口模型，所以"面板"与"设置/统计/导出"是同一个窗口里的
// 不同视图，不是两个窗口。切到设置页时窗口仍然在屏幕上——这符合预期
// （用户正在操作它），只是热键的"呼出/收起"语义作用在这个窗口上。

import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import {
  call,
  type Category,
  type Health,
  type ImportRow,
  type PanelLifecycleReport,
  type SettingsShape,
  type StatsSummary,
  type Tag,
} from './api'
import { makeT, normalizeLang, type Lang } from './i18n'
import { applyTheme, resolveTheme, useSystemDark, type ThemePref } from './theme'
import { usePolledEvents } from './hooks'
import { Panel } from './views/Panel'
import { Settings } from './views/Settings'
import { Stats } from './views/Stats'
import { Backup } from './views/Backup'
import { Categories } from './views/Categories'
import { Tags } from './views/Tags'
import { IconAlert, IconArchive, IconChart, IconSettings, IconX } from './components/Icons'

type View = 'panel' | 'settings' | 'stats' | 'backup' | 'categories' | 'tags'

export default function App() {
  // ── 基础状态 ──────────────────────────────────────────────────
  const [lang, setLang] = useState<Lang>('en')
  const [themePref, setThemePref] = useState<ThemePref>('system')
  const [view, setView] = useState<View>('panel')
  const [settings, setSettings] = useState<SettingsShape | null>(null)
  const [health, setHealth] = useState<Health | null>(null)
  const [categories, setCategories] = useState<Category[]>([])
  const [tags, setTags] = useState<Tag[]>([])
  const [stats, setStats] = useState<StatsSummary | null>(null)
  const [lifecycle, setLifecycle] = useState<PanelLifecycleReport | null>(null)
  const [trashed, setTrashed] = useState(false)
  const [toast, setToast] = useState('')
  const [bootError, setBootError] = useState('')
  // 启动期警告：**不是**错误。程序照常工作，只是有件事用户该知道
  // （目前唯一一种是"config.toml 读不了，本次改用默认设置"）。
  // 单独一个状态而不是复用 bootError，是因为两者的呈现不该一样：
  // 红条会让人以为程序坏了，去卸载重装；而它其实在正常工作。
  const [bootWarn, setBootWarn] = useState('')
  const [focusSearchNonce, setFocusSearchNonce] = useState(0)
  const [lastImport, setLastImport] = useState<ImportRow | null>(null)
  const [version, setVersion] = useState('')
  const [platform, setPlatform] = useState('')
  const [dataDir, setDataDir] = useState('')
  const [configPath, setConfigPath] = useState('')

  const t = useMemo(() => makeT(lang), [lang])
  const systemDark = useSystemDark()
  const resolved = resolveTheme(themePref, systemDark)

  // 主题写到 <html data-theme>：CSS 变量两套，组件不关心主题。
  useEffect(() => {
    applyTheme(resolved)
  }, [resolved])

  // ── 启动：拉一次语言与设置 ────────────────────────────────────
  useEffect(() => {
    let alive = true
    ;(async () => {
      try {
        // 语言先取：界面文案要在第一次渲染后尽快正确。
        // 后端判定（本机设置 → 系统区域 → en）比前端自己猜可靠，
        // 所以**不**用 navigator.language。
        const l = await call('Lang')
        if (alive) setLang(normalizeLang(l))

        const s = await call('Settings')
        if (!alive) return
        setSettings(s)
        if (s) setThemePref(normalizeTheme(s.ui.theme))

        const [v, p, d, c] = await Promise.all([
          call('Version').catch(() => ''),
          call('Health').catch(() => null),
          call('DataDir').catch(() => ''),
          call('ConfigPath').catch(() => ''),
        ])
        if (!alive) return
        setVersion(v)
        if (p) {
          setHealth(p)
          setPlatform(p.platform)
          // 后端初始化失败时必须显式告诉用户：§13 风险表要求"本次运行不记录
          // 任何内容"这件事不能被静默吞掉——用户会以为程序在正常工作。
          if (p.initError) setBootError(p.initError)
          if (p.bootWarning) setBootWarn(p.bootWarning)
        }
        setDataDir(d)
        setConfigPath(c)

        await refreshSidebar()
      } catch (e: unknown) {
        if (alive) setBootError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      alive = false
    }
    // 只在挂载时跑一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const refreshSidebar = useCallback(async () => {
    const [cs, ts, li] = await Promise.all([
      call('Categories').catch(() => null),
      call('Tags').catch(() => null),
      call('LastImport').catch(() => null),
    ])
    setCategories(cs ?? [])
    setTags(ts ?? [])
    setLastImport(li)
  }, [])

  const refreshStats = useCallback(async () => {
    const [s, l] = await Promise.all([
      call('StatsOverview').catch(() => null),
      call('PanelLifecycle').catch(() => null),
    ])
    setStats(s)
    setLifecycle(l)
  }, [])

  // 进统计页才取统计：这些查询要扫表，没必要在后台一直跑。
  useEffect(() => {
    if (view === 'stats') void refreshStats()
  }, [view, refreshStats])

  // ── 原生事件 ──────────────────────────────────────────────────
  //
  // 只在面板视图轮询：这些事件几乎全部是"切视图/刷新列表"，
  // 而用户待在设置页时不需要它们。省下来的是一份持续的 IPC。
  usePolledEvents(
    view === 'panel',
    useCallback(
      (evs) => {
        // 同一批里可能有多个事件，取最后一个"视图类"的即可
        // （后面的覆盖前面的，与用户连续点击托盘的结果一致）。
        let nextView: View | null = null
        for (const e of evs) {
          switch (e.type) {
            case 'show':
              // 呼出：刷新列表 + 聚焦搜索框。
              setFocusSearchNonce((n) => n + 1)
              break
            case 'settings':
              nextView = 'settings'
              break
            case 'stats':
              nextView = 'stats'
              break
            case 'backup':
              nextView = 'backup'
              break
            case 'about':
              setToast(`${t('about.title')} · PawClip ${version}`)
              break
            case 'captureToggled':
              // 托盘切换了记录开关：把设置重读一遍，否则设置页显示的还是旧值。
              void call('Settings').then((s) => {
                if (s) setSettings(s)
              })
              setToast(t('settings.saved'))
              break
            case 'notice':
              // 后端才知道的一句话（导出/导入报告、连续粘贴还剩几条）。
              // 文案已经由后端按当前语言生成好了，这里直接显示。
              if (e.note) setToast(e.note)
              break
            case 'idleHidden':
              // 空闲收起：不提示。用户此刻不在看它，弹一个 toast 反而奇怪。
              break
            case 'hide':
              break
          }
        }
        if (nextView) setView(nextView)
      },
      [t, version],
    ),
  )

  // ── 顶层键盘 ──────────────────────────────────────────────────
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey
      // Esc 在非面板视图等同于"返回"。
      if (e.key === 'Escape' && view !== 'panel') {
        e.preventDefault()
        setView('panel')
        return
      }
      // ⌘, 打开设置（macOS 的通用约定）。
      if (mod && e.key === ',') {
        e.preventDefault()
        setView(view === 'settings' ? 'panel' : 'settings')
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [view])

  const onToast = useCallback((m: string) => setToast(m), [])
  useEffect(() => {
    if (!toast) return
    const id = window.setTimeout(() => setToast(''), 4000)
    return () => window.clearTimeout(id)
  }, [toast])

  const hidePanel = useCallback(() => {
    void call('HidePanel').catch(() => {})
  }, [])

  // 标题栏拖动：面板是无边框窗口（macOS 上是 borderless NSPanel），
  // Wails 的 CSS app-region 机制只作用于它自己的宿主窗口，对面板无效。
  // 所以在标题栏空白处按下时显式调用 DragPanel，把拖动交给原生循环。
  // 落在按钮/输入框上的按下仍然留给控件自己，不能抢。
  const startDrag = useCallback((e: React.MouseEvent) => {
    if (e.button !== 0) return
    const el = e.target as HTMLElement | null
    if (el?.closest('button, input, select, textarea, a')) return
    void call('DragPanel').catch(() => {})
  }, [])

  const modLabel = platform === 'darwin' ? '⌘' : 'Ctrl+'

  return (
    <div className="app">
      <header className="titlebar" onMouseDown={startDrag}>
        <div className="brand">
          <span className="brand-dot" />
          <span className="brand-name">{t('app.name')}</span>
          <span className="brand-tag">{t('app.tagline')}</span>
        </div>
        <nav className="nav">
          <NavBtn view="panel" cur={view} set={setView} icon={<IconArchive size={14} />} label={t('nav.list')} />
          <NavBtn view="stats" cur={view} set={setView} icon={<IconChart size={14} />} label={t('nav.stats')} />
          <NavBtn view="backup" cur={view} set={setView} icon={<IconArchive size={14} />} label={t('nav.backup')} />
          <NavBtn view="settings" cur={view} set={setView} icon={<IconSettings size={14} />} label={t('nav.settings')} />
          {/* 关闭 = 收起面板（热键再按一次也能收起）。 */}
          <button
            type="button"
            className="iconbtn titlebtn-close"
            aria-label={t('common.close')}
            title={t('common.close')}
            onClick={hidePanel}
          >
            <IconX size={13} />
          </button>
        </nav>
      </header>

      {/* 后端初始化失败：一条**常驻**的横幅。
          不做成会自动消失的 toast——它描述的是一个持续存在的状态。 */}
      {bootWarn && (
        <div className="bootwarn" role="status">
          <IconAlert size={15} />
          <div>
            <strong>{t('warn.boot')}</strong>
            <div>{bootWarn}</div>
          </div>
        </div>
      )}

      {bootError && (
        <div className="booterror">
          <IconAlert size={15} />
          <div>
            <strong>{t('err.init')}</strong>
            <div className="mono">{bootError}</div>
          </div>
        </div>
      )}

      <main className="main">
        {view === 'panel' && (
          <Panel
            t={t}
            settings={settings}
            categories={categories}
            tags={tags}
            modLabel={modLabel}
            trashed={trashed}
            onTrashed={setTrashed}
            onHide={hidePanel}
            onOpenView={(v) => setView(v)}
            onToast={onToast}
            focusSearchNonce={focusSearchNonce}
          />
        )}

        {view === 'settings' && (
          <Settings
            t={t}
            settings={settings}
            dataDir={dataDir}
            configPath={configPath}
            version={version}
            platform={platform}
            onReload={() => {
              void call('Settings').then((s) => {
                if (s) {
                  setSettings(s)
                  setThemePref(normalizeTheme(s.ui.theme))
                }
              })
            }}
            onSetTheme={(v) => setThemePref(normalizeTheme(v))}
            onSetLang={(v) => setLang(normalizeLang(v))}
            onToast={onToast}
          />
        )}

        {view === 'stats' && (
          <Stats
            t={t}
            stats={stats}
            lifecycle={lifecycle}
            onReload={() => void refreshStats()}
            onToast={onToast}
            onBack={() => setView('panel')}
          />
        )}

        {view === 'backup' && (
          <Backup
            t={t}
            lastImport={lastImport}
            onReload={() => void refreshSidebar()}
            onToast={onToast}
            onBack={() => setView('panel')}
          />
        )}

        {view === 'categories' && (
          <Categories
            t={t}
            categories={categories}
            onReload={() => void refreshSidebar()}
            onToast={onToast}
            onBack={() => setView('panel')}
          />
        )}

        {view === 'tags' && (
          <Tags
            t={t}
            tags={tags}
            onReload={() => void refreshSidebar()}
            onToast={onToast}
            onBack={() => setView('panel')}
          />
        )}
      </main>

      {toast && (
        <div className="toast" role="status">
          <span>{toast}</span>
          <button type="button" className="iconbtn" aria-label={t('common.close')} onClick={() => setToast('')}>
            <IconX size={12} />
          </button>
        </div>
      )}

      {/* health 只用于开发期排查：贴在右下角，不干扰正式使用。 */}
      {import.meta.env.DEV && health && (
        <div className="devbadge mono">
          fts={String(health.ftsAvailable)} alive={health.aliveItems} schema=v{health.schemaVersion}
        </div>
      )}
    </div>
  )
}

function NavBtn({
  view,
  cur,
  set,
  icon,
  label,
}: {
  view: View
  cur: View
  set: (v: View) => void
  icon: ReactNode
  label: string
}) {
  return (
    <button
      type="button"
      className={`navbtn ${cur === view ? 'navbtn-on' : ''}`}
      onClick={() => set(view)}
      aria-current={cur === view ? 'page' : undefined}
    >
      {icon}
      <span>{label}</span>
    </button>
  )
}

/** normalizeTheme 把未知值落到 system。 */
function normalizeTheme(v: unknown): ThemePref {
  return v === 'light' || v === 'dark' ? v : 'system'
}
