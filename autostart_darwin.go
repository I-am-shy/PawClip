//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// macOS 的开机自启走 LaunchAgent。
//
// 为什么不用 SMAppService（macOS 13+ 的官方做法）：它要求 App 已签名并且有
// 稳定的 bundle 位置，而我们开发期跑的产物在 build/bin 下、还是 ad-hoc 签名。
// LaunchAgent 对未签名的 .app 也能工作，于是"自启这条链路"在开发期就是可
// 验证的。真要上架时再换 SMAppService——换的时候只需要改这个文件。
//
// 两个实现细节值得写下来：
//
//  1. RunAtLoad + KeepAlive 都不要设成"无论如何"。这是一个**剪贴板工具**，
//     用户点 Dock 关掉它时应该真的关掉；只有开机时拉起一次。所以
//     RunAtLoad = true、KeepAlive 不设。
//  2. plist 里写 **绝对路径**（用 os.Executable()）。写相对路径或依赖 PATH
//     的话，登录时 launchd 的环境与交互 shell 不同，会找不到可执行文件而
//     静默不启动——"自启没生效"最难查的就是这种。
const launchAgentLabel = "com.zego.pawclip"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", msgf(msgErrNoHomeDir, err, err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// autoStartEnabled 用 `launchctl print` 判断服务是否已注册。
//
// 为什么不用 `launchctl list | grep`：list 列的是"当前会话已加载的服务"，
// 登录前跑的城市里不一定包含我们；而 `print gui/<uid>/<label>` 直接问
// "这个服务在这个用户域里存不存在"，语义正是我们要的。
func autoStartEnabled() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", "print", launchAgentDomain()+"/"+launchAgentLabel).CombinedOutput()
	if err == nil {
		return true, nil
	}
	// "Could not find service" 是正常的"没开启"，不是错误。
	if strings.Contains(string(out), "Could not find service") ||
		strings.Contains(string(out), "No such process") {
		return false, nil
	}
	// 其它错误（权限、launchctl 不存在）如实报出来，不猜。
	return false, autoStartErr(msgActReadAutoStart, strings.TrimSpace(string(out)), err)
}

func setAutoStart(enabled bool) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !enabled {
		// 先 bootout 再删文件：bootout 失败（比如本来就没注册）不算错误，
		// 但文件一定要删掉，否则下次 launchctl load 会重新把它带回来。
		_, _ = exec.CommandContext(ctx, "launchctl", "bootout",
			launchAgentDomain()+"/"+launchAgentLabel).CombinedOutput()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return autoStartErr(msgActDeleteStart, err.Error(), err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return msgf(msgErrNoExePath, err, err)
	}
	// 解析符号链接：.app 的 MacOS/ 下那个文件通常是个指向真实二进制的链接，
	// 直接把链接路径写进 plist 也能跑，但一旦 build 目录被清理就断了。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}

	plist := buildLaunchAgentPlist(exe)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return autoStartErr(msgActLaunchDir, err.Error(), err)
	}
	// 0644：launchd 以当前用户身份读它，不需要更宽。
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return autoStartErr(msgActWriteAutoStart, err.Error(), err)
	}

	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap",
		launchAgentDomain(), path).CombinedOutput(); err != nil {
		// bootstrap 失败时把刚写的文件删掉：留下来会变成一个"看起来开了、
		// 其实没生效"的僵尸状态，下次用户开开关会撞 "already bootstrapped"。
		_ = os.Remove(path)
		return autoStartErr(msgActWriteAutoStart, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func autoStartDiag() string {
	path, err := launchAgentPath()
	if err != nil {
		return "path error: " + err.Error()
	}
	exe, _ := os.Executable()
	on, _ := autoStartEnabled()
	return fmt.Sprintf("launchAgent=%s exists=%v enabled=%v exe=%s",
		path, fileExists(path), on, exe)
}

// launchAgentDomain 是 launchd 的"用户图形会话"域。
//
// 必须带 uid：`gui/<uid>` 表示"这个用户登录到图形界面后才启动"。
// 用 system 域的话会在开机时（还没人登录）就尝试启动一个需要窗口会话的
// 应用，行为不可预期。
func launchAgentDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// buildLaunchAgentPlist 单独成函数是为了能被单测覆盖（不必真的动系统）。
func buildLaunchAgentPlist(exePath string) string {
	// 用 plist 的 XML 形式而不是 binary：用户可以自己 cat 出来看/改，
	// 出问题时好排查。
	// 路径要 XML 转义——用户主目录完全可能带 & 或 <（虽然罕见）。
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + launchAgentLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + xmlEscape(exePath) + `</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>ProcessType</key>
    <string>Interactive</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
