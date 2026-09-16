//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows 的开机自启走注册表 Run 键（§11 P0）。
//
// 为什么不用"启动"文件夹里的快捷方式：创建 .lnk 需要 COM（IShellLink），
// 而注册表只需要一次写字符串——同一条链路，依赖少得多。
//
// 写在 **HKCU**\Software\Microsoft\Windows\CurrentVersion\Run 而不是 HKLM：
//   - HKCU 不需要管理员权限（HKLM 需要提权，一个剪贴板工具不该要求提权）；
//   - 语义也对："这台机器上的**这个用户**登录时启动"。
const (
	runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueNam = "PawClip"
)

func autoStartEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, autoStartErr(msgActRegistry, err.Error(), err)
	}
	defer func() { _ = k.Close() }()

	v, _, err := k.GetStringValue(runValueNam)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, autoStartErr(msgActReadAutoStart, err.Error(), err)
	}
	return strings.TrimSpace(v) != "", nil
}

func setAutoStart(enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return autoStartErr(msgActRegistry, err.Error(), err)
	}
	defer func() { _ = k.Close() }()

	if !enabled {
		if err := k.DeleteValue(runValueNam); err != nil && err != registry.ErrNotExist {
			return autoStartErr(msgActDeleteStart, err.Error(), err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return msgf(msgErrNoExePath, err, err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}

	// 命令行整体加引号：路径里几乎一定有空格（C:\Program Files\...），
	// 不加引号的话 Windows 会把 "C:\Program" 当成程序、剩下的当参数。
	// 这是注册表自启最经典的一个坑。
	cmd := `"` + exe + `"`
	if err := k.SetStringValue(runValueNam, cmd); err != nil {
		return autoStartErr(msgActWriteAutoStart, err.Error(), err)
	}
	return nil
}

func autoStartDiag() string {
	exe, _ := os.Executable()
	on, _ := autoStartEnabled()
	return fmt.Sprintf("regKey=HKCU\\%s\\%s enabled=%v exe=%s", runKeyPath, runValueNam, on, exe)
}
