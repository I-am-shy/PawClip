// 应用外壳：语言 / 主题 / 设置 / 视图切换 / 原生事件。
//
// 状态都集中在这里（没有状态库，§0.4 的冻结栈）：
// 语言、主题、设置、当前视图、toast。
// 面板自己的列表状态在 views/Panel.tsx 里，切视图即卸载。
//
// 一个必须说清的取舍：**面板窗口是整个应用唯一那个窗口**。
// Wails v2 是单窗口模型，所以"面板"与"设置/统计/导出"是同一个窗口里的
// 不同视图，不是两个窗口。切到设置页时窗口仍然在屏幕上——这符合预期
// （用户正在操作它），只是热键的"呼出/收起"语义作用在这个窗口上。

import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import {
  call,
  type Health,
  type ImportRow,
  type PanelLifecycleReport,
  type SettingsShape,
  type StatsSummary,
} from './api'
import { makeT, normalizeLang, type Lang } from './i18n'
import { formatCombo } from './hotkey'
import { applyTheme, resolveTheme, useSystemDark, type ThemePref } from './theme'
import { usePolledEvents } from './hooks'
import { Panel } from './views/Panel'
import { Settings } from './views/Settings'
import { Stats } from './views/Stats'
import { Backup } from './views/Backup'
import { IconAlert, IconArchive, IconChart, IconSettings, IconX } from './components/Icons'

type View = 'panel' | 'settings' | 'stats' | 'backup'

export default function App() {
  // ── 基础状态 ──────────────────────────────────────────────────
  const [lang, setLang] = useState<Lang>('en')
  const [themePref, setThemePref] = useState<ThemePref>('system')
  const [view, setView] = useState<View>('panel')
  const [settings, setSettings] = useState<SettingsShape | null>(null)
  const [health, setHealth] = useState<Health | null>(null)
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
  // showNonce 是"面板被呼出的次数"。
  //
  // 它同时驱动两件事：把焦点给搜索框（"呼出即可打字"），以及把面板重置回
  // 默认状态（回到历史页、清掉搜索/筛选/预览）。两件事都是"每次呼出都要做"
  // 的，所以共用一个计数器，而不是各来一个。
  const [showNonce, setShowNonce] = useState(0)
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

        await refreshLastImport()
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

  // 只服务"导出/导入"页上那条"上次导入"的提示。
  // 分类/标签管理已下线（见 docs/DESIGN.md §10），这里不再需要拉那两个列表。
  const refreshLastImport = useCallback(async () => {
    setLastImport(await call('LastImport').catch(() => null))
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
  // ⚠️ **常开，不按视图开关。**
  //
  // 原来这里是 `view === 'panel'`，理由是"设置页/统计页用不到这些事件，省一份
  // 持续的 IPC"。但恰恰有一类事件**只有非面板视图才需要**：面板被重新呼出
  // （show）时要把视图切回剪贴板历史。用户上次是在设置页收起面板的，这次按
  // 热键呼出时前端正停在设置页——按旧条件根本不轮询，那条"切回历史页"的
  // 命令就永远收不到，界面就停在上次的页面上。
  //
  // 代价是设置页/统计页上每 400ms 多一次 TakeEvents（取走一个通常是空的切片），
  // 换来的是"每次亮起来都是历史页"这个不依赖用户上次停在哪里的保证。
  usePolledEvents(
    true,
    useCallback(
      (evs) => {
        // 同一批里可能有多个事件，取最后一个"视图类"的即可
        // （后面的覆盖前面的，与用户连续点击托盘的结果一致）。
        let nextView: View | null = null
        for (const e of evs) {
          switch (e.type) {
            case 'show':
              // 呼出：**回到剪贴板历史**，并清掉回收站这个临时态。
              //
              // 不记"上次停在哪个视图"是有意的：按热键呼出时用户想要的是
              // "看看我刚复制的东西"，而不是"接着上次的设置页看"。回收站同理
              // ——它是为了找一条删掉的东西而临时进去的，不该是下次的默认。
              //
              // note 也要弹：启动期"热键被别的应用占用"就是搭在这条事件上的
              // （后端只知道面板要显示，前端才知道该说一句话）。原来这里
              // 只取视图信号、把 note 丢掉，于是用户永远不知道热键是死的。
              if (e.note) setToast(e.note)
              setTrashed(false)
              nextView = 'panel'
              setShowNonce((n) => n + 1)
              break
            case 'settings':
              // 同上：带 note 的"设置页"事件是后端在替这次跳转说一句话。
              if (e.note) setToast(e.note)
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

  // 呼出热键的显示形式（⌘⇧V / Ctrl+Shift+V），空串表示没设热键。
  //
  // 它是**算出来的**，不是存下来的：唯一真源是 settings.ui.hotkey，
  // 设置页改完 → onReload 回读设置 → 这里跟着变。之前界面上的
  // "（⌘⇧V 呼出）"是写在词条里的常量，改设置它也不会动。
  const hotkeyLabel = formatCombo(settings?.ui.hotkey ?? '', platform)

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
            modLabel={modLabel}
            hotkeyLabel={hotkeyLabel}
            trashed={trashed}
            onTrashed={setTrashed}
            onHide={hidePanel}
            onOpenView={(v) => setView(v)}
            onToast={onToast}
            showNonce={showNonce}
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
            onReload={() => void refreshLastImport()}
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
