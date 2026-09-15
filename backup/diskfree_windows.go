//go:build windows

package backup

import (
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpace = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskFree 返回 dir 所在卷的可用字节数（Windows 实现）。
//
// 用 GetDiskFreeSpaceExW 而不是 GetDiskFreeSpaceW：后者返回的是
// 簇数，且 32 位簇数在大卷上会回绕。Ex 版本直接给 64 位字节数，
// 而且取的是"当前用户可用的空闲空间"（已扣除配额），
// 这正是导入前要判断的量。
//
// 取不到时返回 -1。
func diskFree(dir string) int64 {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var freeToCaller, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpace.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return -1
	}
	const maxI64 = int64(^uint64(0) >> 1)
	if freeToCaller > uint64(maxI64) {
		return maxI64
	}
	return int64(freeToCaller)
}
