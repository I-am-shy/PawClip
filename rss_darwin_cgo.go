//go:build darwin && cgo

package main

/*
#include <mach/mach.h>
#include <mach/task_info.h>

// paw_own_rss_bytes 用 Mach 的 task_info 读**本进程**的常驻内存。
//
// 为什么不用 `ps -o rss=`：
//   - 它要 fork 一个进程，为读一个数字付出 ~10ms 与一次 exec，
//     而这是内存验收里会被反复调用的东西；
//   - 它会受环境影响。本项目的开发沙箱就禁掉了 ps
//     （"operation not permitted: ps"），实测直接拿不到数。
//     Mach 调用是对自己的内核请求，没有这两层问题。
//
// 返回 -1 表示调用失败（调用方据此显示"未知"，而不是编一个 0）。
long paw_own_rss_bytes(void) {
    mach_task_basic_info_data_t info;
    mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
    kern_return_t kr = task_info(mach_task_self(),
                                 MACH_TASK_BASIC_INFO,
                                 (task_info_t)&info,
                                 &count);
    if (kr != KERN_SUCCESS) return -1;
    return (long)info.resident_size;
}
*/
import "C"

// ownRSSBytes 返回本进程的常驻内存；读不到时返回 -1。
func ownRSSBytes() int64 {
	return int64(C.paw_own_rss_bytes())
}
