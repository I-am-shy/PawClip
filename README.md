# PawClip · 喵喵贴

跨平台（macOS + Windows）剪贴板历史管理器。**单机、无账号、无同步、无云端。**

> **优先级**：安装包体积 > 常驻内存 > 功能完整度
> **数据主权**：全部数据落在本地 SQLite + blob，可导出为第三方工具也能读的 `.clipbak` 包

---

## 当前状态：M1–M4 已完成（可日用）

**已交付**：捕获链路（文本 / 图片 / 文件）、SQLite + FTS5 中文检索（trigram 两段式 + 1–2 字 LIKE 兜底）、
免抢焦点面板（⌘⇧V）、回贴与 ⌘1..9 直贴、分类与标签、三级 TTL 与回收站、`.clipbak` 导出导入、
统计面板、开机自启、中英双语、内容转换器、连续粘贴、拼音首字母检索。

```bash
go test -tags sqlite_fts5 -p 1 -count=1 ./...   # 257 条用例 / 10 个包 → 全绿
scripts/build.sh                                 # → build/bin/pawclip.app（universal，ad-hoc 签名）
scripts/accept.sh                                # 对打包产物做真机验收
```

**`scripts/accept.sh` 20/21 通过**，唯一未达标的是 §12 的「空闲常驻内存 ≤ 30 MB」：
实测稳态 **54 MB**，其中 **41 MB 是 Wails 建的窗口 + WebView**（同一进程在 `wails.Run` 之前只有 13 MB）。
判据里的「面板销毁态」需要面板闲置销毁才能成立，而它在 Wails v2.16 的单窗口模型下做不了
（见 §13 与下面「已知遗留」）。其余 9 项指标全部实测达标，见文末「验收结果」。

**下一步**：把常驻内存收进 30 MB（唯一实质缺口），以及 Windows 侧的真机验证（本期只在 macOS 上实测）。

---

## 文档地图

| 文件 | 内容 | 什么时候读 |
|---|---|---|
| **`HANDOFF-PROMPT.md`** | **新会话开工提示词**（整份文件即提示词正文，复制粘贴即可） | **开工第一条消息** |
| **`DESIGN.md`** | 唯一权威设计规格（16 节 + 附录 A） | 全程 |
| `BACKUP-FORMAT.md` | `.clipbak` 备份包格式规范 | M3 做导出导入时 |
| **`poc/`** | M0 技术门禁验证：报告 + 可运行工程 | **M2 做面板时**（含可直接复用的 cgo 代码） |
| `assets/` + `scripts/` | 图标源图与构建脚本 | 需要重建图标时 |
| `.workbuddy/memory/` | 决策与踩坑的工作日志 | 想追溯"为什么这么定"时 |

**建议阅读顺序**：`DESIGN.md` **§0 项目基调** → **§16 里程碑** → §2 跨平台架构 → §4 数据模型（含 DDL）→ §5 过期与生命周期 → §7/§8 平台实现要点 → **§14 优化清单（动手前必扫，能省大量返工）**。

---

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.26 |
| 桌面框架 | Wails v2 |
| 前端 | React + Vite + TypeScript |
| 数据库 | SQLite（`mattn/go-sqlite3`，**必须带 `sqlite_fts5` 构建标签**） |
| 原生桥 | macOS：cgo + Objective-C shim；Windows：`golang.org/x/sys/windows`（无需 cgo） |

选型理由与已排除的路线见 §0.4。

---

## 关键决策（已定，不再变更）

| # | 决策 | 已知代价 |
|---|---|---|
| 1 | **不加密** —— 明文 SQLite + blob | 本地任何进程都能读到剪贴板历史 |
| 2 | **不签名** —— 不买 Apple / Windows 证书 | 首次打开需手动绕过 Gatekeeper / SmartScreen |
| 3 | **中英双语**，跟随系统 | 前后端都要走 i18n（易漏位置见 §14 第 24 条） |
| 4 | **仅 GitHub Releases 分发** | 无自动更新通道 |

---

## 功能分期速览

| 期 | 内容 |
|---|---|
| **P0** | 监听文本/图片/文件、落库去重置顶、免抢焦点面板 + 热键、中文搜索、回贴与 ⌘1..9 直贴、应用黑名单与保密类型跳过、托盘与开机自启 |
| **P1** | 三级 TTL + 回收站、分类与标签、缩略图预览、批量操作、`.clipbak` 导出导入、统计面板 |
| **P2** | 连续粘贴、拼音首字母搜索、搜索语法、内容转换器、正则提取、OCR、片段库 |
| **P3** | 隐私暂停、敏感内容识别、数据库加密、Linux 后端 |

完整清单与交付边界见 §11；里程碑拆解见 §16。

---

## 开工前必须知道的三个坑

