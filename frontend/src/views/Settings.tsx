// 设置视图。
//
// 三件事决定了这里的结构：
//
//  1. **写设置是逐键的**（SetSetting(key, json)），不是"提交整份表单"。
//     所以每个控件自己负责一次写入 + 一次回读，而不是攒一个 dirty 状态。
//     好处是改动立刻生效（后端 SetSetting 会热重载过滤器），
//     不存在"改了没保存"这种状态。
//  2. **开机自启的开关初值读系统事实**（IsAutoStart），不读设置。
//     §9 的注释说得很清楚：检测逻辑一旦误判就会删掉用户自己建的自启项，
//     所以开关必须显示"现在到底会不会自启"。
//  3. **没有控件的键要显式列出来**（settings.uncovered）。假装"全部可配"
//     比少几个控件更糟：用户会以为某个键不存在。

import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import type { T as TFn } from '../i18n'
import { comboFromEvent, formatCombo, hasAnyModifier, hasBindableKey, heldModifiers, isClearKey } from '../hotkey'
import { call, type SettingsShape } from '../api'

export type SettingsProps = {
  t: TFn
  settings: SettingsShape | null
  onReload: () => void
  onSetTheme: (v: string) => void
  onSetLang: (v: string) => void
  onToast: (msg: string) => void
  dataDir: string
  configPath: string
  version: string
  platform: string
}

/** 尚未做控件、但后端已支持的设置键。列出来比假装没有好。 */
const UNCOVERED = ['storage.walCheckpointEvery', 'backup.manifestFormat', 'backup.includeExpired']

