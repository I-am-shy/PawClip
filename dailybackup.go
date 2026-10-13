package main

// 本文件落实 docs/DESIGN.md §7 第 9 条那句话：
//
//	"每日备份到 backups/ 保留 7 份"
//
// 为什么要有它 —— 这条承诺原本只写在文档里，代码一行都没有。2026-10-13 那次
// 草稿正文被清空的事故正好说明了它值多少：用户唯一能拿出来的"备份"是自己
// 在出事后手动导出的一份包，而那份包里的 drafts 是 0 —— 备份要在**出事之前**
// 就存在，才有意义。
//
// 与 §6 的"手动导出"共用同一个写入器（backup.Export）：包格式、含草稿、含
// blob、README 的生成规则全都一致，于是这份自动备份**能直接被「导入备份」
// 那条已经测过的路径吃回去**。自己另写一套"拷一份数据库文件"看起来更简单，
// 但它救不回图片（blobs/ 不在库里，而 GC 会把孤儿删掉），也没有导入路径认它。
//
// 五个刻意的选择（理由见各自的函数注释，dailybackup_test.go 有用例）：
//
//	静默          不发通知、不推事件：不是用户按下的动作，每天弹一次是噪音。
//	不拖慢启动    独立 goroutine，失败只记日志。
//	一天只写一份  判据是"最新一份是否够旧"，反复重启也不会写十份。
//	只删自己写的  认不出命名的文件一律不碰（用户手动导出的包要活下来）。
//	先写后清      写失败时一份都不删。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zego/pawclip/backup"
)

const (
	// dailyBackupDirName 是数据目录下的子目录名（§7 第 9 条的 backups/）。
	dailyBackupDirName = "backups"

	// dailyBackupKeep 是保留份数（§7 第 9 条）。
	dailyBackupKeep = 7

	// dailyBackupMinGap 是两份之间必须隔开的时间。
	//
	// 不写成 24h 是**故意的**：应用不是每天在同一时刻被打开，24h 的硬间隔会让
	// "昨天 21:00 开过、今天 20:30 又开"这种最常见的情形一次都不备份（差 30
	// 分钟不够），隔天再补上就已经漏了一整天。20h 让"每天晚一点"的节奏稳定地
	// 一天一份，同时仍然挡住当天重复启动。
	dailyBackupMinGap = 20 * time.Hour

	// dailyBackupCheckEvery 是运行期间的复查间隔（启动时查过一次之后）。
	//
	// 一小时查一次只是一个文件名列表的读取，成本可以忽略；它保证"应用一直
	// 开着"的用户在跨过 20h 界限后的一个小时内拿到当天的备份，而不是等到
	// 下次启动。
	dailyBackupCheckEvery = time.Hour

	// dailyBackupPrefix / dailyBackupSuffix / dailyBackupStampLayout 必须与
	// backup.Export 生成文件名的规则逐字对齐（backup/writer.go 里那句
	// `pawclip-backup-%s-%s.clipbak` 与 `start.Format("20060102-150405")`）：
	// 认不出自己写的文件，清理与"最新一份"的判断就全失效（表现为每次启动都
	// 重写一份，而单元测试全绿）。端到端用例专门钉这条耦合。
	dailyBackupPrefix      = "pawclip-backup-daily-"
	dailyBackupSuffix      = ".clipbak"
	dailyBackupStampLayout = "20060102-150405"
	dailyBackupScopeLabel  = "daily"
)

// dailyBackupFile 是目录里一份可识别的每日备份。
type dailyBackupFile struct {
	Name string
	At   time.Time
	Size int64
}

// parseDailyBackupName 认领一个文件名：只有我们自己那种命名才算数。
//
// 时间戳按**本地时区**解析 —— backup.Export 用的是 time.Now().Format，同一个
// 进程、同一个时区，往返一致。认不出来的返回 false，调用方一律当作"不是我们
// 的文件"（既不参与清理，也不参与"最新一份"的判断）。
func parseDailyBackupName(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, dailyBackupPrefix) || !strings.HasSuffix(name, dailyBackupSuffix) {
		return time.Time{}, false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, dailyBackupPrefix), dailyBackupSuffix)
	at, err := time.ParseInLocation(dailyBackupStampLayout, stamp, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// dailyBackupFiles 列出 dir 下的每日备份，按时间**从新到旧**。
//
// 目录不存在、读不了都返回 nil：这不是错误，只代表"还没有备份"。时间戳相同时
// 按文件名倒序，保证顺序稳定（同一秒里写出两份时才用得上）。
func dailyBackupFiles(dir string) []dailyBackupFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]dailyBackupFile, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		at, ok := parseDailyBackupName(e.Name())
		if !ok {
			continue
		}
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, dailyBackupFile{Name: e.Name(), At: at, Size: size})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Name > out[j].Name
		}
		return out[i].At.After(out[j].At)
	})
	return out
}

