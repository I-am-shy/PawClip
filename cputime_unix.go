//go:build darwin || linux

package main

import "syscall"

// cpuTimeSeconds 返回本进程累计消耗的 CPU 时间（用户态 + 内核态，单位秒）。
//
// 为什么不用 `ps -o %cpu`：那个数是"自进程启动以来的平均占用率"，
// 把启动开销（建库、迁移、Wails 建窗口与 WebView）一起摊了进来，
// 拿它评估 §12 的"空闲 CPU < 1%"会系统性地偏高。
// getrusage(RUSAGE_SELF) 给的是**累计值**，两次采样相减就得到
// "刚刚这一段窗口"的真实占用率——在进程内部就能算，不需要任何权限。
//
// 返回 -1 表示平台不支持（调用方据此显示"未知"，而不是编一个 0）。
func cpuTimeSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

// rusageSupported 报告当前平台是否提供 CPU 累计时间。
const rusageSupported = true
