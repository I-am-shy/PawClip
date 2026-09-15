//go:build unix

package backup

import "golang.org/x/sys/unix"

// diskFree 返回 dir 所在卷的可用字节数。
//
// 用 Bavail 而不是 Bfree：Bfree 里面有一部分是留给 root 的保留块，
// 普通进程写不进去。拿 Bfree 去判断"够不够放"会高估，
// 于是导入跑到一半才发现写不下。
//
// 取不到时返回 -1，调用方据此跳过空间检查。
func diskFree(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return -1
	}
	// 溢出保护：两个 uint64 相乘可能回绕。
	// 先按块数乘，若大到不可能就退回 MaxInt64。
	const maxI64 = int64(^uint64(0) >> 1)
	if st.Bavail > uint64(maxI64)/uint64(st.Bsize) {
		return maxI64
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
