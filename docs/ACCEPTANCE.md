# PawClip 验收报告（`docs/DESIGN.md` §12 全项）

> 这份报告回答一个问题：**§12 列的指标，到底达标没有。**
> 达不到的如实标 ⚠️ 并给出归因，不粉饰；有东西坏了才标 ❌。

实测环境：macOS / Apple Silicon，2026-09-16（首轮）· 2026-09-21（M5 草稿本补测）。
单测口径 `test/run.sh`（= `go test -tags sqlite_fts5 -p 1 -count=1 ./...`）；
真机口径 `test/accept.sh`（对 `build/bin/pawclip.app` 实跑，真剪贴板、真磁盘、真 SIGKILL）。
当前规模：**301 条用例 / 9 个包全绿**（顶层测试函数的计数；按 `--- PASS` 连子测试一起数是 435 条。另有一个无测试的工具包 `tools/genpinyin`）。

---

## 指标结果

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 捕获延迟（文本） | < 30 ms | 流水线 p95 **1.2 ms**（端到端含 50ms 合批窗口约 48 ms） | `capture` `TestCaptureLatencyText` | ✅ |
| 捕获延迟（5 MB 图片） | < 200 ms | 1600×1200 噪声 PNG（5.8 MB）端到端中位数 **108 ms** | `capture` `TestCaptureLatencyImageAt5MB` | ✅ |
| 检索延迟（≥3 字） | < 50 ms @ 10 万条 | 3 字中文 p95 **2.5 ms**；4 字 **4.9 ms**；英文 **4.5 ms**；中英混排 **4.4 ms** | `store` `TestSearchLatencyAt100k` | ✅ |
| 检索延迟（2 字 LIKE） | < 300 ms | p95 **0.67 ms**（中/英） | 同上 | ✅ |
| 空闲内存 | macOS ≤ 30 MB（面板销毁态） | 稳态 **60–73 MB**（同日对照实测：上一版 `v0.1.0` 60/64/65 MB、当前版 65/66/67/73 MB——**同一二进制跨上下文就会摆动**，差异在 WebKit 侧，不是版本差异）；`wails.Run` 之前基线 13 MB | `test/accept.sh` ③（进程自报快照） | ⚠️ 见下 |
| 空闲 CPU | macOS < 1% | **0.33–0.35%**（20 秒窗口，已排除启动开销） | 同上 | ✅ |
| 无自捕获 | 连续 100 次回贴 → 新增 0 | 100 次回贴后 `CountAlive` 仍为 1，守卫拦下 100 次 | `capture` `TestAcceptance1_NoSelfCapture` | ✅ |
| 去重正确性 | 复制 100 次 → 1 行、`use_count=100` | 真机 `pbcopy` 100 次 → 1 行、`use_count=100` | `capture` `TestAcceptance2_DedupeUseCount` + `accept.sh` ⑥ | ✅ |
| 搜索正确性 | 1/2/3 字、英文、混排、大小写、特殊字符 | 全部命中符合预期（含 2 字走 LIKE 的分支） | `store` `TestSearch_CorrectnessTable` / `TestSearchCorrectnessAtTwoChars` | ✅ |
| 导出完整性 | 导出→清库→导入，全字段一致 | 条目/内容/分类/标签/置顶/过期时间逐项比对一致 | `backup` `TestAcceptance1_ExportThenImportPreservesEverything` | ✅ |
| 导入幂等 | 同包导入两次，第二次全跳过 | 第二次全部走跳过，库中无重复 | `backup` `TestAcceptance2_ImportTwiceIsIdempotent` | ✅ |
| 断电安全 | 捕获中强杀，重启后可开库且无半截记录 | 真机 SIGKILL → 重启走「标记 → `integrity_check=ok`」→ 无悬空 blob | `capture` `TestAcceptance3_Kill9Recovery` + `accept.sh` ⑦ | ✅ |
| （附）前端体积 | §14 第 14 条：gzip 后 < 150 KB | gzip **84.6 KB**（JS 79.7 + CSS 4.9）；余量约 65 KB | `npm run build` 产物实测 | ✅ |

