//go:build !(darwin && cgo)

package main

// ownRSSBytes 在非 macOS（或 macOS 但没开 cgo）时返回 -1（= 读不到）。
//
// 返回 -1 而不是 0 是有意的：0 是一个**合法的读数**（进程没占内存是不可能的，
// 但一个"未知"被写成 0 会让人以为量到了），-1 能被调用方明确识别为"没测到"，
// 前端据此显示"未知"而不是"0 MB"。
func ownRSSBytes() int64 { return -1 }
