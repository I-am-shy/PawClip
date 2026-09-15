<!-- 用法：本文件即"提示词正文"。新会话第一条消息，把「## 任务」以下的内容全选复制粘贴即可。
     工作目录 /Users/zego/Desktop/shy/copy-tools —— 这也是 git 仓库根，不要再往上找。 -->

# 交接：PawClip 从「设计定稿」转入「开始构建」（M1 开工）

## 任务

接手一个已经完成方案设计的项目，现在开始写代码。

- **工作目录（= git 仓库根）**：`/Users/zego/Desktop/shy/copy-tools`
- **项目**：**PawClip**（中文名「喵喵贴」）—— macOS + Windows **单机**剪贴板历史管理器
- **当前状态**：方案设计定稿并已提交 git，**代码零行**。你的任务是建立工程骨架并完成 **M1：捕获链路 + 落库**。

---

## 一、先读文档（务必先读，不要凭常识猜）

按此顺序，读完再动手：

1. `README.md` —— 一页总览 + 三个开工坑
2. `DESIGN.md` —— **唯一权威规格**（988 行）。阅读顺序：**§0 项目基调 → §16 里程碑 → §2 跨平台架构 → §4 数据模型（含 DDL）→ §5 过期与生命周期 → §7 / §8 平台实现要点 → §14 优化清单**
3. 暂时不用读：`BACKUP-FORMAT.md`（M3 才用）、`poc/`（M2 做面板时读，里面的 `panel_darwin.m` 与 `probe.go` 可直接复用）

**规则**：`DESIGN.md` 是唯一真源。若我的口头要求与文档冲突，以文档为准并明确指出冲突。若你判断文档有误，先说清楚再改文档，不要静默偏离。

---

## 二、已冻结的前提（不要重新论证、不要重新选型）

| 项 | 值 |
|---|---|
| 后端 | Go 1.26（本机已装 go1.26.3 darwin/arm64） |
| 桌面框架 | **Wails v2**（v3 仍是 alpha，不用） |
| 前端 | React + Vite + TypeScript；**不用** UI 组件库 / router / 全局状态库 |
| 数据库 | SQLite `mattn/go-sqlite3`，编译**必须**带 `-tags sqlite_fts5` |
| macOS 原生桥 | cgo + Objective-C shim |
| Windows 原生桥 | `golang.org/x/sys/windows`，**无需 cgo** |
| 平台 | macOS 12+（universal 二进制）、Windows 10 1809+ / 11 |
| 明确不做 | 加密、代码签名、云同步、账号体系、Linux（只留接口骨架）、应用商店上架 |

**M0 技术门禁已实测通过，不要重新验证。** 结论：免抢焦点面板必须「真正创建 NSPanel」+「App 跑 Accessory 激活策略」**两个条件同时满足**（已排除 `object_setClass` 换类等三条路线）。详见 §0.2。**这部分属于 M2，本次不要碰。**

**环境提醒**：Wails CLI **本机尚未安装**，先 `go install github.com/wailsapp/wails/v2/cmd/wails@latest` 并确保 `$HOME/go/bin` 在 PATH 中。Node v22 / Xcode CLT / clang 已就绪。

**工程根约定**：直接把**当前仓库根**当作 §10 目录结构里的 `pawclip/`（`pawclip/` 只是文档里的逻辑名，不要新建嵌套目录）。

---

## 三、本次范围：M1 = 捕获链路 + 落库（纯后端，不做 UI）

**不要做** UI、搜索、面板、热键、托盘、GC。

交付物：

1. **工程骨架**：Go module（module path 自定，如 `github.com/zego/pawclip`）、`wails.json`、`frontend/`（Vite + React + TS，能 `wails build` 通过即可，页面留占位）、`build/appicon.png`（从 `assets/icon/dist/pawclip-1024.png` 复制）、`.gitignore` 补全（§14 第 22 条）
2. **`store/`**：`schema.go`（建表 + `user_version` 迁移）、`items.go`（写入路径）、`blobs.go`（两级分片路径 + 原子写 + 缩略图）、`settings.go`
3. **单写 goroutine + 合批提交**：攒 50 ms 或 20 条一起 commit
4. **`clipboard/`**：`backend.go`（**照 §2 的定义，不要自己改签名**）、`guard.go`（`SelfWriteGuard`，照 §2 实现）、`normalize.go`、`filter.go`
5. **双平台后端**：`windows.go`（事件驱动 `AddClipboardFormatListener`）+ `darwin.go` 与 `pasteboard_darwin.m/.h`（轮询 + 自适应间隔）
6. **测试与验收脚本**（见第五节）

**建议实现顺序**（§16 给的顺序，照做）：schema → 写入路径 → **Windows 后端**（事件驱动、不涉及轮询，先跑通更省事）→ macOS 后端（轮询 + 自适应间隔 + 同帧读前台 App）→ 过滤与守卫。

---

## 四、M1 期间必须遵守的坑（都已被实测或明确确认，不要绕）

1. **SQLite 写必须串行**：用单写 goroutine 消费 channel（或 `SetMaxOpenConns(1)`）。并发写会撞 `SQLITE_BUSY`。
2. **FTS5 建表必须保留默认 `detail=full`**，绝不用 `detail=none` / `detail=column`——实测与 `trigram` 分词器不兼容，建表与回填都成功但查询直接抛 `OperationalError`。`tokenize = 'trigram'`。
3. **去重靠 partial unique index**：`uq_items_fp_alive ON items(fingerprint) WHERE deleted_at IS NULL`。写库用
   `INSERT ... ON CONFLICT(fingerprint) WHERE deleted_at IS NULL DO UPDATE SET created_at = ?, use_count = use_count + 1, ...`
   ——**冲突目标必须带上那个 `WHERE` 子句**，否则 SQLite 匹配不到 partial index。置顶时**不更新** `first_seen_at`。
