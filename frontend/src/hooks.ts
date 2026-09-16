// 几个共享 hook。
//
// 不引状态库（docs/DESIGN.md §0.4 冻结的技术栈里没有），所以状态由 App 持有、
// 用 props 往下传。这个尺度（一个面板 + 五个视图）还不至于需要 store。

import { useCallback, useEffect, useRef, useState } from 'react'
import { call, type PanelEvent } from './api'

/**
 * useDebounce 返回一个"延迟后才会更新"的值。
 *
 * 为什么是防抖而不是节流：搜索要的是"用户停下打字后再查"。
 * 节流会按固定节奏发出中间态的查询（`文`、`文档`、`文档检` ……），
 * 每一次都是一轮 SQL；防抖只发最后一次。§11 P0 的"防抖 120ms"就是这个。
 *
 * 返回的第二个值是"是否正在等待"：界面据此显示加载态。
 * 注意它会在值真正更新后变回 false，与"查询正在进行"不是一回事。
 */
export function useDebounce<T>(value: T, delayMs: number): [T, boolean] {
  const [settled, setSettled] = useState(value)
  const [pending, setPending] = useState(false)

  useEffect(() => {
    if (value === settled) {
      setPending(false)
      return
    }
    setPending(true)
    const id = window.setTimeout(() => {
      setSettled(value)
      setPending(false)
    }, delayMs)
    return () => window.clearTimeout(id)
    // settled 参与依赖是必要的：它是"当前已提交的值"，
    // 少了它，连续两次相同的输入不会重新排定时器。
  }, [value, delayMs, settled])

  return [settled, pending]
}

/**
 * usePolledEvents 轮询后端的原生事件队列。
 *
 * ⚠️ 为什么是轮询而不是后端主动推：面板的原生回调（托盘点击、全局热键）
 * 发生在 **AppKit/Win32 的线程**上，从那里调 Wails 的 JS eval 是未定义
 * 行为。后端把事件放进队列，由前端按固定节奏取走——代价是这里有一个
 * interval，换来的是不会莫名其妙崩溃。
 *
 * 间隔 400ms 是权衡：热键呼出后用户对"面板亮起来"的感知阈值大约在
 * 200–300ms，而托盘点击本来就有系统菜单的动画；再快就是白烧 CPU。
 * 面板不可见时**不轮询**（见 enabled），空闲时不产生任何 IPC。
 */
export function usePolledEvents(
  enabled: boolean,
  onEvents: (evs: PanelEvent[]) => void,
  intervalMs = 400,
): void {
  // 用 ref 持有回调：把它放进依赖会让每次渲染都重建 interval，
  // 而调用方几乎总是传一个内联函数。
  const cbRef = useRef(onEvents)
  cbRef.current = onEvents

  useEffect(() => {
    if (!enabled) return
    let stop = false
    const tick = async () => {
      if (stop) return
      try {
        const evs = await call('TakeEvents')
        if (!stop && evs && evs.length > 0) cbRef.current(evs)
      } catch {
        // 后端还没就绪（或正在初始化）时不报错：这是启动早期的正常状态，
        // 下一轮会自己好。把错误弹到界面上只会看到一个无意义的红条。
      }
    }
    const id = window.setInterval(tick, intervalMs)
    void tick()
    return () => {
      stop = true
      window.clearInterval(id)
    }
  }, [enabled, intervalMs])
}

/** useInterval 按固定间隔跑一个回调（用于统计页的内存刷新）。 */
export function useInterval(fn: () => void, ms: number | null): void {
  const cbRef = useRef(fn)
  cbRef.current = fn
  useEffect(() => {
    if (ms == null) return
    const id = window.setInterval(() => cbRef.current(), ms)
    return () => window.clearInterval(id)
  }, [ms])
}

/** useAsync 包一层"加载中 / 出错"的状态，省掉每个视图各写一遍。 */
export function useAsync<T>(
  loader: () => Promise<T>,
  deps: unknown[],
): { data: T | null; error: string; loading: boolean; reload: () => void } {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)

  const loaderRef = useRef(loader)
  loaderRef.current = loader

  useEffect(() => {
    let alive = true
    setLoading(true)
    loaderRef.current()
      .then((v) => {
        if (!alive) return
        setData(v)
        setError('')
      })
      .catch((e: unknown) => {
        if (!alive) return
        // 先清掉旧数据：留着它会让界面显示"上次成功的结果 + 一个错误条"，
        // 用户不知道该信哪个。
        setData(null)
        setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])
  return { data, error, loading, reload }
}