### M5 草稿本（2026-09-21 新增）

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 草稿往返 | 导出→清库→导入后逐字段一致，且**图片真的在 `blobs/` 里** | 标题/正文/创建时间/修改时间逐字段一致；包内引用已改写成 `blobs/…`、库内改回 `blob/…`；导入后 `os.Stat` 贴图命中 | `backup` `TestAcceptance3_DraftsRoundTrip` | ✅ |
| 草稿回滚 | 一键回滚带走那批草稿，本机草稿不受影响 | 回滚后该批次草稿数为 0；归档草稿与 `import_id IS NULL` 的草稿不在删除面内 | 同上（测试后半段）+ `store` `TestDeleteImportItems_AlsoRemovesDrafts` / `TestPurgeDraft_DetachesFromImport` | ✅ |
| 草稿图片存活 | 贴图跨过 24 h 门槛跑 3 轮 GC 仍在 | 3 轮 GC 后贴图仍在；另钉住「库里只有草稿、`items` 为空」时孤儿扫描**仍然执行** | `retention` `TestGC_DraftImagesSurviveOrphanSweep` / `TestGC_DraftOnlyLibrarySweepsOrphans` | ✅ |
| 草稿归档到期回收 | 归档草稿超 `draft.archiveTtlSec` 后被硬删 | 超期归档草稿被收走并计入 `Report.DraftsPurged` / `DraftsPurgedChars` | `retention` `TestGC_PurgesArchivedDrafts` | ✅ |
| 归档保留期独立 | 回收站保留期调短不牵连草稿归档（反之亦然） | 回收站 1 h、草稿 30 天：超期回收站条目被删、刚归档草稿保留（两方向都断言） | `retention` `TestGC_DraftArchiveTtlIsIndependentOfTrashTtl` | ✅ |
| 草稿目录规模 | 200 条时目录滚动流畅（列表不读正文全文） | **未做真机滚动取证**；纪律侧已钉住：`ListDrafts` 的 SQL 只取 `substr(md,1,120)` 摘要，不 SELECT 全文 | `store` `TestListDrafts_DoesNotLoadFullBody` | ⚠️ 见下 |
| 草稿保存延迟 | 停手 ≤ 800 ms 落盘；持续打字 5 s 内必落一次 | **未做量化取证**（前端防抖 500 ms + 5 s 强制落盘，代码路径存在但无计时用例） | — | ⚠️ 见下 |
| 草稿断电安全 | 编辑中强杀，重启后为最后一次落盘内容且库可开 | **未做真机取证**（沿用条目侧同一条 `capture` 用例的结论外推，不构成独立证据） | — | ⚠️ 见下 |
| 草稿图片缩放 | 拖出来的宽度存进 md、重开一致；非法宽度不写 | 宽度方言与往返由 `test/check-md.mjs` 钉住；**拖拽交互本身未做自动取证** | `test/check-md.mjs` | ⚠️ 见下 |
| 草稿链接可打开 | 点正文链接用系统程序打开，且 WebView 不导航 | 协议白名单与被拒时"一步都不往外走"有 Go 单测；**点击通路未做自动取证** | `dialogs_test.go` `TestOpenableURL` / `TestOpenURL_RefusesBeforeReachingTheSystem` | ⚠️ 见下 |
| 破坏性动作确认 | 四处不可恢复动作（草稿彻底删除 / 回收站彻底删除 / 清空回收站 / 撤销导入）执行前都弹确认框；弹窗开着时面板的键盘快捷键失效 | **未做真机取证**；纪律侧已钉住：弹窗本体只此一处实现（`components/ConfirmModal.tsx`），四处调用全部走它；`frontend/src` 里已无 `window.confirm` 调用 | 人工检视（无自动用例） | ⚠️ 见下 |

草稿本的**格式与不变量**在单测层面是齐的（`test/check-md.mjs` 96 条断言钉住
md ⇄ HTML 往返、转义契约、图片宽度方言、外链白名单与裸 URL 识别规则；`store`/`backup`/`retention` 共
21 条草稿相关用例）。
上表六条 ⚠️ 是**缺真机取证**，不是发现缺陷——它们都需要在真实面板里手动操作
（滚动、计时、强杀、拖拽、点链接、点确认框），`accept.sh` 目前不驱动 UI，
所以自动验收覆盖不到。

