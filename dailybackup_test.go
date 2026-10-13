package main

// dailybackup_test.go 钉住"每日备份"这条承诺里最容易悄悄失效的几件事。
//
// 这里最贵的一条是 TestDailyBackup_EndToEnd：它跑真实的 initialize（临时库、
// 真 blob 存储、真导出器），然后把写出来的包交给**导入侧的 Precheck**。
// 理由是这条链路上有一个纯代码看不出来的耦合：文件名由 backup.Export 生成
// （`pawclip-backup-%s-%s.clipbak`），而"最新一份"和"该清理谁"由本包的
// parseDailyBackupName 认领 —— 一旦那边改了命名规则，这边就再也认不出自己
// 写的文件，表现为**每次启动都重新备份一份**（"最新一份"永远是零值），
// 磁盘上日积月累，而所有单元测试都还是绿的。
//
// 第二条是"只删自己写的"：用户手动导出到同一个目录里的包必须活下来。
// 备份功能删掉用户的东西，是比不备份严重得多的错。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zego/pawclip/backup"
)

func TestParseDailyBackupName(t *testing.T) {
	// 固定用本地时区构造期望值：备份包名里的时间戳是 backup.Export 用
	// time.Now().Format 写出来的（本地时间），解析也必须按本地时间。
	want := time.Date(2026, 9, 16, 16, 43, 54, 0, time.Local)

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"标准命名", "pawclip-backup-daily-20260916-164354.clipbak", true},
		{"跨年边界", "pawclip-backup-daily-20260101-000000.clipbak", true},

		// 用户手动导出的包（scope=full）与我们无关：既不算"已有备份"，
		// 也绝不被清理删掉。这条如果反了，用户第一次用每日备份功能就会
		// 发现自己的手动导出被删了。
		{"手动导出的 full 包不认", "pawclip-backup-full-20260916-164354.clipbak", false},
		{"其它 scope", "pawclip-backup-pinned-20260916-164354.clipbak", false},

		{"前缀不对", "other-backup-daily-20260916-164354.clipbak", false},
		{"后缀不对", "pawclip-backup-daily-20260916-164354.zip", false},
		{"时间戳不合法", "pawclip-backup-daily-junk.clipbak", false},
		{"时间戳位数不对", "pawclip-backup-daily-20260916.clipbak", false},
		{"空时间戳", "pawclip-backup-daily-.clipbak", false},
		{"目录名那种", "pawclip-backup-daily-20260916-164354.clipbak.tmp", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, ok := parseDailyBackupName(c.in)
			if ok != c.ok {
				t.Fatalf("parseDailyBackupName(%q) ok=%t，期望 %t", c.in, ok, c.ok)
			}
			if !c.ok {
				return
			}
			if !at.Equal(want) && c.in != "pawclip-backup-daily-20260101-000000.clipbak" {
				t.Fatalf("parseDailyBackupName(%q) = %v，期望 %v", c.in, at, want)
			}
		})
	}
}

func TestDailyBackupDue(t *testing.T) {
	now := time.Date(2026, 9, 16, 16, 0, 0, 0, time.Local)

	cases := []struct {
		name   string
		newest time.Time
		want   bool
	}{
		{"还没有任何备份：该备份", time.Time{}, true},
		{"刚写 1 分钟前：不重复写", now.Add(-time.Minute), false},
		{"19 小时前：还不到", now.Add(-19 * time.Hour), false},
		{"正好 20 小时：到点（边界倾向于备份）", now.Add(-dailyBackupMinGap), true},
		{"21 小时前：该备份", now.Add(-21 * time.Hour), true},
		{"三天前（应用一直没开）：该备份", now.Add(-72 * time.Hour), true},
		// 时钟被往前调 / 从别的机器拷来一个"未来"的包：不备份。
		// 方向是安全的——宁可晚一天，也不要每次启动都写一份。
		{"时间戳在未来：不备份", now.Add(2 * time.Hour), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dailyBackupDue(c.newest, now); got != c.want {
				t.Fatalf("dailyBackupDue(%v, now) = %t，期望 %t", c.newest, got, c.want)
			}
		})
	}
}

func TestDailyBackupExcess(t *testing.T) {
	// 从新到旧（dailyBackupFiles 的排序契约）。
	files := []dailyBackupFile{
		{Name: "d-09"}, {Name: "d-08"}, {Name: "d-07"}, {Name: "d-06"},
		{Name: "d-05"}, {Name: "d-04"}, {Name: "d-03"}, {Name: "d-02"},
		{Name: "d-01"},
	}

	t.Run("多于保留数：删最旧的", func(t *testing.T) {
		got := dailyBackupExcess(files, 7)
		want := []string{"d-02", "d-01"}
		if len(got) != len(want) {
			t.Fatalf("dailyBackupExcess(9 份, keep=7) = %v，期望 %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("dailyBackupExcess(9 份, keep=7) = %v，期望 %v", got, want)
			}
		}
	})

	t.Run("正好等于保留数：不删", func(t *testing.T) {
		if got := dailyBackupExcess(files[:7], 7); got != nil {
			t.Fatalf("刚好 7 份时不该删，得到 %v", got)
		}
	})

	t.Run("少于保留数：不删", func(t *testing.T) {
		if got := dailyBackupExcess(files[:3], 7); got != nil {
			t.Fatalf("只有 3 份时不该删，得到 %v", got)
		}
	})

	// 一个坏参数绝不能变成"把备份删空"。
	t.Run("keep 为 0 或负数：一份都不删", func(t *testing.T) {
		if got := dailyBackupExcess(files, 0); got != nil {
			t.Fatalf("keep=0 时不该删，得到 %v", got)
		}
		if got := dailyBackupExcess(files, -3); got != nil {
			t.Fatalf("keep=-3 时不该删，得到 %v", got)
		}
	})
}