**1. 免抢焦点面板——已实测通过，但实现路径是唯一解。**
必须"**真正创建** `NSPanel`（不能 `object_setClass` 事后换类，会 SIGTRAP）+ App 跑在 **Accessory 激活策略**下，**两个条件缺一不可**；显示顺序必须 `orderFrontRegardless` → `makeKeyWindow`。判据是 `frontmostApplication` 而非 `NSApp.isActive`（后者会把正确答案误判成失败）。
完整方案见 §0.2，可运行代码见 `poc/wails-panel/panel_darwin.m`。

**2. 中文搜索必须两段式。**
FTS5 的 `trigram` 分词器要求查询词 **≥ 3 字符**，1–2 字（含英文的 `AI`、`js`、`5G`）必须走 `LIKE` 兜底，否则搜"文档"永远为空。单测要覆盖 1/2/3 字与中英混排。

**3. 图片绝不走 IPC。**
缩略图通过 Wails 静态资源服务交给 WebView 直载。把 100 张缩略图 base64 塞进 IPC 会让首屏卡 1 秒以上、内存翻倍。

---

## 环境要求

| 项 | 要求 |
|---|---|
| Go | 1.26+ |
| Wails CLI | v2.16+（`go install github.com/wailsapp/wails/v2/cmd/wails@latest`） |
| Xcode CLT | 必需（macOS 侧 cgo 编译） |
| Node | 20+（仅前端构建与图标重建；`poc/` 不需要） |

构建命令见 §10。

---

## 怎么构建与验收

```bash
# 后端全量测试。两个开关都不能省，理由见下面。
go test -tags sqlite_fts5 -p 1 -count=1 ./...

# 构建 .app —— 一定用这个包装脚本，别直接 wails build
scripts/build.sh                          # 默认 darwin/arm64
scripts/build.sh -platform darwin/universal
scripts/build.sh -platform windows/amd64  # 透传任意 wails build 参数

# 成品验收：对打包好的 .app 做真机实跑（真剪贴板、真磁盘、真 SIGKILL）
scripts/accept.sh

# 看懂 M1 那条捕获链路（启动真实 App → 模拟复制 → 查库）
scripts/demo-m1.sh

# 真机验收（会覆盖你当前的系统剪贴板）
PAWCLIP_REAL_CLIPBOARD=1 go test ./capture/ -tags sqlite_fts5 -run TestAcceptance4 -v
```

**为什么构建必须走 `scripts/build.sh`：**
FTS5 全文索引依赖 `sqlite_fts5` 构建标签，而 `wails.json` 的 schema **没有**任何能持久化
Go 构建标签的字段（只有 CLI 的 `-tags`）。直接 `wails build` 出来的产物缺 fts5 模块，
`store.Open` 会按设计**优雅降级**——打一行 WARN 然后退回 LIKE 检索，进程照常启动。
也就是说全文检索会**静默失效**，不查日志根本发现不了。包装脚本把标签钉死，避免这个坑。
同一纪律在 `.github/workflows/release.yml` 里也复用（那个 job 走的就是这个脚本）。

**为什么测试要带 `-p 1`：**
本仓库有两类**按墙上时间取证**的规模测试（store 的 10 万条检索延迟、capture 的 5MB 图片捕获
延迟）。`go test` 默认并行跑包，CPU 被 10 个包抢满时这些数字会飙到 2–3 倍——那是机器被占满，
不是产品退化。串行跑让度量条件稳定，总耗时基本不变。

**为什么测试要带 `-tags sqlite_fts5`：**
缺了这个标签，`store` 会因为 fts5 模块不存在而**跳过**一批全文检索用例（而不是变红）——
静默降级的另一种形态。

**`scripts/accept.sh` 与 `go test` 的分工**（两者缺一不可）：
单测证明"逻辑正确"（注入假后端、可重复）；accept.sh 证明"这个产物真能跑"——
签名坏了、`Info.plist` 丢了 LSUIElement、构建漏了标签，这些**一条单测都不会红**，
但用户一定打不开或功能少一半。

---

## 图标资源

```bash
# 换源图后先探测新的几何常量（裁剪框 + 圆角半径），再重建
node scripts/probe-icon-source.cjs assets/icon/pawclip-source.png

# 生成全套：.icns / .ico / 9 档 PNG / 托盘模板图 / 4 张预览校验图
NODE_PATH=<node_modules> node scripts/build-icons.cjs
```

- 源图 `assets/icon/pawclip-source.png` 是**不带 alpha** 的位图稿，四角为不透明近白、外围还有一圈环境阴影 —— 所有灰度阈值/洪泛填充方案都必然失败，只能靠**几何圆角遮罩**抠轮廓。原因与实测量见 §15.3。
- 依赖 `sharp`。产物已入库（约 3.3 MB），所以**纯 Go 侧构建不需要 Node**。换图流程见 §15.2。

---

## 验收结果（DESIGN §12 全项）

实测环境：macOS / Apple Silicon，2026-09-16。单测口径为 `go test -tags sqlite_fts5 -p 1`；
真机口径为 `scripts/accept.sh`（对 `build/bin/pawclip.app` 实跑）。

