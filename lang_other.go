//go:build !darwin && !windows

package main

// nativeSystemLang 在 Linux 上只看环境变量（DetectSystemLang 已经做了）。
//
// 不再补一次 glibc 的 setlocale：绑定要开 cgo，而本项目在 Linux 上本就
// 只留接口骨架（HANDOFF-PROMPT §二），为一条 fallback 引入 cgo 不划算。
func nativeSystemLang() Lang { return LangSystem }