// dailyBackupDue 判断此刻该不该写一份：没有备份、或最新一份已经够旧。
//
// "够旧"用 >= 而不是 >：正好卡在 20h 这个边界上时倾向于**备份**。时钟被往前调
// （时区/夏令时/用户手动改）会让 newest 落在未来，此时 newest.Add(gap) 也在未来，
// 于是不备份 —— 这是安全的方向（不重复写）。
func dailyBackupDue(newest, now time.Time) bool {
	if newest.IsZero() {
		return true
	}
	return !now.Before(newest.Add(dailyBackupMinGap))
}

// dailyBackupExcess 返回 files 里该删掉的那些（纯函数，见 pruneDailyBackups）。
//
// keep < 1 时返回 nil：一个坏参数绝不能变成"把备份删空"。
func dailyBackupExcess(files []dailyBackupFile, keep int) []string {
	if keep < 1 || len(files) <= keep {
		return nil
	}
	out := make([]string, 0, len(files)-keep)
	for _, f := range files[keep:] {
		out = append(out, f.Name)
	}
	return out
}

// pruneDailyBackups 只保留最新 keep 份，返回真正删掉的文件名。
//
// 删除失败（被占用、权限）只跳过：清理是维护动作，不该让调用方以为"备份出
// 问题了"。也因此返回的是**真的删掉了**的那些名字。
func pruneDailyBackups(dir string, keep int) []string {
	excess := dailyBackupExcess(dailyBackupFiles(dir), keep)
	if len(excess) == 0 {
		return nil
	}
	removed := make([]string, 0, len(excess))
	for _, name := range excess {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			continue
		}
		removed = append(removed, name)
	}
	return removed
}

// startDailyBackup 起一个后台 goroutine：启动时查一次，之后每小时复查。
//
// 调用点在 initialize 的**非测试路径**上（与 attachPanel、startIdleWatcher 同一
// 段）：noPlatformUI 的测试进程不该往临时目录里写备份包。
func (a *App) startDailyBackup(ctx context.Context) {
	go func() {
		// 先查一次再进 ticker：应用一天只被打开一次的话，这一次就是全部机会
		// （等到一小时后才查，那时的进程可能已经退出了）。
		a.dailyBackupTick(ctx)

		t := time.NewTicker(dailyBackupCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.dailyBackupTick(ctx)
			}
		}
	}()
}

// dailyBackupDir 返回备份目录（数据目录下的 backups/）；拿不到返回空串。
func (a *App) dailyBackupDir() string {
	db, _, _, err := a.ready()
	if err != nil {
		return ""
	}
	return filepath.Join(db.Dir(), dailyBackupDirName)
}

// dailyBackupTick 是一次"该不该备份"的完整判断，必要时写一份并清理旧的。
//
// 所有失败都只是日志：这个 goroutine 的存在不该让任何东西变脆。
func (a *App) dailyBackupTick(ctx context.Context) {
	dir := a.dailyBackupDir()
	if dir == "" {
		// 库还没就绪或初始化失败。不是错误，只是这次查不了 —— 下次复查再说。
		return
	}

	files := dailyBackupFiles(dir)
	var newest time.Time
	if len(files) > 0 {
		newest = files[0].At
	}
	if !dailyBackupDue(newest, time.Now()) {
		return
	}

	path, err := a.writeDailyBackup(ctx)
	if err != nil {
		// 备份失败不打扰用户：它不是用户按下的动作，界面上弹一条"备份失败"
		// 只会让人以为数据出事了。真出事时靠这一行日志取证。
		a.log.Warn("每日备份失败（不影响记录与使用）", "err", err)
		return
	}

	// 写完才清理（见文件头"先写后清"）。
	a.log.Info("已写入每日备份",
		"path", path,
		"dir", dir,
		"keep", dailyBackupKeep,
		"existing", len(files)+1,
	)
	if removed := pruneDailyBackups(dir, dailyBackupKeep); len(removed) > 0 {
		a.log.Info("已清理过期的每日备份", "removed", removed, "keep", dailyBackupKeep)
	}
}

// writeDailyBackup 写一份完整备份包，返回它的路径。
//
// scope 用 full：§6 里"只有 scope = full 才带草稿"，而草稿正是这次事故里丢掉
// 的东西 —— 少带草稿的备份救不回同类问题。
func (a *App) writeDailyBackup(ctx context.Context) (string, error) {
	db, _, _, err := a.ready()
	if err != nil {
		return "", err
	}
	blobs := a.blobStore()
	if blobs == nil {
		return "", errors.New(a.T(msgErrNoBlobs))
	}
	dir := filepath.Join(db.Dir(), dailyBackupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	res, err := backup.Export(ctx, db, blobs, backup.ExportOptions{
		Scope:          backup.ScopeFull,
		ScopeLabel:     dailyBackupScopeLabel,
		ManifestFormat: "json",
		OutputDir:      dir,
		AppVersion:     Version,
		Platform:       runtimeGOOS(),
		Preamble:       a.readmePreamble(),
	}, a.gcEngine(), a.log)
	if err != nil {
		// backup.Export 自己会把写了一半的包删掉，这里不必再收拾。
		return "", err
	}
	return res.Path, nil
}
