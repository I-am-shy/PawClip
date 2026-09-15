import { useEffect, useState } from 'react'

// ── 与 Go 侧的绑定 ──────────────────────────────────────────────
//
// Wails 会把 App 的导出方法生成到 frontend/wailsjs/ 下。那个目录是
// **构建产物**（已在 .gitignore 里），所以这里手写一份最小声明，
// 让 `tsc --noEmit` 在没有生成物时也能通过。
// 字段与 app.go 的 Health() 返回值一一对应。
type Health = {
  version: string
  platform: string
  initError: string
  dbPath: string
  schemaVersion: number
  ftsAvailable: boolean
  integrityNote: string
  aliveItems: number
  trashedItems: number
  allItems: number
  totalAliveBytes: number
  capture: {
    ticks: number
    reads: number
    accepted: number
    enqueueErr: number
    readErrs: number
    empty: number
    busy: number
    flapping: number
    drops: Record<string, number>
    lastError: string
  }
  writer: {
    enqueued: number
    rejected: number
    items: number
    batches: number
    errors: number
    checkpoints: number
    lastFlushMs: number
  }
}

declare global {
  interface Window {
    go?: {
      main?: {
        App?: {
          Health?: () => Promise<Health>
        }
      }
    }
  }
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState<string>('')

  // M1 没有 UI，这个页面唯一的作用是"证明后端活着"：
  // 每秒拉一次 Health，把捕获链路与落库的计数直接摊在屏幕上。
  useEffect(() => {
    let stopped = false
    let timer = 0

    async function tick() {
      const fn = window.go?.main?.App?.Health
      if (!fn) {
        // 绑定还没注入好（或页面被单独打开），等等再来
        timer = window.setTimeout(tick, 300)
        return
      }
      try {
        const h = await fn()
        if (!stopped) {
          setHealth(h)
          setError('')
        }
      } catch (e) {
        if (!stopped) setError(String(e))
      }
      if (!stopped) timer = window.setTimeout(tick, 1000)
    }

    tick()
    return () => {
      stopped = true
      window.clearTimeout(timer)
    }
  }, [])

  return (
    <main className="shell">
      <header className="hdr">
        <h1>PawClip · 喵喵贴</h1>
        <p className="sub">M1 · 捕获链路 + 落库（纯后端，界面留占位）</p>
      </header>

      {error && <p className="err">读取后端状态失败：{error}</p>}
      {!health && !error && <p className="muted">正在连接后端…</p>}

      {health?.initError && (
        <p className="err">
          后端初始化失败，本次运行不会记录任何内容：{health.initError}
        </p>
      )}

      {health && !health.initError && health.dbPath === '' && (
        <p className="muted">后端正在初始化…</p>
      )}

      {health && health.dbPath !== '' && (
        <>
          <section className="card">
            <h2>存储</h2>
            <dl>
              <dt>版本</dt>
              <dd>
                {health.version} · {health.platform}
              </dd>
              <dt>库</dt>
              <dd className="mono break">{health.dbPath}</dd>
              <dt>
                schema / FTS5
              </dt>
              <dd>
                v{health.schemaVersion} · {health.ftsAvailable ? '可用' : '降级为 LIKE'}
              </dd>
              <dt>条目</dt>
              <dd>
                存活 {health.aliveItems} · 回收站 {health.trashedItems} · 合计{' '}
                {health.allItems}
              </dd>
              <dt>占用</dt>
              <dd>{formatBytes(health.totalAliveBytes)}</dd>
              {health.integrityNote && (
                <>
                  <dt>完整性</dt>
                  <dd className="err">{health.integrityNote}</dd>
                </>
              )}
            </dl>
          </section>

          <section className="card">
            <h2>捕获</h2>
            <dl>
              <dt>信号 / 读取</dt>
              <dd>
                {health.capture.ticks} / {health.capture.reads}
              </dd>
              <dt>已落库</dt>
              <dd>{health.capture.accepted}</dd>
              <dt>入队失败</dt>
              <dd>{health.capture.enqueueErr}</dd>
              <dt>读失败</dt>
              <dd>
                空 {health.capture.empty} · 占用 {health.capture.busy} · 抖动{' '}
                {health.capture.flapping} · 其他 {health.capture.readErrs}
              </dd>
              <dt>丢弃</dt>
              <dd className="mono break">
                {Object.keys(health.capture.drops).length === 0
                  ? '（暂无）'
                  : Object.entries(health.capture.drops)
                      .map(([k, v]) => `${k}=${v}`)
                      .join('  ')}
              </dd>
              {health.capture.lastError && (
                <>
                  <dt>最近错误</dt>
                  <dd className="err break">{health.capture.lastError}</dd>
                </>
              )}
            </dl>
          </section>

          <section className="card">
            <h2>写入器</h2>
            <dl>
              <dt>入队 / 落库</dt>
              <dd>
                {health.writer.enqueued} / {health.writer.items}
              </dd>
              <dt>批次 / 检查点</dt>
              <dd>
                {health.writer.batches} / {health.writer.checkpoints}
              </dd>
              <dt>最近一批</dt>
              <dd>{health.writer.lastFlushMs} ms</dd>
              <dt>错误 / 拒绝</dt>
              <dd>
                {health.writer.errors} / {health.writer.rejected}
              </dd>
            </dl>
          </section>
        </>
      )}
    </main>
  )
}