**另记一条已修缺陷**（不是遗留）：Wails v2.16 声明了 `WKUIDelegate` 的遵循关系
却**没有实现任何 `runJavaScript*Panel` 方法**，因此 `window.confirm` 在打包产物
里不出现——"清空回收站"与"撤销上次导入"这两处的
`if (!window.confirm(...)) return` **按下去什么都不会发生**（不是提示的样子不对，
是那行之后的动作从未被执行）。已统一改为自绘模态框（§0.6）。这条同样只能靠
源码检视 + 手测取证，没有自动化用例——`accept.sh` 不驱动 UI。

**另记一条 harness 假红**（不是产品缺陷，2026-09-21 发 v0.1.1 前发现并修掉）：
从自动化环境里跑 `--accept`（`LC_CTYPE=C`）时，④ 文本捕获与 ⑥ 去重双双失败，
而 ⑤ 的图片全绿——**"文字全红、图片全绿"这个分裂就是线索**（图片走 `osascript`，
绕开了 `pbcopy`）。真因是 `pbcopy` 按 `LC_CTYPE` 解释 stdin：locale 不是 UTF-8 时
它对非 ASCII **静默写入空字符串**（剪贴板类型还在、长度是 0），于是应用"什么都没
捕获到"，报出来的样子与捕获故障一模一样。`test/accept.sh` 现在自己钉死
`LC_ALL=en_US.UTF-8`（`PAWCLIP_ACCEPT_LOCALE` 可覆盖），并在 ④ 加了"我们到底
写进去了没有"的前置断言，把这类环境失败与真正的捕获故障在输出里分开。
修完实跑：**21 项（20 ✅ / 1 ⚠️ / 0 ❌）**。

`test/accept.sh` 共 21 项，**20 通过 / 1 已知差距 / 0 失败**——已知差距即上表的「空闲内存」。
退出码只跟真正的失败走，理由见下一节。

### 每日备份（2026-10-13 新增）

§7 第 9 条原先只有文档承诺、没有实现（见「产物级问题」第 12 条）。现在它落成
`dailybackup.go`：启动时查一次、之后每小时复查，距上一份满 **20 小时**就写一份
`backups/pawclip-backup-daily-<YYYYMMDD-HHMMSS>.clipbak`（`scope = full`，含草稿
与 blob），只保留最新 **7** 份；全程静默，只在日志里留一行。

| 项 | 判据 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 备份可用 | 写出来的包能被**导入侧**接受 | `backup.Precheck` 通过（真实 `initialize` + 真实导出器） | 根包 `TestDailyBackup_EndToEnd` | ✅ |
| 一天一份 | 20 小时内不重复写 | 第一次 tick 写 1 份，紧接着再 tick 仍是 1 份 | 同上 + `TestDailyBackupDue`（19h / 20h / 21h / 未来时间戳四个边界） | ✅ |
| 只留 7 份 | 多出来的只删自己那种命名 | 9 份 → 删最旧 2 份；`pawclip-backup-full-…` 与 `notes.txt` 一个不动 | `TestPruneDailyBackups_OnlyTouchesItsOwn`、`TestDailyBackupExcess` | ✅ |
| 命名耦合 | 自己写出的文件必须认得出来（认不出就会每次启动都重写一份） | 端到端用例对**真实产物**跑 `parseDailyBackupName` | `TestDailyBackup_EndToEnd` | ✅ |
| 真机节奏 | 隔一天真的写一份，日志里出现"已写入每日备份" | **未做真机取证**：20h 的墙钟跨度等不了 | 人工：正常用一天后看数据目录下的 `backups/` | ⚠️ 见下 |

---

## 唯一未达标项：空闲内存 ~60–73 MB

数字本身是真的，归因也是确定的：

- `wails.Run` **之前**进程只有 **13 MB**；
- 把窗口与 WebView 建出来之后就是 **60–73 MB**（同日对照实测：上一版 `v0.1.0`
  60/64/65 MB、当前版 65/66/67/73 MB）。

也就是说超出的部分**全在 Wails/WebKit 侧**，Go + SQLite + 捕获链路那部分只占十几 MB。

> ⚠️ 这个数**同一份二进制跨上下文就会摆动**（60–73 MB）：它取决于 WebKit 当刻的
> 状态，而不是版本差异——上面的对照实测（把上一版重新构建出来、与当前版交替各跑
> 三轮）就是为了证明这一点。所以看到单次数字涨了先别急着归因到改动上，先做对照。

