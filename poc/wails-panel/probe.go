package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// diag 对应 panel_darwin.m 里 pawclip_diag_json 的快照结构
type diag struct {
	Mode               int    `json:"mode"`
	Accessory          int    `json:"accessory"`
	AppBundleID        string `json:"appBundleID"`
	WindowClass        string `json:"windowClass"`
	InstanceSize       int    `json:"instanceSize"`
	NSWindowSize       int    `json:"nsWindowSize"`
	NSPanelSize        int    `json:"nsPanelSize"`
	StyleMask          int64  `json:"styleMask"`
	NonactivatingBit   int    `json:"nonactivatingBit"`
	CollectionBehavior int64  `json:"collectionBehavior"`
	NSAppIsActive      int    `json:"nsAppIsActive"`
	WindowIsKey        int    `json:"windowIsKey"`
	CanBecomeKey       int    `json:"canBecomeKey"`
	WindowVisible      int    `json:"windowVisible"`
	FrontmostBundleID  string `json:"frontmostBundleID"`
	FrontmostName      string `json:"frontmostName"`
}

type snapshot struct {
	At   string `json:"at"`
	Tag  string `json:"tag"`
	Note string `json:"note"`
	D    diag   `json:"diag"`
}

type probeReport struct {
	Mode      string     `json:"mode"`
	Accessory bool       `json:"accessory"`
	Verdict   string     `json:"verdict"`
	Findings  []string   `json:"findings"`
	Snapshots []snapshot `json:"snapshots"`
	StartedAt string     `json:"startedAt"`
}

var (
	probeLogLines []string
	probeRunning  bool
	probeDone     bool
)

func probeLog(format string, a ...interface{}) {
	line := fmt.Sprintf(format, a...)
	probeLogLines = append(probeLogLines, line)
	fmt.Println(line)
}

func modeName(mode int) string {
	switch mode {
	case modeBaseline:
		return "baseline"
	case modeMaskOnly:
		return "mask-only"
	case modePanel:
		return "panel"
	case modePanelMin:
		return "panel-min"
	case modeAdopt:
		return "adopt"
	}
	return "unknown"
}

// activateOtherApp 把前台切到 Finder，用来构造"别的 App 在前台"的场景
func activateOtherApp() error {
	return exec.Command("open", "-a", "Finder").Run()
}

func runProbe(mode int, accessory bool) {
	probeRunning = true
	defer func() { probeRunning = false; probeDone = true }()

	var snaps []snapshot
	t0 := time.Now()
	el := func() string { return fmt.Sprintf("%6.2fs", time.Since(t0).Seconds()) }

	record := func(tag, note string) diag {
		var d diag
		raw := platformDiag()
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			probeLog("%s  [%s] 解析诊断数据失败: %v", el(), tag, err)
		}
		snaps = append(snaps, snapshot{At: el(), Tag: tag, Note: note, D: d})
		probeLog("%s  [%-24s] active=%-5v key=%-5v visible=%-5v frontmost=%s",
			el(), tag, d.NSAppIsActive == 1, d.WindowIsKey == 1,
			d.WindowVisible == 1, d.FrontmostName)
		return d
	}

	probeLog("")
	probeLog("═══ PawClip M0 门禁验证 · mode=%s accessory=%v ═══", modeName(mode), accessory)
	probeLog("")

	// 1. 等 Wails 把窗口建出来
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !platformHasWindow() {
		time.Sleep(80 * time.Millisecond)
	}
	if !platformHasWindow() {
		probeLog("%s  FATAL：等待 Wails 窗口超时，验证中止", el())
		finish(mode, accessory, snaps, "ABORT", []string{"等待 Wails 建窗超时，无法验证"})
		return
	}

	// 2. 应用处理方案
	if err := platformSetup(mode, accessory); err != nil {
		probeLog("%s  FATAL：setup 失败 %v", el(), err)
		finish(mode, accessory, snaps, "ABORT", []string{"setup 失败: " + err.Error()})
		return
	}
	time.Sleep(400 * time.Millisecond)
	record("A0-after-setup", "方案已应用")

	// 3. 冷启动显示：不激活 App，直接显示 + 取 key
	k1 := platformShowNoActivate()
	time.Sleep(600 * time.Millisecond)
	s1 := record("A1-show-cold", fmt.Sprintf("orderFrontRegardless→makeKey，key=%v", k1))

	// 4. 显式激活 App 后再取 key —— 分离出"App 激活"这个因素
	platformActivateApp()
	time.Sleep(400 * time.Millisecond)
	k2 := platformMakeKey()
	time.Sleep(500 * time.Millisecond)
	s2 := record("A2-app-active-then-key", fmt.Sprintf("activate→makeKey，key=%v", k2))

	// 5. 把前台让给别的 App —— 核心观察点：key 会不会被系统收走
	if err := activateOtherApp(); err != nil {
		probeLog("%s  警告：切换前台 App 失败 %v", el(), err)
	}
	time.Sleep(1600 * time.Millisecond)
	s3 := record("A3-app-deactivated", "前台已让出，观察 key 是否保持")

	// 6. App 非激活状态下取 key —— 这是真实使用路径
	k4 := platformMakeKey()
	time.Sleep(700 * time.Millisecond)
	s4 := record("A4-key-while-inactive", fmt.Sprintf("非激活下 makeKey，key=%v", k4))

	// 7. 连续采样，看这个状态能不能稳定持住（真实使用中面板要开着若干秒）
	var stable []diag
	held := 0
	for i := 0; i < 6; i++ {
		time.Sleep(350 * time.Millisecond)
		d := record(fmt.Sprintf("A5-hold-%d", i+1), "稳定性采样")
		stable = append(stable, d)
		if d.WindowIsKey == 1 && d.FrontmostBundleID != d.AppBundleID {
			held++
		}
	}
	probeLog("     稳定性：%d/6 次采样中「有键盘焦点且未抢占前台」", held)

	verdict, findings := evaluate(s1, s2, s3, s4, stable)
	finish(mode, accessory, snaps, verdict, findings)
}

