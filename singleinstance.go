package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
)

// instanceKey 把"这次要打开哪个数据库"折成一个稳定的短 key。
//
// 单实例守卫按**数据库路径**而不是按进程名：同一个库只允许一个 PawClip，
// 不同 -config（不同库）仍然可以同时跑——这是开发期隔离测试需要的，
// 也避免把"两个真的不同 profile"误杀。两个默认安装（build/bin 与
// %LocalAppData%\Programs\PawClipAccept）指向同一个默认库，因此会互相拦住。
//
// Windows 路径大小写不敏感，先 ToLower 再哈希；macOS/Linux 保留原样。
func instanceKey(boot BootstrapConfig) string {
	p := strings.TrimSpace(boot.Database.Path)
	if p == "" {
		if def, err := DefaultDatabasePath(); err == nil {
			p = def
		}
	}
	if p == "" {
		// 连默认路径都确定不了（环境级故障）时仍然给一个稳定的兜底 key，
		// 让两个同样故障的进程至少不会同时去抢初始化。
		p = "pawclip-default"
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:16])
}