而 §12 的判据写的是「**面板销毁态**」：它默认面板闲置时会被销毁、把 WebView 一起还回去。
本实现做不到这件事，因为 **Wails v2.16 的单窗口模型只导出 `Show/Hide/Quit`，没有窗口
销毁与重建 API**（销毁了就没法再显示，那比内存超标更糟）。所以当前实现改成"按空闲阈值
收起面板"，并把这个事实与实测数字摆在 `PanelLifecycle()` 里（见 §13 已列风险、§14 第 10 条）。

要真正收进 30 MB，需要换掉 Wails 的单窗口模型（改成启动时不建窗口、首次呼出时才创建，
或改用能销毁/重建窗口的方案）。那是一次架构改动，不属于本期打磨范围。

> 脚本对这条的处理是**两段判据**（`test/accept.sh` ③）：
> ≤ 30 MB 记 ✅ 达标；31–90 MB 记 ⚠️ **已知差距**（有归因、记录在案，**不影响退出码**）；
> > 90 MB 记 ❌ 失败。
>
> 后半段是刻意留的**回归线**，取值改过一次（80 → 90）：实测摆动区间已经到 73 MB，
> 80 只比它高 9%，一次正常抖动就会假红；而回归线要抓的是"翻倍"级退化，不是 10%。
> 之所以不让它一直算失败——**一条永远红的判据会让人学会无视整张表**，
> 那它就再也不报警了。判据本身没有下调，30 MB 这个目标值仍然写在明面上。

---

## 验收过程中抓出的产物级问题

这些问题**一条单测都不会红**——前 6 条的共同点是只有"真跑一次产物"才会暴露，
第 7 条更靠外一层（连产物验收脚本也不驱动到的界面）；列在这里是为了说明
`test/accept.sh` 为什么必须存在，以及它存在之外还剩什么要人看。

