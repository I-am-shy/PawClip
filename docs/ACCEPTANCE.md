# PawClip 验收报告（`docs/DESIGN.md` §12 全项）

> 这份报告回答一个问题：**§12 列的指标，到底达标没有。**
> 达不到的如实标 ❌ 并给出归因，不粉饰。

实测环境：macOS / Apple Silicon，2026-09-16。
单测口径 `go test -tags sqlite_fts5 -p 1 ./...`；真机口径 `scripts/accept.sh`
（对 `build/bin/pawclip.app` 实跑，真剪贴板、真磁盘、真 SIGKILL）。
当前规模：**257 条用例 / 9 个包全绿**（另有一个无测试的工具包 `tools/genpinyin`）。

---

## 指标结果

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 捕获延迟（文本） | < 30 ms | 流水线 p95 **1.2 ms**（端到端含 50ms 合批窗口约 48 ms） | `capture` `TestCaptureLatencyText` | ✅ |
| 捕获延迟（5 MB 图片） | < 200 ms | 1600×1200 噪声 PNG（5.8 MB）端到端中位数 **108 ms** | `capture` `TestCaptureLatencyImageAt5MB` | ✅ |
| 检索延迟（≥3 字） | < 50 ms @ 10 万条 | 3 字中文 p95 **2.5 ms**；4 字 **4.9 ms**；英文 **4.5 ms**；中英混排 **4.4 ms** | `store` `TestSearchLatencyAt100k` | ✅ |
| 检索延迟（2 字 LIKE） | < 300 ms | p95 **0.67 ms**（中/英） | 同上 | ✅ |
| 空闲内存 | macOS ≤ 30 MB（面板销毁态） | 稳态 **54 MB**；`wails.Run` 之前基线 13 MB → **窗口 + WebView 约 41 MB** | `scripts/accept.sh` ⑤（进程自报快照） | ❌ 见下 |
| 空闲 CPU | macOS < 1% | **0.33–0.42%**（20 秒窗口，已排除启动开销） | 同上 | ✅ |
| 无自捕获 | 连续 100 次回贴 → 新增 0 | 100 次回贴后 `CountAlive` 仍为 1，守卫拦下 100 次 | `capture` `TestAcceptance1_NoSelfCapture` | ✅ |
| 去重正确性 | 复制 100 次 → 1 行、`use_count=100` | 真机 `pbcopy` 100 次 → 1 行、`use_count=100` | `capture` `TestAcceptance2_DedupeUseCount` + `accept.sh` ⑥ | ✅ |
| 搜索正确性 | 1/2/3 字、英文、混排、大小写、特殊字符 | 全部命中符合预期（含 2 字走 LIKE 的分支） | `store` `TestSearch_CorrectnessTable` / `TestSearchCorrectnessAtTwoChars` | ✅ |
| 导出完整性 | 导出→清库→导入，全字段一致 | 条目/内容/分类/标签/置顶/过期时间逐项比对一致 | `backup` `TestAcceptance1_ExportThenImportPreservesEverything` | ✅ |
| 导入幂等 | 同包导入两次，第二次全跳过 | 第二次全部走跳过，库中无重复 | `backup` `TestAcceptance2_ImportTwiceIsIdempotent` | ✅ |
| 断电安全 | 捕获中强杀，重启后可开库且无半截记录 | 真机 SIGKILL → 重启走「标记 → `integrity_check=ok`」→ 无悬空 blob | `capture` `TestAcceptance3_Kill9Recovery` + `accept.sh` ⑦ | ✅ |
| （附）前端体积 | §14 第 14 条：gzip 后 < 150 KB | gzip **69.3 KB**（JS 65.4 + CSS 3.7） | `npm run build` 产物实测 | ✅ |

`scripts/accept.sh` 共 21 项，**20 通过 / 1 失败**——失败项即上表的「空闲内存」。

---

## 唯一未达标项：空闲内存 54 MB

数字本身是真的，归因也是确定的：

- `wails.Run` **之前**进程只有 **13 MB**；
- 把窗口与 WebView 建出来之后就是 **53–59 MB**，随后回落到稳态 **54 MB**。

也就是说超出的 41 MB **全在 Wails/WebKit 侧**，Go + SQLite + 捕获链路那部分只占十几 MB。

而 §12 的判据写的是「**面板销毁态**」：它默认面板闲置时会被销毁、把 WebView 一起还回去。
本实现做不到这件事，因为 **Wails v2.16 的单窗口模型只导出 `Show/Hide/Quit`，没有窗口
销毁与重建 API**（销毁了就没法再显示，那比内存超标更糟）。所以当前实现改成"按空闲阈值
收起面板"，并把这个事实与实测数字摆在 `PanelLifecycle()` 里（见 §13 已列风险、§14 第 10 条）。

要真正收进 30 MB，需要换掉 Wails 的单窗口模型（改成启动时不建窗口、首次呼出时才创建，
或改用能销毁/重建窗口的方案）。那是一次架构改动，不属于本期打磨范围。

> 因此验收脚本里这一项的失败是**预期内的已知缺口**，不是回归。脚本不改判据，
> 免得把"唯一没做到的事"从报告里抹掉。

---

## 验收过程中抓出的产物级问题

这些问题**一条单测都不会红**——它们的共同点是只有"真跑一次产物"才会暴露。
列在这里是为了说明 `scripts/accept.sh` 为什么必须存在。

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

---

## 复现

```bash
go test -tags sqlite_fts5 -p 1 -count=1 ./...   # 单测口径（含两类规模测试）
scripts/build.sh -platform darwin/universal     # 出产物
scripts/accept.sh                               # 对产物做真机验收（会覆盖当前剪贴板）
```

`accept.sh` 需要 `ps` 取进程资源统计。若运行环境禁用 `ps`，第 ③ 项（空闲内存/CPU）
会报"没量到"而不是给假数字——这是故意的：拿不到证据就说拿不到。
