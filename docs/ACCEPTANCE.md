# PawClip 验收报告（`docs/DESIGN.md` §12 全项）

> 这份报告回答一个问题：**§12 列的指标，到底达标没有。**
> 达不到的如实标 ⚠️ 并给出归因，不粉饰；有东西坏了才标 ❌。

实测环境：macOS / Apple Silicon，2026-09-16（首轮）· 2026-09-21（M5 草稿本补测）。
单测口径 `test/run.sh`（= `go test -tags sqlite_fts5 -p 1 -count=1 ./...`）；
真机口径 `test/accept.sh`（对 `build/bin/pawclip.app` 实跑，真剪贴板、真磁盘、真 SIGKILL）。
当前规模：**299 条用例 / 9 个包全绿**（另有一个无测试的工具包 `tools/genpinyin`）。

---

## 指标结果

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 捕获延迟（文本） | < 30 ms | 流水线 p95 **1.2 ms**（端到端含 50ms 合批窗口约 48 ms） | `capture` `TestCaptureLatencyText` | ✅ |
| 捕获延迟（5 MB 图片） | < 200 ms | 1600×1200 噪声 PNG（5.8 MB）端到端中位数 **108 ms** | `capture` `TestCaptureLatencyImageAt5MB` | ✅ |
| 检索延迟（≥3 字） | < 50 ms @ 10 万条 | 3 字中文 p95 **2.5 ms**；4 字 **4.9 ms**；英文 **4.5 ms**；中英混排 **4.4 ms** | `store` `TestSearchLatencyAt100k` | ✅ |
| 检索延迟（2 字 LIKE） | < 300 ms | p95 **0.67 ms**（中/英） | 同上 | ✅ |
| 空闲内存 | macOS ≤ 30 MB（面板销毁态） | 稳态 **53–54 MB**；`wails.Run` 之前基线 13 MB → **窗口 + WebView 约 40 MB** | `test/accept.sh` ③（进程自报快照） | ⚠️ 见下 |
| 空闲 CPU | macOS < 1% | **0.33–0.35%**（20 秒窗口，已排除启动开销） | 同上 | ✅ |
| 无自捕获 | 连续 100 次回贴 → 新增 0 | 100 次回贴后 `CountAlive` 仍为 1，守卫拦下 100 次 | `capture` `TestAcceptance1_NoSelfCapture` | ✅ |
| 去重正确性 | 复制 100 次 → 1 行、`use_count=100` | 真机 `pbcopy` 100 次 → 1 行、`use_count=100` | `capture` `TestAcceptance2_DedupeUseCount` + `accept.sh` ⑥ | ✅ |
| 搜索正确性 | 1/2/3 字、英文、混排、大小写、特殊字符 | 全部命中符合预期（含 2 字走 LIKE 的分支） | `store` `TestSearch_CorrectnessTable` / `TestSearchCorrectnessAtTwoChars` | ✅ |
| 导出完整性 | 导出→清库→导入，全字段一致 | 条目/内容/分类/标签/置顶/过期时间逐项比对一致 | `backup` `TestAcceptance1_ExportThenImportPreservesEverything` | ✅ |
| 导入幂等 | 同包导入两次，第二次全跳过 | 第二次全部走跳过，库中无重复 | `backup` `TestAcceptance2_ImportTwiceIsIdempotent` | ✅ |
| 断电安全 | 捕获中强杀，重启后可开库且无半截记录 | 真机 SIGKILL → 重启走「标记 → `integrity_check=ok`」→ 无悬空 blob | `capture` `TestAcceptance3_Kill9Recovery` + `accept.sh` ⑦ | ✅ |
| （附）前端体积 | §14 第 14 条：gzip 后 < 150 KB | gzip **81.5 KB**（JS 76.77 + CSS 4.71）；余量约 68 KB | `npm run build` 产物实测 | ✅ |