// 核心判据：抢不抢焦点不看 NSApp.isActive，而看 **系统前台应用有没有被换成本 App**。
// Accessory 策略下 App 可以 isActive=true 而 frontmost 仍是别的 App —— 菜单栏没被抢走。
func evaluate(s1, s2, s3, s4 diag, stable []diag) (string, []string) {
	var f []string

	held := 0
	for _, d := range stable {
		if d.WindowIsKey == 1 && d.FrontmostBundleID != d.AppBundleID {
			held++
		}
	}
	grabbed := 0
	for _, d := range stable {
		if d.FrontmostBundleID == d.AppBundleID {
			grabbed++
		}
	}

	f = append(f, fmt.Sprintf("① 冷启动显示            → key=%-5v 前台=%s",
		s1.WindowIsKey == 1, s1.FrontmostName))
	f = append(f, fmt.Sprintf("② activate 后取 key     → key=%-5v 前台=%s",
		s2.WindowIsKey == 1, s2.FrontmostName))
	f = append(f, fmt.Sprintf("③ 让出前台后            → key=%-5v 前台=%s",
		s3.WindowIsKey == 1, s3.FrontmostName))
	f = append(f, fmt.Sprintf("④ 非激活下取 key        → key=%-5v 前台=%s  ← 真实使用路径",
		s4.WindowIsKey == 1, s4.FrontmostName))
	f = append(f, fmt.Sprintf("⑤ 稳定性（6 次采样）    → 有焦点且未抢前台 %d/6；抢走前台 %d/6",
		held, grabbed))
	f = append(f, fmt.Sprintf("窗口类=%s（%d 字节），styleMask=%d，nonactivating 位=%v，activationPolicy accessory=%v",
		s4.WindowClass, s4.InstanceSize, s4.StyleMask, s4.NonactivatingBit == 1, s4.Accessory == 1))

	verdict := "FAIL"
	switch {
	case held >= 5 && grabbed == 0:
		verdict = "PASS"
		f = append(f, "✓ 面板稳定持有键盘焦点，且系统前台应用全程未被抢占 —— 免抢焦点成立")
	case held > 0:
		verdict = "PARTIAL"
		f = append(f, "△ 能拿到键盘焦点但状态不稳定（时有时无）")
	case s4.WindowIsKey == 1:
		verdict = "PARTIAL"
		f = append(f, "△ 能拿到 key 但前台被抢走，或未能持续")
	default:
		f = append(f, "✗ 面板拿不到键盘焦点 —— 免抢焦点不成立")
	}
	return verdict, f
}

func finish(mode int, accessory bool, snaps []snapshot, verdict string, findings []string) {
	rep := probeReport{
		Mode:      modeName(mode),
		Accessory: accessory,
		Verdict:   verdict,
		Findings:  findings,
		Snapshots: snaps,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	blob, _ := json.MarshalIndent(rep, "", "  ")

	out := os.Getenv("PAWCLIP_PROBE_OUT")
	if out == "" {
		out = fmt.Sprintf("/tmp/pawclip-probe-%s.json", modeName(mode))
	}
	_ = os.WriteFile(out, blob, 0o644)

	probeLog("")
	probeLog("═══ 结论：%s ═══", verdict)
	for _, l := range findings {
		probeLog("   %s", l)
	}
	probeLog("")
	probeLog("完整报告：%s", out)
	probeLog("")
}