| # | 问题 | 为什么单测抓不到 |
|---|---|---|
| 1 | App 带 `LSUIElement` 启动时进程已是 Accessory，`setActivationPolicy:Accessory` 返回 NO 的含义是"没变化"而非失败，却每次启动固定打一条 WARN | WARN 只出现在真机日志里，而它恰在 §0.2「免抢焦点必要条件」上，会把排查的人带偏 |
| 2 | CI 里 `go build ./... -tags sqlite_fts5` 的标签写在包路径之后，被当成包路径解析，这个 job 从未成功过 | `go test` 对包后参数有兼容处理，照抄测试命令就得到一条永远不会红的 CI 步骤 |
| 3 | 版本号两份不一致：`Info.plist` 说 0.1.0、程序自报 0.1.0-m1 | 没有任何用例读版本号；改为从 `wails.json` 单一真源经 `-ldflags` 注入 |
| 4 | 验收脚本自身的 flake：固定 `sleep 0.3` 复制 100 次，第二遍跑出 99（撞上 200 ms 轮询相位） | 是**测量方式**问题，不是去重语义问题；改成"等 `use_count` 涨到预期值再放下一次" |
| 5 | `.gitignore` 漏了 `build/dist/`，本地打包后多出一个 10 MB 未跟踪文件 | 不影响构建与测试，但很容易被 `git add -A` 带进仓库 |
| 6 | 验收脚本在非 UTF-8 locale 下 ④⑥ 假红：`pbcopy` 对非 ASCII **静默写入空字符串**，报出来的样子与"捕获坏了"完全一样（详见上一节） | 是**测量环境**问题，不是产品问题；脚本已自行钉死 `LC_ALL=en_US.UTF-8`，并在 ④ 加了"写进去了没有"的前置断言把两类失败分开 |
| 7 | 导入确认页「图片实际占用」显示的是**剩余磁盘空间**（121.9 GB），同一页的体积行还和数量行**同名**都叫「包内条目」——是标签错配，数值本身没错（0.1.3 修） | 比上面 6 条更靠外一层：**连 `accept.sh` 也抓不到**——它不驱动 UI，而 `check-i18n.mjs` 只校验键的两份字典一致且被引用，管不了"标签的语义是否等于所填值的语义"。这一条是**人眼读界面**发现的 |
| 8 | 草稿本"切走再切回、正文变空、再切走就**把库里的正文写成空**"（用户报障，2026-10-13）。根因：编辑器只在 `selectedId != null` 时挂载，而 `openDraft` 是先把 `innerHTML` 写进 `nodeRef.current`、再 `setState` ——首次进入编辑器那一刻 `nodeRef.current` 还是 `null`，内容被丢进虚空；下一次卸载时的 `flush({force:true})` 于是把 `""` 落库 | 前端**没有任何组件测试**（仓库口径：不引入 jsdom/RTL），而这条要的是"写→切走→切回→再切走"的**时序**，任何单帧快照都看不出来。定位靠把真组件挂进一个**仓库外**的 jsdom 台架重放三步，并在三步后读库（`1:"<正文>"`）；顺带发现即使读回失败也绝不能被当成"用户清空了正文"，所以补了 `loadedIdRef` 守卫 |
| 9 | **Windows 面板一呼出就"坍缩"**（用户报障）：`swShowNoActivate = 6` 写的是 `SW_SHOWNOACTIVATE`，而 Win32 的 `6` 是 **`SW_MINIMIZE`** —— 每次 `Show()` 都等于"把面板最小化"，于是一圈死边框 + 尺寸看着不对 + 边缘抓不住 | `panel/windows.go` 带 `//go:build windows`，开发机（macOS）上**根本不参与编译**，`go vet` / `go test` 也只看宿主平台；`test/run.sh` 第 ⑤ 层只证明它**编得过**。更隐蔽的是文件里那句注释写的就是它想表达的意思，读代码的人会顺着注释相信常量。修法：改用 `windows.SW_SHOWNOACTIVATE`（让常量由 x/sys 定义），并把该文件里其余约 40 个手写常量逐个对照 MSDN 复核 |
| 10 | "呼出时再把几何断言一次"这条**恢复路径在默认配置下从未生效**：Win32 只在尺寸**真的变了**时才发 `WM_SIZE`，而 Wails 只在 `WM_SIZE` 里刷新 WebView2 的视口——尺寸早在 `Attach` 里就是目标值，于是那次 `SetWindowPos` 一个消息都不发。要兜的是 Wails `setupChromium` 与 `OnSize` 绑定之间那几十微秒的缝（`Attach` 是 50ms 轮询找窗口的，可能正好落在里面）：缝里改的尺寸没人接，WebView2 就停在 Embed 时的视口上 | 失效的样子**极其安静**：代码执行了、日志也有，只有真机上"UI 缩在窗口一角"才看得出来；而 Wails 的边缘缩放热区是在 WebView2 里按**视口**算的（`window.outerWidth - e.clientX < 6`），视口比窗口小时右/下边缘在数学上不可达——"边缘拖不动、放大范围小"与它是同一个根因。修法：`syncPanelSize(force=true)` 用"退 1px 再设回"逼出两次真实 `WM_SIZE`，并把"该不该动手"这个唯一能脱离 Windows 验证的判断抽成纯函数 `PlanPanelSize`（`panel/geometry_test.go` 有用例） |
| 11 | 面板尺寸的**单位不一致**：`Size()` 读的是客户区，而 `applyPanelSize` 设的是窗口矩形，两者被 Wails 的无边框 `WM_NCCALCSIZE` 差着 1px（`rgrc.Bottom += 1`）。后果有两个，都不是"差 1px"那么轻：① 落库值每显隐一轮 +1，面板一路长到 1100 的上限；② `Show` 里"尺寸和我要的不一样 = 用户拖过"的反推（`placed`）被这 1px 顶成**恒真** —— 面板从第二次呼出起不再回到鼠标所在显示器 | 1px 在肉眼与截图里都不可见，单测又只能看到自己那一侧的单位；这一条是把两侧的**取值来源**（`GetClientRect` vs `GetWindowRect`）并排读才发现的。修法：几何统一以**窗口矩形**为准（`windowPoints`），这也正好与 mac 侧读 `NSPanel.frame` 对齐；`Diag()` 同时报 `live*`（窗口）与 `view*`（客户区），"UI 缩在窗口一角"从此有第一手数字 |
| 12 | `docs/DESIGN.md` §7 第 9 条写着"每日备份到 `backups/` 保留 7 份"，而**代码一行都没有**（全仓只有一处 i18n 文案里出现过 backups 这个词）。出事时用户手里唯一的"备份"是事后手动导出的包，其中 `drafts` 是 0 | **没有任何测试会去读文档**，文档承诺与实现之间没有判据——它甚至撑过了好几轮验收。修法：实现它（`dailybackup.go`，复用 `backup.Export` 写 `scope = full` 的包，20h 最小间隔、保留 7 份、静默、只删自己写的），并把"能被导入侧 `Precheck` 接受"写成断言——**备份不可导入等于没有备份** |
| 13 | **验收脚本自身在中文 locale 下崩掉**：`test/run.sh` 的失败汇总写的是 `"$name（退出码 $rc）"`，而 macOS 自带 bash 3.2 在 `LANG=zh_CN.UTF-8` 下会把紧跟变量的多字节字符吞进变量名（实测报 `name: unbound variable`），叠加 `set -u` 后**整个汇总表直接死掉** —— 表现是"跑了 5 层、什么都没说、退出码 1"，任何一层红都看不见。同类写法在 `accept.sh`（13 处）与 `demo-m1.sh`（1 处）也在 | CI 是 Linux + bash 5 + `C.UTF-8`，复现不了；而它在开发机上只在**某一层真的失败时**才触发——也就是说它专挑"需要它报告失败"的时刻失灵。修法：变量一律加花括号（`${name}（…）`），并用"把 PATH 抽掉让 4 层必红"的方式验证汇总表这一次真的打得出来 |
| 14 | 草稿时间列的语义被"查看"污染：**点开一篇草稿看一眼，它的时间就变成"刚刚"**（用户报障，2026-10-13）。两层根因：① 前端切草稿时会先 `flush({force:true})` 把上一篇结算一次，而 `force` 的旧含义正是"跳过『内容没变就不写』的短路"；② `store.SaveDraftMD` / `RenameDraft` 是**无条件 `UPDATE`**，写回同样的字节也会把 `updated_at` 顶成 now | 单测抓不到，因为不变量被**推给了调用方**：`SaveDraftMD` 的注释当时明确写着"不判断『内容是否变化』——那由调用方比对后决定（见前端 useDraftAutosave）"，而调用方那条比对恰好被 `force` 绕开。两边都以为对方在管，于是没有任何一层会红——它只在真机上表现为一个时间文案。修法是把不变量放回**库那一侧**：比对进 SQL 的 WHERE（`AND md <> ?`），一致时零写入并把原来的 `updated_at` 原样返回。连带发现"0 行"有两种含义，必须再查一次才能分开"没变化"与"草稿已被删除"（否则要么误报删除，要么把删除吞成"保存成功"）——两条都有用例钉住 |
| 15 | 草稿本侧边栏的开合**记不住，而且方向是反的**：收起之后重启还是展开、展开过的重启反而收起（用户报障，2026-10-13）。根因是键名与内容相反——设置键叫 `ui.draftTocCollapsed`（"收起"），默认值与读取端（`setTocOpen(!tocCollapsed)`）都按"收起"解释，只有写入端（`SetSetting(…, String(nv))`，nv 是**展开**状态）按"展开"写 | 两处代码**各自都自洽**：写入端"写我此刻的状态"、读取端"取反就是展开"，只看自己那半边都不会觉得有问题；而它们之间隔着一次 IPC 与一次 JSON 序列化，没有任何编译期约束要求两边同极性。前端又没有组件测试（仓库口径），Go 侧只看到一个 `bool`。修法不是在一处补取反——那只是让两个错误互相抵消，下一个读这个键的人照样踩——而是**把键改成它真正的含义**：`ui.draftTocOpen`（默认 `true` = 展开，正是"默认打开侧边栏"），读写两侧同极性。旧键只在**新键缺席**时被沿用一次，且**值原样搬、不取反**（旧值本来就是展开状态），于是老用户上次的姿态得以保留，而不是被默认值覆盖 |