### M5 草稿本（2026-09-21 新增）

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 草稿往返 | 导出→清库→导入后逐字段一致，且**图片真的在 `blobs/` 里** | 标题/正文/创建时间/修改时间逐字段一致；包内引用已改写成 `blobs/…`、库内改回 `blob/…`；导入后 `os.Stat` 贴图命中 | `backup` `TestAcceptance3_DraftsRoundTrip` | ✅ |
| 草稿回滚 | 一键回滚带走那批草稿，本机草稿不受影响 | 回滚后该批次草稿数为 0；归档草稿与 `import_id IS NULL` 的草稿不在删除面内 | 同上（测试后半段）+ `store` `TestDeleteImportItems_AlsoRemovesDrafts` / `TestPurgeDraft_DetachesFromImport` | ✅ |
| 草稿图片存活 | 贴图跨过 24 h 门槛跑 3 轮 GC 仍在 | 3 轮 GC 后贴图仍在；另钉住「库里只有草稿、`items` 为空」时孤儿扫描**仍然执行** | `retention` `TestGC_DraftImagesSurviveOrphanSweep` / `TestGC_DraftOnlyLibrarySweepsOrphans` | ✅ |
| 草稿归档到期回收 | 归档草稿超 `retention.trashTtlSec` 后被硬删 | 超期归档草稿被收走并计入 `Report.DraftsPurged` / `DraftsPurgedChars` | `retention` `TestGC_PurgesArchivedDrafts` | ✅ |
| 草稿目录规模 | 200 条时目录滚动流畅（列表不读正文全文） | **未做真机滚动取证**；纪律侧已钉住：`ListDrafts` 的 SQL 只取 `substr(md,1,120)` 摘要，不 SELECT 全文 | `store` `TestListDrafts_DoesNotLoadFullBody` | ⚠️ 见下 |
| 草稿保存延迟 | 停手 ≤ 800 ms 落盘；持续打字 5 s 内必落一次 | **未做量化取证**（前端防抖 500 ms + 5 s 强制落盘，代码路径存在但无计时用例） | — | ⚠️ 见下 |
| 草稿断电安全 | 编辑中强杀，重启后为最后一次落盘内容且库可开 | **未做真机取证**（沿用条目侧同一条 `capture` 用例的结论外推，不构成独立证据） | — | ⚠️ 见下 |

草稿本的**格式与不变量**在单测层面是齐的（`test/check-md.mjs` 68 条断言钉住
md ⇄ HTML 往返与转义契约；`store`/`backup`/`retention` 共 20 条草稿相关用例）。
上表三条 ⚠️ 是**缺真机取证**，不是发现缺陷——它们都需要在真实面板里手动操作
（滚动、计时、强杀），`accept.sh` 目前不驱动 UI，所以自动验收覆盖不到。

`test/accept.sh` 共 21 项，**20 通过 / 1 已知差距 / 0 失败**——已知差距即上表的「空闲内存」。
退出码只跟真正的失败走，理由见下一节。

---

## 唯一未达标项：空闲内存 ~54 MB

数字本身是真的，归因也是确定的：

- `wails.Run` **之前**进程只有 **13 MB**；
- 把窗口与 WebView 建出来之后就是 **53–59 MB**，随后回落到稳态 **53–54 MB**。

也就是说超出的 40 MB **全在 Wails/WebKit 侧**，Go + SQLite + 捕获链路那部分只占十几 MB。

而 §12 的判据写的是「**面板销毁态**」：它默认面板闲置时会被销毁、把 WebView 一起还回去。
本实现做不到这件事，因为 **Wails v2.16 的单窗口模型只导出 `Show/Hide/Quit`，没有窗口
销毁与重建 API**（销毁了就没法再显示，那比内存超标更糟）。所以当前实现改成"按空闲阈值
收起面板"，并把这个事实与实测数字摆在 `PanelLifecycle()` 里（见 §13 已列风险、§14 第 10 条）。

要真正收进 30 MB，需要换掉 Wails 的单窗口模型（改成启动时不建窗口、首次呼出时才创建，
或改用能销毁/重建窗口的方案）。那是一次架构改动，不属于本期打磨范围。

> 脚本对这条的处理是**两段判据**（`test/accept.sh` ③）：
> ≤ 30 MB 记 ✅ 达标；31–80 MB 记 ⚠️ **已知差距**（有归因、记录在案，**不影响退出码**）；
> > 80 MB 记 ❌ 失败。
>
> 后半段是刻意留的**回归线**：稳态 53–54 MB，涨到 80 以上就说明真的多占了东西，
> 那时必须查。之所以不让它一直算失败——**一条永远红的判据会让人学会无视整张表**，
> 那它就再也不报警了。判据本身没有下调，30 MB 这个目标值仍然写在明面上。

