package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoUnlocalizedUserFacingErrors 把 §14 第 24 条里"错误提示"那一项
// 从"记得改"变成"改了才能提交"。
//
// # 它防的是什么
//
// 这个项目里有一条真实存在的裂缝：**能拿到翻译器的地方**（*App 的方法）
// 与**拿不到的地方**（包级函数：dialogs.go 的一部分、autostart*.go、
// reveal_*.go）混在一起。后者写错误信息时最自然的动作就是直接拼一句中文，
// 而那串中文会一路冒到界面上——于是英文界面里出现中文报错。
//
// 光靠"注意一下"是守不住的：这类字符串看起来像"系统调用的内部说明"，
// 不像 UI 文案。这个文件写完的当时就有 35 处。
//
// # 规则
//
// 主包（package main）的非测试文件里，`errors.New(...)` / `fmt.Errorf(...)`
// 的第一个参数若含中文，必须满足下面之一：
//
//	① 走 msgf(...) / msgErr —— 即根本不是字面量，本测试扫不到；
//	② 同一行或**前 4 行内**带 `// diag:` 标记，说明"这句是诊断信息，
//	   故意不翻译"，并在标记后写清为什么。
//
// 为什么允许 ②：确实有一批错误不该翻译。比如构造函数参数校验
// （`writer: 需要剪贴板后端`）——它在运行时不可能触发，翻译它等于
// 制造"用户看得懂但永远看不到"的文案；又比如启动期读配置失败，
// 它进的是日志。把这些也翻译掉会让目录里堆一批死键。
//
// 关键是**这个判断必须显式**：加一个 `// diag:` 是一次有意识的决定，
// 而不是"顺手写了句中文"。新加一句用户可见的报错而忘了走目录时，
// 这条测试会红。
func TestNoUnlocalizedUserFacingErrors(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}

	// 匹配 errors.New("...") / fmt.Errorf("...")，只取第一个字符串参数。
	callRe := regexp.MustCompile(`(?:errors\.New|fmt\.Errorf)\(\s*("(?:[^"\\]|\\.)*")`)
	hzRe := regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)

	// 允许 4 行：跨行的字面量（`fmt.Errorf("...%w",\n\tx)`）与两三行注释
	// 都不该让规则失效。
	const lookback = 4

	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		// i18n.go 是目录本身，messages.go 是接线层：这两个文件里的中文
		// 就是"翻译好的样子"，不是待翻译的漏网之鱼。
		if name == "i18n.go" || name == "messages.go" {
			continue
		}
		scanned++

		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			m := callRe.FindStringSubmatch(line)
			if m == nil || !hzRe.MatchString(m[1]) {
				continue
			}
			// 往前找 `// diag:` 标记。
			lo := i - lookback
			if lo < 0 {
				lo = 0
			}
			marked := false
			for j := i; j >= lo; j-- {
				if strings.Contains(lines[j], "// diag:") {
					marked = true
					break
				}
			}
			if !marked {
				t.Errorf("%s:%d 有一句写死的中文错误，但它既不在目录里、也没标 diag：\n"+
					"        %s\n"+
					"    用户会在界面上读到这句话。请二选一：\n"+
					"      - 走 msgf(msgKey, cause, args...)（用户能看到的东西都要翻译）\n"+
					"      - 如果是诊断信息/编程错误，就在上面加 `// diag: 为什么不该翻译`",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}

	// 防"这条测试因为扫不到文件而恒真"。
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个文件 —— 这条测试退化了（是不是改过目录结构？）", scanned)
	}
	t.Logf("扫了 %d 个主包文件", scanned)
}

// TestDiagMarkerIsStillNeeded 反向检查：标记不该变成"到处贴的免死金牌"。
//
// 一个 `// diag:` 标记只有在**它下面真的跟着一句中文错误**时才有意义。
// 如果某天有人删掉了那句错误却留下标记，标记会慢慢变成噪声，
// 而噪声里的真标记就没人看了。这条测试要求两者配对。
func TestDiagMarkerIsStillNeeded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	callRe := regexp.MustCompile(`(?:errors\.New|fmt\.Errorf)\(\s*("(?:[^"\\]|\\.)*")`)
	hzRe := regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "messages.go" {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "// diag:") {
				continue
			}
			// 标记之后 4 行内必须有一句中文错误，否则它已经过期了。
			ok := false
			hi := i + 4
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for j := i + 1; j <= hi; j++ {
				if m := callRe.FindStringSubmatch(lines[j]); m != nil && hzRe.MatchString(m[1]) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s:%d 的 `// diag:` 标记下面（4 行内）没有中文错误字面量 —— "+
					"标记与它豁免的那句话已经对不上了，请删掉这个标记",
					name, i+1)
			}
		}
	}
}