4. **大文本**：指纹对**完整内容**做 sha256，`text_content` 只截断存前 256K 字符，否则去重会漏。
5. **`filter.go` 顺序**：保密标记 → 应用黑名单 → 类型开关 → 自写入守卫 → 尺寸上限。最便宜的检查放最前面。
6. **WAL 治理**：每 1000 次写入或每小时 `PRAGMA wal_checkpoint(TRUNCATE)`。
7. **启动自检**：跑 `SELECT * FROM items_fts LIMIT 1`，失败则整体退化为 LIKE 模式。**不要**每次启动做 `PRAGMA integrity_check`（万级库要几秒），改用标记文件方案（正常退出删标记，启动见标记才做完整性检查）。
8. **macOS 轮询热路径上零分配**：每 0.2 s 只读一个 `NSInteger changeCount`，不要在轮询里构造 `NSArray` / `NSString`；Go 侧循环内不要 `defer`、不要新建结构体。自适应间隔 0.2 s / 1 s。
9. **前台 App 必须与剪贴板内容在同一次读取里取**（`frontmostApplication`），不要分两次取，否则会记错来源应用。
10. **Windows 侧三个必做转换**：`CF_DIBV5` → **手动转 PNG**（不像 macOS 直接给 PNG）；`"HTML Format"` 必须剥离 `Version:0.9\r\nStartHTML:...` 头部；去抖。保密标记（`IS_PRIVATE` / `org.nspasteboard.ConcealedType` 对应物）也要在 Windows 侧检测。
11. **blob 布局**：`blobs/<sha 前2>/<sha 第3-4>/<sha256>.<ext>`，两级分片；写入用**临时文件 + rename** 原子替换。
12. **读取要自带重试**：Windows `OpenClipboard` 可能被别的进程占住，读操作内部要有退避重试；macOS 侧同理处理 `changeCount` 抖动。
13. **不要用 `golang.design/x/clipboard`**（不暴露 `changeCount` 与原始 pasteboard types）；**不要用 `modernc.org/sqlite`**（体积 +8–10 MB）。平台层必须自己写。

---

## 五、M1 验收标准（§12 摘录，必须逐项给证据）

| # | 判据 | 怎么测 |
|---|---|---|
| 1 | **无自捕获**：连续 100 次回贴，历史条目数增加为 **0** | 此时还没有面板，直接循环调 `Write()` 100 次，比较库中条目数 |
| 2 | **去重正确性**：同一内容复制 100 次，库中始终 **1 行**且 `use_count = 100` | 同上，或真机复制同一段文字 100 次 |
| 3 | **断电安全**：捕获过程中强杀进程，重启后库可正常打开、无半截记录 | `kill -9` 后重新打开：`SELECT count(*)` + 完整性校验 |
| 4 | **手工抽查**：文本 / 图片 / 文件三类都能落库，图片缩略图正确 | 真机复制三种内容，检查 `items` 行与 `blobs/` 文件 |

**跨平台测试的务实做法（重要，别在这上面浪费时间）**：

- 平台层用 build tag 隔离（`//go:build darwin` / `//go:build windows`）；本机（mac）只能真跑 macOS 后端。
- 把 Windows 专属逻辑里**纯计算的部分**抽成无平台依赖的纯函数（`CF_DIBV5 → PNG`、`CF_HTML` 头部剥离、去抖、类型归并），放在 `normalize.go`，**在 mac 上直接跑单测**，用真实字节样本做用例。
- Windows 后端整体只要求 `GOOS=windows go build ./...` 与 `go vet` 通过 + 逻辑评审。
- 验收 1 / 2 / 3 用**注入的假后端**（fake `Backend`）跑，保证可重复、不依赖真实剪贴板。

---

## 六、工程约定

- 新建分支 `feat/m1-capture` 并在其上提交，保持 `main` 随时可交接。
- 提交用 Conventional Commits（`feat:` / `fix:` / `test:` / `chore:` / `docs:`），一次提交一件事，说明可以写中文。
- **不要提交**构建产物（`build/bin/`、`frontend/dist/`、`node_modules/`、`*.db*`、`blobs/`）；**例外**：`assets/icon/dist/` 要入库——它是 Wails 的构建**输入**，不是发布产物。
- 前端 gzip 产物目标 < 150 KB（M1 还不紧张，但不要引入大依赖）。
- 新增第三方依赖前先说明理由与体积代价。
- **保持 `poc/` 原样不动**，不要清理或改写。
- 不要改 `DESIGN.md` 的既有决策；发现文档确有错误时，改动单独成一个 `docs:` 提交，并在回复里点出来。

---

## 七、工作方式（我的偏好，请照做）

- **不要一步一步征求同意。** 开工前用不超过 10 行说明你的落地顺序，然后连续把 M1 做完。
- 每完成一个可验证单元（schema / 写入路径 / 某平台后端 / 守卫），**立刻跑测试并贴出真实输出**，不要只说"已完成"。
- 全部做完后给一份收尾报告：**改动文件清单** + **每条验收判据的实测结果（含负数结果与未解风险）** + 下一步（M2）准备动什么。
- 遇到文档没覆盖、且会影响后续返工的分歧点，**停下来问我一次**，不要猜。

---

## 八、从哪开始

先读文档（第一节），然后：

1. 用 10 行说明 M1 落地顺序
2. 建工程骨架 + `store/schema.go`，跑通建表
3. 一路做到验收全过（第 1–4 项）
4. 给收尾报告