---

## 验收过程中抓出的产物级问题

这些问题**一条单测都不会红**——它们的共同点是只有"真跑一次产物"才会暴露。
列在这里是为了说明 `test/accept.sh` 为什么必须存在。

| # | 问题 | 为什么单测抓不到 |
|---|---|---|
| 1 | App 带 `LSUIElement` 启动时进程已是 Accessory，`setActivationPolicy:Accessory` 返回 NO 的含义是"没变化"而非失败，却每次启动固定打一条 WARN | WARN 只出现在真机日志里，而它恰在 §0.2「免抢焦点必要条件」上，会把排查的人带偏 |
| 2 | CI 里 `go build ./... -tags sqlite_fts5` 的标签写在包路径之后，被当成包路径解析，这个 job 从未成功过 | `go test` 对包后参数有兼容处理，照抄测试命令就得到一条永远不会红的 CI 步骤 |
| 3 | 版本号两份不一致：`Info.plist` 说 0.1.0、程序自报 0.1.0-m1 | 没有任何用例读版本号；改为从 `wails.json` 单一真源经 `-ldflags` 注入 |
| 4 | 验收脚本自身的 flake：固定 `sleep 0.3` 复制 100 次，第二遍跑出 99（撞上 200 ms 轮询相位） | 是**测量方式**问题，不是去重语义问题；改成"等 `use_count` 涨到预期值再放下一次" |
| 5 | `.gitignore` 漏了 `build/dist/`，本地打包后多出一个 10 MB 未跟踪文件 | 不影响构建与测试，但很容易被 `git add -A` 带进仓库 |

另外补齐了 §12 里原先**一条测试都没有**的「捕获延迟」两项（`capture/latency_test.go`）。

---

## 已知遗留

| 项 | 现状 | 影响 |
|---|---|---|
| 空闲常驻内存 54 MB | 面板闲置销毁做不了（Wails v2.16 单窗口无重建 API） | §12 唯一未达标项；面板收起后内存仍在 |
| Windows 真机未验证 | 只有 `CGO_ENABLED=0` 交叉编译烟测 + `windows-2022` 上的正式构建 | 只能证明"能编过"；剪贴板/热键/托盘的实际行为未在真机测过 |
| 辅助功能授权未授予 | 真机验收里 `axTrusted: 0` | 自动粘贴不可用，降级为"只复制"（§13 已列，行为正确） |
| Linux | 只有接口骨架 | 按 §11 的边界，本期不投入 |
| 面板交互只有真机手测 | 拖动 / 边缘缩放 / 点面板以外自动收起这三件事没有自动化判据 | 它们要真的产生 AppKit 事件（鼠标按下、窗口 key 变化），脚本化得靠 AX 权限与座标点击，收益不如代价。Go 侧的**策略**（`ui.closeOnBlur`、落盘尺寸）有单测钉着，原生侧的**触发**靠手测 |
| 热键输入态只在浏览器里验过 | harness（假绑定页面）实测了草稿 / 确认 / 取消 / 留空确认四条路径，并在 `activeElement=BODY`（无焦点）下验证了按键捕获——这正是 WebKit 里点 button 不聚焦的真实形态 | 真机（WKWebView）上仍要手测一遍完整录制；与浏览器的差异只剩两点：全局热键让出后系统不再拦按键、以及 WKWebView 的键盘通路（搜索框可输入已证明它是通的） |
| 草稿本三条真机项未取证 | 「目录 200 条滚动流畅」「保存延迟 ≤ 800 ms / 5 s 强制落盘」「编辑中强杀」三项需要真人操作面板（滚动、计时、`kill -9`），`accept.sh` 不驱动 UI | 不影响功能正确性——不变量侧（往返转换、blob 存活、回滚、列表不读全文）都有单测钉住；缺的是**性能与断电语义的实测数字**，与上面的「面板交互只有真机手测」同类 |

---

## 复现

```bash
test/run.sh                    # 单测口径（含两类规模测试）+ 前端检查
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
