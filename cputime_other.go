//go:build !darwin && !linux

package main

// cpuTimeSeconds 在 Windows 上返回 -1。
//
// Windows 没有 getrusage(2)：对应的量是 GetProcessTimes()，
// 要经 x/sys/windows 拿 FILETIME 再自己换算。本期的内存/CPU 验收只在
// macOS 上做（§12 给了 Windows 的**目标值** 25MB，但没有要求在 mac 上
// 量 Windows 的数），所以这里先如实返回"读不到"，而不是填一个 0
// ——0 会被下游当成"占用为 0"，那是比没有数据更糟的谎。
//
// 要补的时候在这里实现 GetProcessTimes：返回相同的语义（累计秒数）。
func cpuTimeSeconds() float64 { return -1 }

// rusageSupported 报告当前平台是否提供 CPU 累计时间。
const rusageSupported = false
