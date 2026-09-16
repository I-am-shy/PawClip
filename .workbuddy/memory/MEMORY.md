# PawClip 项目长期约定

## 文档布局

- **面向使用者的说明只在根 `README.md`**（功能 / 安装 / 使用 / 构建 / 已知限制）。
- **开发文档一律在 `docs/`**：`DESIGN.md`（唯一权威规格）、`BACKUP-FORMAT.md`、
  `ACCEPTANCE.md`（§12 验收实测）、`HANDOFF-PROMPT.md`（M1 提示词存档）`、README.md`（索引）。
- 源码注释引用设计条款写 **`docs/DESIGN.md §N`**（带路径、带 `.md`，可寻址），
  不写裸文件名；裸 `§N` 只在同文件上下文里用。
- 移动/重命名文档时，**必须同步改全仓引用**（这个仓库有 150+ 处）。
- `poc/` 是自包含的 M0 验证工程，连它自己的 md 一起保持原样，不要拆散、不要清理。

## 测试与脚本布局

- `scripts/` 只放**构建与开发的入口**：`build.sh`（出包）、`dev.sh`（本地开发）、图标工具。
- `test/` 放**可独立执行的测试资产**：`run.sh`（总入口）、`accept.sh`（产物验收）、
  `demo-m1.sh`（链路演示）、`check-i18n.mjs`（前端 i18n）、`README.md`（分层说明）。
- **`*_test.go` 不搬**：Go 要求与被测包同目录（`package foo_test` 也一样），
  且本仓库测试访问包内未导出符号。别被"测试文件应集中"的直觉带偏。
- 本地开发用 `scripts/dev.sh`，不直接 `wails dev` —— 同样要钉 `-tags sqlite_fts5`，
  且默认会把数据目录隔离到 `.workbuddy/tmp/pawclip-dev/`（`PAWCLIP_DEV_REAL=1` 可关）。
- 已知差距（如内存未达标）在验收脚本里记 **warn 不影响退出码**，另留一条回归线；
  **不要让一条永远红的判据存在**，那会让人学会无视整张表。

## 构建与测试（四条硬口径，CI 里都钉了一遍）

| 纪律 | 理由 |
|---|---|
| 构建走 `scripts/build.sh`，不直接 `wails build` | `wails.json` 无法持久化 Go 构建标签，漏 `-tags sqlite_fts5` 会让 FTS5 **静默降级**成逐行匹配（只打一行 WARN，界面照常） |
| 测试带 `-tags sqlite_fts5` | 缺标签时 `store` 会**跳过**全文检索用例而不是变红 |
| 测试带 `-p 1`、`-count=1` | 有两类按墙上时间取证的规模测试，并行抢 CPU 会让数字虚高 2–3 倍；`-count=1` 才能与 CI 互推 |
| 一切测试与验收走 `test/run.sh` | 它跑的每条命令都与 CI 同口径（`test/run.sh --build --accept` 是发布前完整口径） |

- 提交纪律：Conventional Commits；改 `docs/DESIGN.md` 的**既有决策**要单独一个 `docs:` 提交
  并在回复里点出来（纯排版/路径/目录树修正不算改决策，但也要说明）。
- 不提交构建产物（`build/bin/`、`build/dist/`、`frontend/dist/`、`node_modules/`、`*.db*`）；
  **例外**：`assets/icon/dist/` 要入库——它是 Wails 的构建**输入**。

## 平台差异（写文档时不能含糊）

- **免抢焦点面板只是 macOS 的能力**（真 NSPanel + Accessory 策略，缺一不可）。
  Win32 做不到——`WS_EX_NOACTIVATE` 的窗口收不到键盘输入，所以 Windows 侧会取一次前台、
  粘贴完成后把前台还给原窗口（`panel/windows.go` 注明这是**有意差异**）。
- Windows 侧目前**只在 CI 构建，未在真机验证**；`panel/windows.go` 顶部列了三个待验假设。
- 已知不达标项：空闲内存 54 MB vs 目标 30 MB（面板"闲置销毁"在 Wails v2.16 单窗口模型下
  做不了，当前是**闲置收起**）。这条要如实写在 README 与 `docs/ACCEPTANCE.md`，不粉饰。

## 用户偏好（沿用）

- 一次把剩余步骤跑完再统一复核，不要逐步征求同意；但每批/每 PR 要给出可核对的实测输出
  （不要只说"已完成"）。
- 给结论要带 diff / 文件级拆分 / 表格，宁详细勿含糊；未达标项与未解风险必须显式列出。
