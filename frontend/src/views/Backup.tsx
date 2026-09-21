// 导出 / 导入视图。
//
// 导入是**两阶段**的：PrecheckBackup（预检）→ 用户确认 → ImportBackup。
// 不让"选完文件就开导"，因为导入会改库，而用户在按下确认之前应该看到
// "将导入多少 / 跳过多少 / 需要多少磁盘"。预检结果也原样回传给导入，
// 避免两次读包之间包被换掉导致确认页与实际执行的不一致。
//
// 另外：**导入期间后端会暂停 GC**（backup.Import 内部做嵌套计数），
// 所以这里不需要也不该自己加阻止逻辑。

import { useCallback, useState } from 'react'
import type { T as TFn } from '../i18n'
import {
  call,
  type ExportOptions,
  type ExportResult,
  type ImportOptions,
  type ImportResult,
  type ImportRow,
  type PrecheckResult,
} from '../api'
import { formatBytes, formatDateTime, formatDuration } from '../format'
import { ConfirmModal } from '../components/ConfirmModal'

export type BackupProps = {
  t: TFn
  lastImport: ImportRow | null
  onReload: () => void
  onToast: (msg: string) => void
  onBack: () => void
}

export function Backup({ t, lastImport, onReload, onToast, onBack }: BackupProps) {
  const [tab, setTab] = useState<'export' | 'import'>('export')

  return (
    <div className="view-body">
      <div className="view-head">
        <button type="button" className="linkbtn" onClick={onBack}>
          ← {t('nav.list')}
        </button>
        <h2>{t('nav.backup')}</h2>
      </div>

      <div className="tabs">
        <button type="button" className={`tab ${tab === 'export' ? 'tab-on' : ''}`} onClick={() => setTab('export')}>
          {t('backup.export')}
        </button>
        <button type="button" className={`tab ${tab === 'import' ? 'tab-on' : ''}`} onClick={() => setTab('import')}>
          {t('backup.import')}
        </button>
      </div>

      {tab === 'export' ? (
        <ExportPane t={t} onToast={onToast} />
      ) : (
        <ImportPane t={t} lastImport={lastImport} onReload={onReload} onToast={onToast} />
      )}
    </div>
  )
}

// ── 导出 ──────────────────────────────────────────────────────────