> 第 7 条值得单独拎出来说：它的根因是**确认页没有自己的体积键，就复用了统计页的**
> （`stats.diskBytes` / `stats.aliveBytes` / `backup.total`），词义随之跑偏。
> 三个键在各自正确的页面仍被引用，所以 i18n 检查一路绿。
> 现在三个键补齐了语义正确的替代（`backup.uncompressedBytes` / `backup.needBytes` /
> `backup.availableDisk`），并在字典里写了"不要再复用 `stats.*`"的注释。
>
> **这类问题的自动判据目前不存在，也不必硬造**：把"文案语义"变成断言要么写死中文串
> （一改文案就假红），要么引入人评。按仓库一贯口径，界面文案属真机手测（见「已知遗留」）。
> 列在这里的意义是给后来者一个坐标——**绿的不等于对的**。

另外补齐了 §12 里原先**一条测试都没有**的「捕获延迟」两项（`capture/latency_test.go`）。

---

## 已知遗留

| 项 | 现状 | 影响 |
|---|---|---|
| 空闲常驻内存 60–73 MB | 面板闲置销毁做不了（Wails v2.16 单窗口无重建 API） | §12 唯一未达标项；面板收起后内存仍在 |
| Windows 真机未验证 | 只有 `CGO_ENABLED=0` 交叉编译烟测 + `windows-2022` 上的正式构建 | 只能证明"能编过"；剪贴板 / 热键 / 托盘，以及**面板的尺寸 / 定位 / 缩放 / 尺寸边界**（0.1.2 新做的，见 `docs/DESIGN.md` §8 第 11 条）都未在真机测过。面板几何里**只有纯计算那一半**（顶部 12% 留白、水平居中、多显示器负原点、上下边界夹取）有单测覆盖（`panel/geometry_test.go`，做过变异检查），系统调用那一半（`MonitorFromPoint` / `GetMonitorInfoW` / `SetWindowPos` / `WM_GETMINMAXINFO` / 抢到前台后键盘是否真进 WebView2）没有 |
| 辅助功能授权未授予 | 真机验收里 `axTrusted: 0` | 自动粘贴不可用，降级为"只复制"（§13 已列，行为正确） |
| Linux | 只有接口骨架 | 按 §11 的边界，本期不投入 |
| 面板交互只有真机手测 | 拖动 / 边缘缩放 / 点面板以外自动收起这三件事没有自动化判据 | 它们要真的产生 AppKit 事件（鼠标按下、窗口 key 变化），脚本化得靠 AX 权限与座标点击，收益不如代价。Go 侧的**策略**（`ui.closeOnBlur`、落盘尺寸）有单测钉着，原生侧的**触发**靠手测 |
| 热键输入态只在浏览器里验过 | harness（假绑定页面）实测了草稿 / 确认 / 取消 / 留空确认四条路径，并在 `activeElement=BODY`（无焦点）下验证了按键捕获——这正是 WebKit 里点 button 不聚焦的真实形态 | 真机（WKWebView）上仍要手测一遍完整录制；与浏览器的差异只剩两点：全局热键让出后系统不再拦按键、以及 WKWebView 的键盘通路（搜索框可输入已证明它是通的） |
| 草稿本三条真机项未取证 | 「目录 200 条滚动流畅」「保存延迟 ≤ 800 ms / 5 s 强制落盘」「编辑中强杀」三项需要真人操作面板（滚动、计时、`kill -9`），`accept.sh` 不驱动 UI | 不影响功能正确性——不变量侧（往返转换、blob 存活、回滚、列表不读全文）都有单测钉住；缺的是**性能与断电语义的实测数字**，与上面的「面板交互只有真机手测」同类 |

---

## 复现

```bash
test/run.sh                    # 单测口径（含两类规模测试）+ 前端检查 + 跨平台编译
test/run.sh --build --accept   # 出产物并对它做真机验收（会覆盖当前剪贴板）
```

也可以逐条跑，等价命令：

```bash
go test -tags sqlite_fts5 -p 1 -count=1 ./...
scripts/build.sh -platform darwin/universal
test/accept.sh
```

> `accept.sh` **不需要 `ps`/`top`**：空闲内存与 CPU 由 App 自己报
> （`resprobe.go` 在第 5 / 25 秒各打一条「资源快照」，常驻用 Mach `task_info`
> 读自身、CPU 用 `getrusage(RUSAGE_SELF)` 两次相减）。起因是这类"读别的进程"
> 的操作在受限环境（沙箱、部分 MDM 策略）下会被直接拒绝，于是那两项会长期
> 停在"设计目标"上——拿不到证据时应当说拿不到，而不是让判据悄悄失效。