| 项 | §12 指标 | 实测 | 取证方式 | 结论 |
|---|---|---|---|---|
| 捕获延迟（文本） | < 30 ms | 流水线 p95 **1.2 ms**（端到端含 50ms 合批窗口约 48 ms） | `capture` `TestCaptureLatencyText` | ✅ |
| 捕获延迟（5 MB 图片） | < 200 ms | 1600×1200 噪声 PNG（5.8 MB）端到端中位数 **108 ms** | `capture` `TestCaptureLatencyImageAt5MB` | ✅ |
| 检索延迟（≥3 字） | < 50 ms @ 10 万条 | 3 字中文 p95 **2.5 ms**；4 字 **4.9 ms**；英文 **4.5 ms**；中英混排 **4.4 ms** | `store` `TestSearchLatencyAt100k` | ✅ |
| 检索延迟（2 字 LIKE） | < 300 ms | p95 **0.67 ms**（中/英） | 同上 | ✅ |
| 空闲内存 | macOS ≤ 30 MB（面板销毁态） | 稳态 **54 MB**；`wails.Run` 之前基线 13 MB → **窗口+WebView 约 41 MB** | `scripts/accept.sh` ③（进程自报快照） | ❌ 见下 |
| 空闲 CPU | macOS < 1% | **0.33–0.42%**（20 秒窗口，已排除启动开销） | 同上 | ✅ |
| 无自捕获 | 连续 100 次回贴 → 新增 0 | 100 次回贴后 `CountAlive` 仍为 1，守卫拦下 100 次 | `capture` `TestAcceptance1_NoSelfCapture` | ✅ |
| 去重正确性 | 复制 100 次 → 1 行、`use_count=100` | 真机 `pbcopy` 100 次 → 1 行、`use_count=100` | `capture` `TestAcceptance2` + `accept.sh` ⑥ | ✅ |
| 搜索正确性 | 1/2/3 字、英文、混排、大小写、特殊字符 | 全部命中符合预期（含 2 字走 LIKE 的分支） | `store` `TestSearch_CorrectnessTable` / `TestSearchCorrectnessAtTwoChars` | ✅ |
| 导出完整性 | 导出→清库→导入，全字段一致 | 条目/内容/分类/标签/置顶/过期时间逐项比对一致 | `backup` `TestAcceptance1_ExportThenImportPreservesEverything` | ✅ |
| 导入幂等 | 同包导入两次，第二次全跳过 | 第二次全部走跳过，库中无重复 | `backup` `TestAcceptance2_ImportTwiceIsIdempotent` | ✅ |
| 断电安全 | 捕获中强杀，重启后可开库且无半截记录 | 真机 SIGKILL → 重启走「标记 → integrity_check=ok」→ 无悬空 blob | `capture` `TestAcceptance3_Kill9Recovery` + `accept.sh` ⑦ | ✅ |
| （附）前端体积 | §14 第 14 条：gzip 后 < 150 KB | gzip **69.3 KB**（JS 65.4 + CSS 3.7） | `npm run build` 产物实测 | ✅ |

**关于唯一未达标的「空闲内存」**：数字本身是真的，归因也是确定的——
`wails.Run` 之前进程只有 13 MB，把窗口与 WebView 建出来之后就是 53–55 MB。
也就是说超出的 41 MB 全在 Wails/WebKit 侧，Go + SQLite + 捕获链路那部分只占十几 MB。
而 §12 的判据写的是「**面板销毁态**」：它默认面板闲置时会被销毁、把 WebView 一起还回去。
本实现做不到这件事，因为 Wails v2.16 的单窗口模型只导出 `Show/Hide/Quit`，
**没有窗口销毁与重建 API**（销毁了就没法再显示，那比内存超标更糟）。
所以当前实现改成"按空闲阈值收起面板"，并把这个事实与实测数字摆在 `PanelLifecycle()` 里
（见 §13 已列风险、§14 第 10 条）。

要真正收进 30 MB，需要换掉 Wails 的单窗口模型（改成启动时不建窗口、首次呼出时才创建，
或改用能销毁/重建窗口的方案）。那是一次架构改动，不属于本期打磨范围。

---

## 已知遗留

| 项 | 现状 | 影响 |
|---|---|---|
| 空闲常驻内存 54 MB | 面板闲置销毁做不了（Wails v2.16 单窗口无重建 API） | §12 唯一未达标项；面板收起后内存仍在 |
| Windows 真机未验证 | 只有 `CGO_ENABLED=0` 交叉编译烟测 + `windows-2022` 上的正式构建 | 只能证明"能编过"；剪贴板/热键/托盘的实际行为未在真机测过 |
| 辅助功能授权未授予 | 真机验收里 `axTrusted: 0` | 自动粘贴不可用，降级为"只复制"（§13 已列，行为正确） |
| Linux | 只有接口骨架 | 按 §11 的边界，本期不投入 |

