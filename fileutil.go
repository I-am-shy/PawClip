package main

import "os"

// ensureDir 确保目录存在（含中间层级）。
//
// 模式固定 0o700：数据目录里全是用户复制过的内容（可能含密码、验证码），
// 权限放宽就是把这些内容暴露给同机其他用户。这个值不该由调用方决定，
// 所以不开放为参数。
func ensureDir(p string) error {
	if p == "" {
		return nil
	}
	return os.MkdirAll(p, 0o700)
}

// fileExists 报告一个路径是否存在（目录也算存在）。
//
// 存在的唯一用途是诊断串（"自启项文件在不在"），所以**不返回 error**：
// 诊断信息里"查不出来"与"不存在"都表现为 false 已经够用，
// 而为了区分它们把签名搞成 (bool, error) 会让每个调用点都要处理 error。
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}