// TestPruneDailyBackups_OnlyTouchesItsOwn 是"删"这件事的安全带。
//
// 备份目录是用户也会用的地方（手动导出的默认落点就在数据目录下的
// exports/，但用户完全可能把包拷进 backups/ 一起收着）。清理必须只认
// 自己那种命名，别的一律不碰。
func TestPruneDailyBackups_OnlyTouchesItsOwn(t *testing.T) {
	dir := t.TempDir()

	daily := []string{
		"pawclip-backup-daily-20260908-090000.clipbak", // 最旧，该删
		"pawclip-backup-daily-20260909-090000.clipbak", // 次旧，该删
		"pawclip-backup-daily-20260910-090000.clipbak",
		"pawclip-backup-daily-20260911-090000.clipbak",
		"pawclip-backup-daily-20260912-090000.clipbak",
		"pawclip-backup-daily-20260913-090000.clipbak",
		"pawclip-backup-daily-20260914-090000.clipbak",
		"pawclip-backup-daily-20260915-090000.clipbak",
		"pawclip-backup-daily-20260916-090000.clipbak",
	}
	// 不该被碰的三个：用户手动导出的包、名字像但不是我们的、以及一个普通文件。
	bystanders := []string{
		"pawclip-backup-full-20260916-164354.clipbak",
		"pawclip-backup-daily-junk.clipbak",
		"notes.txt",
	}

	for _, n := range append(append([]string{}, daily...), bystanders...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("造文件 %s: %v", n, err)
		}
	}

	removed := pruneDailyBackups(dir, dailyBackupKeep)

	want := map[string]bool{
		"pawclip-backup-daily-20260908-090000.clipbak": true,
		"pawclip-backup-daily-20260909-090000.clipbak": true,
	}
	if len(removed) != len(want) {
		t.Fatalf("删了 %v，期望删 %v", removed, want)
	}
	for _, n := range removed {
		if !want[n] {
			t.Fatalf("删了不该删的 %q（期望只删 %v）", n, want)
		}
	}

	// 留下来的必须正好是：最新的 7 份每日备份 + 3 个旁观者。
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(left) != dailyBackupKeep+len(bystanders) {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("目录里剩 %d 个文件（%v），期望 %d 个", len(left), names, dailyBackupKeep+len(bystanders))
	}
	for _, n := range bystanders {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("旁观文件 %s 不该被删：%v", n, err)
		}
	}
	for _, n := range daily[:2] {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("最旧的两份该被删掉，%s 还在", n)
		}
	}
	for _, n := range daily[2:] {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("该留下的 %s 不见了：%v", n, err)
		}
	}
}

// TestDailyBackup_EndToEnd 跑真实链路：initialize（临时库 + blob 存储 +
// 真导出器）→ dailyBackupTick → 包真的落在 backups/ 里、名字认得出来、
// **能被导入侧的 Precheck 接受**，并且紧接着再 tick 一次不会重复写。
func TestDailyBackup_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	boot := DefaultBootstrap()
	boot.Database.Path = filepath.Join(dir, "paw.db")
	boot.UI.Language = "zh-CN"

	a := NewApp(boot, filepath.Join(dir, "config.toml"), discardLogger())
	// 测试进程里等不到 Wails 窗口（Attach 会白等 15s），并且这一段正好是
	// "不启动每日备份 goroutine"的那条路径——本用例手动调 tick，见下。
	a.noPlatformUI = true
	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(a.Shutdown)

	backupDir := a.dailyBackupDir()
	if backupDir == "" {
		t.Fatal("dailyBackupDir 返回空串（库应当已就绪）")
	}
	if want := filepath.Join(dir, dailyBackupDirName); backupDir != want {
		t.Fatalf("备份目录 = %q，期望 %q", backupDir, want)
	}

	// noPlatformUI 路径不会自己起 goroutine，所以这里手动跑一次 tick。
	a.dailyBackupTick(context.Background())

	files := dailyBackupFiles(backupDir)
	if len(files) != 1 {
		t.Fatalf("第一次 tick 后应有 1 份备份，实得 %d 份：%+v", len(files), files)
	}
	pkg := filepath.Join(backupDir, files[0].Name)
	if info, err := os.Stat(pkg); err != nil || info.Size() == 0 {
		t.Fatalf("备份包没写出来或为空：%v（%v）", err, info)
	}

	// 这一条钉住与 backup.Export 命名规则的耦合：文件是它生成的，
	// 名字必须能被我们自己认领（否则"最新一份"永远读不到 → 每次启动都重写）。
	if _, ok := parseDailyBackupName(files[0].Name); !ok {
		t.Fatalf("认不出自己刚写的备份包 %q —— 与 backup.Export 的命名规则脱钩了", files[0].Name)
	}

	// 最强的一条：这份自动备份必须真的能被导入路径吃回去。
	// Precheck 会把包结构、清单字段、blob 引用全过一遍。
	if _, err := backup.Precheck(context.Background(), a.db, pkg, backup.ImportOptions{}); err != nil {
		t.Fatalf("每日备份包过不了导入预检（等于备份不可用）：%v", err)
	}

	// 20h 最小间隔：紧接着再 tick 不该写第二份。
	a.dailyBackupTick(context.Background())
	if again := dailyBackupFiles(backupDir); len(again) != 1 {
		t.Fatalf("20h 间隔内重复备份了：现在有 %d 份", len(again))
	}
}
