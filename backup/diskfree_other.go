//go:build !unix && !windows

package backup

// diskFree 在既不是 unix 也不是 windows 的平台上无法探测可用空间。
//
// 返回 -1 而不是 0 或某个猜测值：-1 的语义是"不知道"，
// 调用方会据此**跳过**空间检查而不是拒绝导入。让一个探测能力的缺失
// 变成"拒绝服务"是不可接受的。
func diskFree(dir string) int64 { return -1 }