export function Settings(p: SettingsProps) {
  const { t, settings } = p
  const [autoStart, setAutoStart] = useState<boolean | null>(null)
  const [autoStartNote, setAutoStartNote] = useState('')
  const [busy, setBusy] = useState(false)

  // 自启状态读系统事实。
  useEffect(() => {
    let alive = true
    call('IsAutoStart')
      .then((v) => {
        if (alive) setAutoStart(v)
      })
      .catch(() => {
        if (alive) setAutoStart(null)
      })
    // 诊断串顺手取一次：不支持自启的平台（或写注册表失败）会在这里说明原因，
    // 比界面上一句"不支持"有用得多。
    const app = window.go?.main?.App as { AutoStartDiag?: () => Promise<string> } | undefined
    Promise.resolve(app?.AutoStartDiag?.())
      .then((d) => {
        if (alive && d) setAutoStartNote(d)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [])

  /** set 写一个设置键。value 由调用方给 JSON 字面量（字符串要带引号）。 */
  const set = useCallback(
    async (key: string, value: unknown) => {
      setBusy(true)
      try {
        await call('SetSetting', key, JSON.stringify(value))
        p.onReload()
      } catch (e: unknown) {
        p.onToast(t('settings.saveFailed', { err: e instanceof Error ? e.message : String(e) }))
      } finally {
        setBusy(false)
      }
    },
    [p, t],
  )

  if (!settings) {
    return <div className="view-body dim">{t('list.loading')}</div>
  }

  const c = settings.capture
  const x = settings.exclude
  const r = settings.retention
  const u = settings.ui

  const toggleAutoStart = async () => {
    const next = !(autoStart ?? false)
    setBusy(true)
    try {
      await call('SetAutoStart', next)
      setAutoStart(next)
      p.onToast(t('settings.saved'))
    } catch (e: unknown) {
      // 如实报错：自启失败的原因（无权限、路径不可写）用户需要知道。
      p.onToast(t('settings.saveFailed', { err: e instanceof Error ? e.message : String(e) }))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="view-body settings">
      {/* ── 界面 ───────────────────────────────────────────── */}
      <Section title={t('nav.settings')}>
        <Row label={t('settings.lang')}>
          <Select
            value={u.language}
            disabled={busy}
            onChange={(v) => {
              // 语言要**先写库再让 App 重读**：顺序反了会闪一下旧语言。
              void (async () => {
                await set('ui.language', v)
                p.onSetLang(v)
              })()
            }}
            options={[
              { value: 'system', label: t('settings.lang.system') },
              { value: 'zh-CN', label: '简体中文' },
              { value: 'en', label: 'English' },
            ]}
          />
        </Row>

        <Row label={t('settings.theme')}>
          <Select
            value={u.theme}
            disabled={busy}
            onChange={(v) => {
              void (async () => {
                await set('ui.theme', v)
                p.onSetTheme(v)
              })()
            }}
            options={[
              { value: 'system', label: t('settings.theme.system') },
              { value: 'light', label: t('settings.theme.light') },
              { value: 'dark', label: t('settings.theme.dark') },
            ]}
          />
        </Row>

        {/* 热键是"读键盘"的控件，不是文本框：见下面 HotkeyInput 的注释。 */}
        <Row label={t('settings.hotkey')} note={t('settings.hotkey.warn')} wide>
          <HotkeyInput
            t={t}
            platform={p.platform}
            value={u.hotkey}
            disabled={busy}
            onCommit={(v) => void set('ui.hotkey', v)}
          />
        </Row>

        <Row label={t('settings.pasteMode')}>
          <Select
            value={u.pasteMode}
            disabled={busy}
            onChange={(v) => void set('ui.pasteMode', v)}
            options={[
              { value: 'clipboard', label: t('settings.pasteMode.clipboard') },
              { value: 'autoPaste', label: t('settings.pasteMode.autoPaste') },
            ]}
          />
        </Row>

        <Row label={t('settings.restoreClipboard')}>
          <Toggle value={u.restoreClipboard} disabled={busy} onChange={(v) => void set('ui.restoreClipboard', v)} />
        </Row>

        <Row label={t('settings.restoreDelayMs')}>
          <NumberInput value={u.restoreDelayMs} disabled={busy} onCommit={(v) => void set('ui.restoreDelayMs', v)} />
        </Row>

        <Row label={t('settings.quickPaste')}>
          <NumberInput value={u.quickPasteCount} disabled={busy} onCommit={(v) => void set('ui.quickPasteCount', v)} />
        </Row>

        <Row label={t('settings.closeOnBlur')} note={t('settings.closeOnBlur.note')} wide>
          <Toggle value={u.closeOnBlur} disabled={busy} onChange={(v) => void set('ui.closeOnBlur', v)} />
        </Row>

        <Row label={t('settings.idleDestroy')} note={t('settings.idleDestroy.note')} wide>
          <NumberInput
            value={u.windowIdleDestroySec}
            disabled={busy}
            onCommit={(v) => void set('ui.windowIdleDestroySec', v)}
          />
        </Row>

        <Row label={t('settings.autostart')} note={autoStartNote || t('settings.autostart.note')} wide>
          <Toggle value={autoStart ?? false} disabled={busy || autoStart == null} onChange={() => void toggleAutoStart()} />
        </Row>
      </Section>

      {/* ── 捕获 ───────────────────────────────────────────── */}
      <Section title={t('settings.captureEnabled')}>
        <Row label={t('settings.captureEnabled')}>
          <Toggle value={c.enabled} disabled={busy} onChange={(v) => void set('capture.enabled', v)} />
        </Row>

        <Row label={t('settings.captureTypes')}>
          <MultiCheck
            values={c.types}
            disabled={busy}
            options={[
              { value: 'text', label: t('kind.text') },
              { value: 'image', label: t('kind.image') },
              { value: 'files', label: t('kind.files') },
            ]}
            onChange={(v) => void set('capture.types', v)}
          />
        </Row>

        <Row label={t('settings.textMaxChars')}>
          <NumberInput value={c.textMaxChars} disabled={busy} onCommit={(v) => void set('capture.textMaxChars', v)} />
        </Row>

        <Row label={t('settings.imageMaxBytes')}>
          <NumberInput value={c.imageMaxBytes} disabled={busy} onCommit={(v) => void set('capture.imageMaxBytes', v)} />
        </Row>

        <Row label={t('settings.debounceMs')}>
          <NumberInput value={c.debounceMs} disabled={busy} onCommit={(v) => void set('capture.debounceMs', v)} />
        </Row>

        <Row label={t('settings.pollActiveMs')}>
          <NumberInput
            value={c.pollIntervalActiveMs}
            disabled={busy}
            onCommit={(v) => void set('capture.pollIntervalActiveMs', v)}
          />
        </Row>

        <Row label={t('settings.pollIdleMs')}>
          <NumberInput
            value={c.pollIntervalIdleMs}
            disabled={busy}
            onCommit={(v) => void set('capture.pollIntervalIdleMs', v)}
          />
        </Row>

        <Row label={t('settings.idleThresholdSec')}>
          <NumberInput
            value={c.idleThresholdSec}
            disabled={busy}
            onCommit={(v) => void set('capture.idleThresholdSec', v)}
          />
        </Row>

        <Row label={t('settings.excludeApps')} note={t('settings.excludeApps.note')} wide>
          <TextArea
            value={x.apps.join('\n')}
            disabled={busy}
            rows={4}
            onCommit={(v) =>
              void set(
                'exclude.apps',
                v
                  .split('\n')
                  .map((s) => s.trim())
                  .filter(Boolean),
              )
            }
          />
        </Row>

        <Row label={t('settings.excludePrivateTypes')}>
          <Toggle value={x.privateTypes} disabled={busy} onChange={(v) => void set('exclude.privateTypes', v)} />
        </Row>
      </Section>

      {/* ── 保留 ───────────────────────────────────────────── */}
      <Section title={t('settings.retentionTitle')}>
        <Row label={t('settings.defaultTtlSec')}>
          <NumberInput value={r.defaultTtlSec} disabled={busy} onCommit={(v) => void set('retention.defaultTtlSec', v)} />
        </Row>

        <Row label={t('settings.onExpire')}>
          <Select
            value={r.onExpire}
            disabled={busy}
            onChange={(v) => void set('retention.onExpire', v)}
            options={[
              { value: 'trash', label: t('settings.onExpire.trash') },
              { value: 'delete', label: t('settings.onExpire.delete') },
              { value: 'archive', label: t('settings.onExpire.archive') },
            ]}
          />
        </Row>

        <Row label={t('settings.trashTtlSec')}>
          <NumberInput value={r.trashTtlSec} disabled={busy} onCommit={(v) => void set('retention.trashTtlSec', v)} />
        </Row>

        <Row label={t('settings.maxItems')}>
          <NumberInput value={r.maxItems} disabled={busy} onCommit={(v) => void set('retention.maxItems', v)} />
        </Row>

        <Row label={t('settings.maxDiskBytes')}>
          <NumberInput value={r.maxDiskBytes} disabled={busy} onCommit={(v) => void set('retention.maxDiskBytes', v)} />
        </Row>

        <Row label={t('settings.gcIntervalSec')}>
          <NumberInput value={r.gcIntervalSec} disabled={busy} onCommit={(v) => void set('retention.gcIntervalSec', v)} />
        </Row>
      </Section>

      {/* ── 草稿本 ────────────────────────────────────────── */}
      <Section title={t('settings.draftTitle')}>
        {/*
          归档保留期刻意放在这里而不是"保留策略"区：它与回收站的
          trashTtlSec 互不影响（一个是用户手写的作品，一个是自动捕获
          的流水），放在一起会让人以为改一个会带动另一个。
        */}
        <Row label={t('settings.draftArchiveTtlSec')} note={t('settings.draftArchiveTtlSec.note')} wide>
          <NumberInput
            value={settings.draft.archiveTtlSec}
            disabled={busy}
            onCommit={(v) => void set('draft.archiveTtlSec', v)}
          />
        </Row>

        <Row label={t('settings.draftAutoSaveDebounceMs')}>
          <NumberInput
            value={settings.draft.autoSaveDebounceMs}
            disabled={busy}
            onCommit={(v) => void set('draft.autoSaveDebounceMs', v)}
          />
        </Row>

        <Row label={t('settings.draftImageMaxBytes')}>
          <NumberInput
            value={settings.draft.imageMaxBytes}
            disabled={busy}
            onCommit={(v) => void set('draft.imageMaxBytes', v)}
          />
        </Row>
      </Section>

      {/* ── 存储 / 关于 ────────────────────────────────────── */}
      <Section title={t('settings.storage')}>
        <Row label={t('settings.cleanShutdownMarker')}>
          <Toggle
            value={settings.storage.cleanShutdownMarker}
            disabled={busy}
            onChange={(v) => void set('storage.cleanShutdownMarker', v)}
          />
        </Row>

        <Row label={t('settings.dataDir')} wide>
          <div className="pathrow">
            <code className="path">{p.dataDir || '—'}</code>
            <button
              type="button"
              className="btn btn-quiet"
              onClick={() => {
                void call('RevealDataDir').catch(() => {})
              }}
            >
              {t('settings.openDataDir')}
            </button>
          </div>
        </Row>

        <Row label={t('settings.configPath')} wide>
          <code className="path">{p.configPath || '—'}</code>
        </Row>

        <Row label={t('about.version')}>
          <code className="path">
            {p.version} · {p.platform}
          </code>
        </Row>

        <Row label={t('settings.uncovered')} wide note={UNCOVERED.join('  ·  ')} />
      </Section>
    </div>
  )
}

// ── 小控件 ────────────────────────────────────────────────────────
//
// 没有引 UI 库（§0.4 冻结的技术栈），所以这几个是最小可用实现：
// 受控值 + 一个"提交时机"明确的事件。
//
// 文本类控件**用 onBlur/回车提交而不是 onChange**：每次按键都写库
// 会让后端热重载过滤器几十次，而热重载是要重置去抖器的——
// 用户还在打字就被重置，行为很怪。

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="settings-section">
      <h2 className="settings-title">{title}</h2>
      <div className="settings-rows">{children}</div>
    </section>
  )
}

function Row({ label, note, wide, children }: { label: string; note?: string; wide?: boolean; children?: React.ReactNode }) {
  return (
    <div className={`settings-row ${wide ? 'settings-row-wide' : ''}`}>
      <div className="settings-label">
        <span>{label}</span>
        {note && <small className="settings-note">{note}</small>}
      </div>
      {children && <div className="settings-control">{children}</div>}
    </div>
  )
}

function Toggle({ value, disabled, onChange }: { value: boolean; disabled?: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className={`toggle ${value ? 'toggle-on' : ''}`}>
      <input
        type="checkbox"
        checked={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span className="toggle-track">
        <span className="toggle-knob" />
      </span>
    </label>
  )
}

function Select({
  value,
  options,
  disabled,
  onChange,
}: {
  value: string
  options: Array<{ value: string; label: string }>
  disabled?: boolean
  onChange: (v: string) => void
}) {
  return (
    <select className="input" value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

function MultiCheck({
  values,
  options,
  disabled,
  onChange,
}: {
  values: string[]
  options: Array<{ value: string; label: string }>
  disabled?: boolean
  onChange: (v: string[]) => void
}) {
  return (
    <div className="multicheck">
      {options.map((o) => (
        <label key={o.value} className="checkbox">
          <input
            type="checkbox"
            checked={values.includes(o.value)}
            disabled={disabled}
            onChange={(e) => {
              const next = e.target.checked ? [...values, o.value] : values.filter((v) => v !== o.value)
              // 全不选意味着"什么都不记录"，那和关掉捕获是一回事。
              // 不允许清空：让用户去关总开关，语义更明确。
              if (next.length === 0) return
              onChange(next)
            }}
          />
          {o.label}
        </label>
      ))}
    </div>
  )
}

function NumberInput({ value, disabled, onCommit }: { value: number; disabled?: boolean; onCommit: (v: number) => void }) {
  const [draft, setDraft] = useState(String(value))
  useEffect(() => setDraft(String(value)), [value])
  const commit = () => {
    const n = Number(draft)
    // 非数字就回退显示（不写库）：写 NaN 进去会让设置项永久损坏。
    if (!Number.isFinite(n)) {
      setDraft(String(value))
      return
    }
    if (n !== value) onCommit(n)
  }
  return (
    <input
      className="input input-num"
      type="text"
      inputMode="numeric"
      value={draft}
      disabled={disabled}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') (e.target as HTMLInputElement).blur()
      }}
    />
  )
}

/**
 * HotkeyInput 是"读键盘"的热键控件，**不是**文本框。
 *
 * 原来的做法是一个普通 TextInput，用户得自己把 "CmdOrCtrl+Shift+V" 敲进去：
 * 键名写法（CmdOrCtrl 还是 CommandOrControl？分隔符要不要空格？）全靠猜，
 * 猜错了后端解析失败、热键静默失效。键盘就在手边，不该让用户拼字符串。
 *
 * 交互约定：**先输入，再确认**。
 *
 *   点字段         → 进入输入态（系统里的热键临时让出，见 SuspendHotkey）
 *   按下组合键     → 只写进草稿并回显，**不落库、不生效**
 *   「确认」       → 提交这段草稿（草稿留空即"取消全局热键"）
 *   「取消」/ Esc  → 丢弃草稿，什么都不改
 *
 * 为什么不是"按下即生效"：全局热键是**系统级**副作用，会吞掉所有应用里的
 * 那个组合。用户按错了却当场生效，他得先把错的记住、再改一次；"先输入再确认"
 * 给了他一个反悔的机会，也让"留空即取消"有了明确的落点。
 *
 * 两处细节是真机反馈"输入不了"之后补上的，缺一个控件看起来就是坏的：
 *
 *  ① **监听挂在 window 的捕获阶段，不挂在元素上。** 原来靠按钮的 onKeyDown，
 *     而 macOS 的 WebKit（Safari 引擎）里点 button 并不会给它键盘焦点——
 *     这与 Chrome 不同，于是 keydown 根本派发不到那个元素：在浏览器里测得
 *     好好的，装进 App 就是"点了没反应"。
 *  ② **输入态期间让出全局热键。** 系统级热键在事件分发之前就把按键吃掉了，
 *     WebView 收不到那一次 keydown；用户想重按一遍当前组合（或换成任何已被
 *     占用的组合）时，界面会毫无反应。
 *
 * 输入态里按下修饰键就立刻回显（⌘⇧…），这不是装饰：它是用户判断"键盘被听见了"
 * 的唯一线索。显示走 formatCombo（⌘⇧V / Ctrl+Shift+V），空值显式写成"未设置"
 * 而不是留空——热键被清掉是一个**状态**，用户需要看得出来。
 */
function HotkeyInput({
  t,
  value,
  platform,
  disabled,
  onCommit,
}: {
  t: TFn
  value: string
  platform: string
  disabled?: boolean
  onCommit: (v: string) => void
}) {
  // draft === null 表示"不在输入态"，用 null 而不是布尔量是为了让
  // "在输入态但还没按过键"（草稿是空串）与"没在输入"能分开——
  // 前者的确认按钮是"取消快捷键"，后者根本不显示这些按钮。
  const [draft, setDraft] = useState<string | null>(null)
  const [notice, setNotice] = useState('')
  const inputting = draft !== null
  const label = formatCombo(value, platform)

  // 键盘监听是在 effect 里注册的，它的闭包拿不到最新的 draft（effect 只在
  // 进入/退出输入态时重挂）。用 ref 存一份同步副本。
  const draftRef = useRef('')
  const fieldRef = useRef<HTMLButtonElement>(null)

  const put = useCallback((v: string | null) => {
    draftRef.current = v ?? ''
    setDraft(v)
  }, [])

  /** 结束让出态：把系统里的热键按**当前设置**装回去（前端没提交的话就是旧值）。 */
  const release = useCallback(async () => {
    try {
      await call('ResumeHotkey')
    } catch {
      // 让出/收回是"尽力而为"的辅助动作：失败时热键可能暂时不可用，
      // 但不该在用户正看着设置页时弹一句他处理不了的错误。
    }
  }, [])

  const begin = useCallback(() => {
    if (disabled) return
    put(value)
    setNotice('')
    // 必须赶在用户按下第一个键之前让出，所以这里不 await。
    void call('SuspendHotkey').catch(() => {})
  }, [disabled, put, value])

  const cancel = useCallback(() => {
    put(null)
    setNotice('')
    void release()
  }, [put, release])

  const confirm = useCallback(() => {
    const next = draftRef.current.trim()
    put(null)
    setNotice('')
    // 顺序：先把热键装回来（结束让出态），再走常规设置路径写新值——
    // 那条路径自己会校验、注册，并在被占用时给出提示。
    void release().then(() => {
      if (next !== value) onCommit(next)
    })
  }, [onCommit, put, release, value])

  // 进入输入态时把焦点给字段（焦点圈本身就是"正在听"的一部分）。
  useEffect(() => {
    if (inputting) fieldRef.current?.focus()
  }, [inputting])

  // 卸载兜底：输入到一半切走视图时，别把热键留在"已让出"状态。
  // ResumeHotkey 是幂等的，没挂起时它什么都不做。
  useEffect(() => () => void release(), [release])

  // ── 输入态的键盘监听 ──────────────────────────────────────────
  //
  // ⚠️ 挂在 window 的**捕获**阶段。挂在元素的 onKeyDown 上在 WebKit 里不可靠：
  // macOS 上点 button 不给键盘焦点，事件根本到不了那个元素。
  //
  // 期间**吞掉**所有按键（preventDefault + stopPropagation）：否则 ⌘, 会把
  // 界面切回历史页、Esc 会被 App 当成"返回"、空格会滚列表——用户还在正经
  // 按组合键，界面自己先跑了。
  useEffect(() => {
    if (!inputting) return

    const onKeyDown = (e: KeyboardEvent) => {
      e.preventDefault()
      e.stopPropagation()
      // 长按会连发 keydown：重复写同一个草稿没意义，还一直重渲染。
      if (e.repeat) return

      if (!hasAnyModifier(e, platform)) {
        // 没有修饰键时，Esc / Enter / Backspace 是**操作**而不是热键。
        if (e.key === 'Escape') {
          cancel()
          return
        }
        if (e.key === 'Enter') {
          confirm()
          return
        }
        if (isClearKey(e)) {
          put('')
          setNotice('')
          return
        }
        // 裸键：后端不收（裸键的全局热键会把那个键从整个系统吃掉）。
        // 但"按了没反应"会让用户以为控件坏了，所以这里明说一句。
        // （两个键分开写，是为了让它们都以 t('literal') 的形式出现在源码里——
        //   test/check-i18n.mjs 是静态扫字面量的，写成三元它会认不出。）
        if (hasBindableKey(e)) {
          setNotice(t('settings.hotkey.needModifier'))
        } else {
          setNotice(t('settings.hotkey.unsupportedKey'))
        }
        return
      }

      const combo = comboFromEvent(e, platform)
      if (combo) {
        put(combo)
        setNotice('')
        return
      }
      // 带了修饰键、但主键后端不认（标点之类）：把按住的修饰键先显示出来，
      // 至少让用户看见"键盘是通的"。
      const held = heldModifiers(e, platform)
      if (held) put(held)
      setNotice(t('settings.hotkey.unsupportedKey'))
    }

    // keyup / keypress 也要吞：否则松手时补发的 click、空格滚动照样漏出去。
    const swallow = (e: Event) => {
      e.preventDefault()
      e.stopPropagation()
    }

    window.addEventListener('keydown', onKeyDown, true)
    window.addEventListener('keyup', swallow, true)
    window.addEventListener('keypress', swallow, true)
    return () => {
      window.removeEventListener('keydown', onKeyDown, true)
      window.removeEventListener('keyup', swallow, true)
      window.removeEventListener('keypress', swallow, true)
    }
  }, [inputting, platform, t, cancel, confirm, put])

  if (inputting) {
    // 提示语单独占一行：它比字段本身长得多，挤在一行会把字段压成一条缝。
    const held = formatCombo(draft ?? '', platform)
    // 草稿空 + 原本有热键 → 这个按钮的动作是"把它取消掉"，文案要跟着变。
    const confirmLabel = draft === '' && label !== '' ? t('settings.hotkey.confirmClear') : t('settings.hotkey.confirm')
    return (
      <div className="hotkeyrow hotkeyrow-col">
        <div className="hotkeyrow">
          <button
            ref={fieldRef}
            type="button"
            className="input hotkey-field hotkey-field-recording"
            // 输入态已经在这个元素上；点击时把焦点要回来，免得中途点到
            // 别处（比如提示文字）之后焦点圈消失、看起来像退出了输入态。
            onClick={() => fieldRef.current?.focus()}
          >
            {held || t('settings.hotkey.recording')}
          </button>
          <button type="button" className="btn btn-primary" onClick={confirm}>
            {confirmLabel}
          </button>
          <button type="button" className="btn btn-quiet" onClick={cancel}>
            {t('settings.hotkey.cancel')}
          </button>
        </div>
        <small className={notice ? 'hotkey-hint hotkey-hint-warn' : 'hotkey-hint'}>
          {notice || t('settings.hotkey.hint')}
        </small>
      </div>
    )
  }

  return (
    <div className="hotkeyrow">
      <button
        ref={fieldRef}
        type="button"
        className="input hotkey-field"
        disabled={disabled}
        title={t('settings.hotkey.record')}
        onClick={begin}
      >
        {label || t('settings.hotkey.none')}
      </button>
      {label !== '' && (
        <button type="button" className="btn btn-quiet" disabled={disabled} onClick={() => onCommit('')}>
          {t('settings.hotkey.clear')}
        </button>
      )}
    </div>
  )
}

function TextArea({
  value,
  rows,
  disabled,
  onCommit,
}: {
  value: string
  rows: number
  disabled?: boolean
  onCommit: (v: string) => void
}) {
  const [draft, setDraft] = useState(value)
  useEffect(() => setDraft(value), [value])
  return (
    <textarea
      className="input input-area"
      rows={rows}
      value={draft}
      disabled={disabled}
      spellCheck={false}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => draft !== value && onCommit(draft)}
    />
  )
}