function ExportPane({ t, onToast }: { t: TFn; onToast: (m: string) => void }) {
  const [busy, setBusy] = useState(false)
  const [outDir, setOutDir] = useState('')
  const [scope, setScope] = useState('full')
  const [embedFiles, setEmbedFiles] = useState(false)
  const [includeExpired, setIncludeExpired] = useState(false)
  const [result, setResult] = useState<ExportResult | null>(null)

  const run = async () => {
    setBusy(true)
    setResult(null)
    try {
      const opts: ExportOptions = {
        scope,
        categoryId: null,
        since: null,
        until: null,
        excludeKinds: null,
        includeExpired,
        manifestFormat: 'json',
        embedFiles,
        outputDir: outDir,
      }
      const res = await call('Export', opts)
      setResult(res)
      onToast(t('backup.exportDone'))
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="pane">
      <div className="formrow">
        <label>{t('backup.scope')}</label>
        <select className="input" value={scope} disabled={busy} onChange={(e) => setScope(e.target.value)}>
          <option value="full">{t('backup.scope.full')}</option>
          <option value="favorites">{t('backup.scope.favorites')}</option>
        </select>
      </div>

      <div className="formrow">
        <label>{t('backup.path')}</label>
        <div className="pathrow">
          <input className="input" type="text" value={outDir} placeholder="(默认数据目录/exports)" disabled={busy} onChange={(e) => setOutDir(e.target.value)} />
          <button
            type="button"
            className="btn btn-quiet"
            disabled={busy}
            onClick={async () => {
              // 用户取消返回空串：**不要**把它当成"选择了空目录"覆盖掉已有的输入。
              const d = await call('PickExportDir').catch(() => '')
              if (d) setOutDir(d)
            }}
          >
            …
          </button>
        </div>
      </div>

      <div className="formrow">
        <label className="checkbox">
          <input type="checkbox" checked={embedFiles} disabled={busy} onChange={(e) => setEmbedFiles(e.target.checked)} />
          {t('backup.embedFiles')}
        </label>
      </div>
      <div className="formrow">
        <label className="checkbox">
          <input type="checkbox" checked={includeExpired} disabled={busy} onChange={(e) => setIncludeExpired(e.target.checked)} />
          {t('backup.includeExpired')}
        </label>
      </div>

      <div className="formrow">
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void run()}>
          {busy ? t('backup.exporting') : t('backup.start')}
        </button>
      </div>

      {result && (
        <div className="resultbox">
          <div className="result-title">{t('backup.exportDone')}</div>
          <KV k={t('backup.path')} v={result.path} mono />
          <KV k={t('backup.total')} v={String(result.items)} />
          {/* 草稿只在 full 范围里进包，为 0 时不必占一行。 */}
          {result.drafts > 0 && <KV k={t('backup.drafts')} v={String(result.drafts)} />}
          <KV k={t('stats.diskBytes')} v={`${formatBytes(result.blobBytes)} (${result.blobs})`} />
          <KV k={t('backup.tookMs')} v={formatDuration(result.tookMs)} />
          {result.missingBlobs > 0 && (
            <div className="warnbox">
              {t('common.warnings')}: {result.missingBlobs}
            </div>
          )}
          {result.warnings && result.warnings.length > 0 && (
            <ul className="warnlist">
              {result.warnings.slice(0, 8).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          )}
          <div className="formrow">
            <button
              type="button"
              className="btn btn-quiet"
              onClick={() => {
                void (window.go?.main?.App as { RevealPath?: (p: string) => Promise<void> } | undefined)?.RevealPath?.(result.path).catch(() => {})
              }}
            >
              {t('settings.openDataDir')}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// ── 导入 ──────────────────────────────────────────────────────────

function defaultImportOpts(): ImportOptions {
  return {
    // 默认 merge：它是**唯一不会丢数据**的策略（skip 丢新内容、
    // overwrite 丢本机那条），而用户点"导入"的意图通常正是"把这些也加进来"。
    conflictPolicy: 'merge',
    expiryPolicy: 'keep',
    importExpired: false,
    categoryPolicy: 'merge',
    importSettings: false,
  }
}

function ImportPane({
  t,
  lastImport,
  onReload,
  onToast,
}: {
  t: TFn
  lastImport: ImportRow | null
  onReload: () => void
  onToast: (m: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const [path, setPath] = useState('')
  const [opts, setOpts] = useState<ImportOptions>(defaultImportOpts)
  const [pre, setPre] = useState<PrecheckResult | null>(null)
  const [result, setResult] = useState<ImportResult | null>(null)
  const [latest, setLatest] = useState<ImportRow | null>(lastImport)

  const pick = async () => {
    const p = await call('PickBackupFile').catch(() => '')
    if (!p) return
    setPath(p)
    setPre(null)
    setResult(null)
    setBusy(true)
    try {
      const pc = await call('PrecheckBackup', p, opts)
      setPre(pc)
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  const confirmImport = async () => {
    if (!pre) return
    setBusy(true)
    try {
      // 把预检结果原样传回去：后端据此复用"已经算出的一批判断"，
      // 也避免两次读包之间包被换掉导致"确认页说 100 条、实际导 80 条"。
      const res = await call('ImportBackup', pre.path, opts, pre)
      setResult(res)
      setPre(null)
      onToast(t('backup.importDone'))
      const li = await call('LastImport').catch(() => null)
      setLatest(li)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  /** recheck 在选项变化后重跑预检（不然确认页的数字是旧策略算出来的）。 */
  const recheck = useCallback(
    async (p: string, o: ImportOptions) => {
      setBusy(true)
      try {
        setPre(await call('PrecheckBackup', p, o))
      } catch (e: unknown) {
        onToast(t('err.generic', { err: msg(e) }))
      } finally {
        setBusy(false)
      }
    },
    [onToast, t],
  )

  // 撤销导入是破坏性的（那批条目会被删掉），所以先弹确认框。
  // 以前这里是 window.confirm —— 见 ConfirmModal 顶部：WKWebView 里那个
  // 对话框根本不会出现，`if (!confirm(...)) return` 会让这个按钮**点了没反应**。
  const [askRollback, setAskRollback] = useState(false)

  const rollback = async () => {
    if (!latest || !latest.rollbackPossible) return
    setAskRollback(false)
    setBusy(true)
    try {
      const n = await call('RollbackImport', latest.id)
      onToast(t('trash.restored', { n }))
      setLatest(null)
      onReload()
    } catch (e: unknown) {
      onToast(t('err.generic', { err: msg(e) }))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="pane">
      <div className="formrow">
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void pick()}>
          {busy && !pre ? t('backup.prechecking') : t('backup.pickFile')}
        </button>
        {path && <code className="path">{path}</code>}
      </div>

      <div className="formrow">
        <label>{t('backup.conflictPolicy')}</label>
        <select
          className="input"
          value={opts.conflictPolicy}
          disabled={busy}
          onChange={(e) => {
            const next = { ...opts, conflictPolicy: e.target.value }
            setOpts(next)
            // 改了策略要重跑预检：否则"将导入 N 条"是旧策略算出来的，
            // 用户会以为策略没生效。
            if (path) void recheck(path, next)
          }}
        >
          <option value="merge">{t('backup.conflict.merge')}</option>
          <option value="skip">{t('backup.conflict.skip')}</option>
          <option value="overwrite">{t('backup.conflict.overwrite')}</option>
        </select>
      </div>

      <div className="formrow">
        <label>{t('backup.expiryPolicy')}</label>
        <select
          className="input"
          value={opts.expiryPolicy}
          disabled={busy}
          onChange={(e) => {
            const next = { ...opts, expiryPolicy: e.target.value }
            setOpts(next)
            if (path) void recheck(path, next)
          }}
        >
          <option value="keep">{t('backup.expiry.keep')}</option>
          <option value="rebase">{t('backup.expiry.rebase')}</option>
          <option value="reset">{t('backup.expiry.reset')}</option>
        </select>
      </div>

      {pre && (
        <div className="resultbox">
          <div className="result-title">{t('backup.precheckTitle')}</div>
          {pre.alreadyImported && <div className="warnbox">{t('backup.alreadyImported')}</div>}
          <KV k={t('backup.total')} v={String(pre.total)} />
          <KV k={t('backup.willImport')} v={String(pre.willImport)} />
          {pre.skipDuplicate > 0 && <KV k={t('backup.skipDup')} v={String(pre.skipDuplicate)} />}
          {pre.skipExpired > 0 && <KV k={t('backup.skipExpired')} v={String(pre.skipExpired)} />}
          {pre.invalid > 0 && <KV k={t('backup.invalid')} v={String(pre.invalid)} />}
          {/* 草稿单独一行，措辞必须是"新增"：它没有指纹，重导一次就多一批。
              写"将导入"会让用户以为和条目一样会去重。 */}
          {pre.totalDrafts > 0 && (
            <KV k={t('backup.draftsNew')} v={String(pre.willImportDrafts)} />
          )}
          <KV k={t('backup.total')} v={`${formatBytes(pre.uncompressedBytes)} → ${t('stats.aliveBytes')} ${formatBytes(pre.needBytes)}`} />
          {pre.availableBytes >= 0 && (
            <KV k={t('stats.diskBytes')} v={formatBytes(pre.availableBytes)} />
          )}
          {pre.warnings && pre.warnings.length > 0 && (
            <ul className="warnlist">
              {pre.warnings.slice(0, 8).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          )}
          <div className="formrow">
            <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void confirmImport()}>
              {busy ? t('backup.importing') : t('backup.confirm')}
            </button>
            <button type="button" className="btn btn-quiet" disabled={busy} onClick={() => setPre(null)}>
              {t('common.cancel')}
            </button>
          </div>
        </div>
      )}

      {result && (
        <div className="resultbox">
          <div className="result-title">{t('backup.importDone')}</div>
          {/* 主数字是"现在在库里的"，那才是用户关心的；真插了几行另列。 */}
          <KV k={t('backup.willImport')} v={String(result.imported)} />
          <KV k={t('backup.inserted')} v={String(result.inserted)} />
          <KV k={t('backup.skipDup')} v={String(result.skipped)} />
          <KV k={t('backup.merged')} v={String(result.merged)} />
          <KV k={t('backup.overwritten')} v={String(result.overwritten)} />
          {result.failed > 0 && <KV k={t('backup.failed')} v={String(result.failed)} />}
          {result.draftsImported > 0 && (
            <KV k={t('backup.draftsNew')} v={String(result.draftsImported)} />
          )}
          {result.draftsFailed > 0 && (
            <KV k={t('backup.draftsFailed')} v={String(result.draftsFailed)} />
          )}
          <KV k={t('backup.tookMs')} v={formatDuration(result.tookMs)} />
          {result.errors && result.errors.length > 0 && (
            <ul className="errlist">
              {result.errors.slice(0, 8).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          )}
          {result.warnings && result.warnings.length > 0 && (
            <ul className="warnlist">
              {result.warnings.slice(0, 8).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          )}
        </div>
      )}

      <div className="formrow">
        {latest ? (
          <div className="lastimport">
            <span className="dim">
              {t('backup.lastImport')}: {formatDateTime(latest.finishedAt ?? latest.startedAt)} ·{' '}
              {t('backup.willImport')} {latest.imported}
            </span>
            <button
              type="button"
              className="btn btn-quiet"
              disabled={busy || !latest.rollbackPossible}
              onClick={() => setAskRollback(true)}
            >
              {t('backup.rollback')}
            </button>
          </div>
        ) : (
          <span className="dim">{t('backup.noLastImport')}</span>
        )}
      </div>

      {askRollback && (
        <ConfirmModal
          title={t('backup.rollbackTitle')}
          body={t('backup.rollbackConfirm')}
          confirmLabel={t('backup.rollback')}
          cancelLabel={t('common.cancel')}
          onCancel={() => setAskRollback(false)}
          onConfirm={() => void rollback()}
        />
      )}
    </div>
  )

}

function KV({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="kv">
      <span className="kv-k">{k}</span>
      <span className={mono ? 'kv-v mono' : 'kv-v'}>{v}</span>
    </div>
  )
}

function msg(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
