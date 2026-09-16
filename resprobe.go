package main

import (
	"context"
	"time"
)

// 本文件是 §12「空闲内存 / 空闲 CPU」这两行的取证手段。
//
// 这两行原先在整个仓库里没有任何自动取证：它们既不在单测范围内
// （测试进程的 RSS 与真实 App 无关），也没法靠脚本从外面读——`ps` / `top`
// 属于"读别的进程"，在受限环境（沙箱、部分 MDM 策略）下会被直接拒绝
// （实测 "operation not permitted: ps"）。于是这两个数字一直停在"设计目标"。
//
// 解法是让**进程自己**报：常驻内存用 Mach 的 task_info（见
// rss_darwin_cgo.go，读自己不需要权限），累计 CPU 时间用
// getrusage(RUSAGE_SELF)（见 cputime_unix.go）。两次采样相减就得到
// "这一段窗口"的真实占用率，天然排除了启动开销。
//
// 为什么是 Debug 级别：正常用户不需要在日志里看到它，而验收脚本用
// `log.level = debug` 跑，正好拿到。§14 第 10 条要求的"面板销毁后 log 一次
// 实际 RSS"是**事件**日志（panel 销毁时打一次 Info），与这里的周期性
// 采样不是一件事——那个要等面板闲置销毁落地（§13 已列风险）。

// resProbeDelays 是两个采样点相对启动的时刻。
//
// 取 5s / 25s：5s 足够 Wails 建完窗口与 WebView、库里跑完迁移与 FTS 回填，
// 之后留出 20 秒的干净窗口——空转窗口越长，"每隔 200ms 醒一次"这种量级的
// 开销越容易从计时噪声里分辨出来（实测该窗口内累计 CPU 是毫秒级）。
// 上限不取更长是因为验收脚本要为此干等：20 秒已经能把 1% 分辨到 0.1% 以下
// （getrusage 的精度是微秒）。
var resProbeDelays = []time.Duration{5 * time.Second, 25 * time.Second}

// startResourceProbes 在后台按 resProbeDelays 打资源快照。
//
// ctx 取消（App 关停）时立刻退出，不拖住退出流程。
func (a *App) startResourceProbes(ctx context.Context) {
	if !rusageSupported {
		return
	}
	go func() {
		start := time.Now()
		for _, d := range resProbeDelays {
			wait := d - time.Since(start)
			if wait < 0 {
				continue
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}

			at := time.Since(start)
			// rssMB 读不到时如实报 -1：0 会被下游当成"占用为 0"，
			// 那是比没有数据更糟的谎。
			rssMB := int64(-1)
			if n := ownRSSBytes(); n >= 0 {
				rssMB = n / (1024 * 1024)
			}
			a.log.Debug("资源快照",
				"atSec", int(at.Seconds()),
				"rssMB", rssMB,
				"cpuSec", cpuTimeSeconds(),
			)
		}
	}()
}
