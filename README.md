# PawClip · 喵喵贴

跨平台（macOS + Windows）剪贴板历史管理器。**单机、无账号、无同步、无云端。**

> **优先级**：安装包体积 > 常驻内存 > 功能完整度
> **数据主权**：全部数据落在本地 SQLite + blob，可导出为第三方工具也能读的 `.clipbak` 包

---

## 当前状态：M1 已完成（捕获链路 + 落库）

**M1 已交付**：工程骨架、`store/` 持久化层、`clipboard/` 双平台后端、`capture/` 捕获流水线，
`main` 10 / `store` 50 / `clipboard` 59 / `capture` 22 = **141 条测试全绿**，
`scripts/build.sh` 可产出可运行的 `.app`。M1 是**纯后端**，没有历史列表 UI —— 想"看效果"请用
下面「怎么构建与验收」。

**下一步**：M2（搜索面板、回贴/自动粘贴、回收站与保留策略、设置 UI、免抢焦点面板）。

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
# 跑测试（-tags sqlite_fts5 必须带，否则 store 包会因为缺 fts5 模块而失败）
go test ./... -tags sqlite_fts5

# 构建 .app —— 一定用这个包装脚本，别直接 wails build
scripts/build.sh                          # 默认 darwin/arm64
scripts/build.sh -platform windows/amd64  # 透传任意 wails build 参数

# 看懂 M1 到底干了什么（启动真实 App → 模拟复制 → 查库）
scripts/demo-m1.sh

# 真机验收（会覆盖你当前的系统剪贴板）
PAWCLIP_REAL_CLIPBOARD=1 go test ./capture/ -tags sqlite_fts5 -run TestAcceptance4 -v
```

**为什么构建必须走 `scripts/build.sh`：**
FTS5 全文索引依赖 `sqlite_fts5` 构建标签，而 `wails.json` 的 schema **没有**任何能持久化
Go 构建标签的字段（只有 CLI 的 `-tags`）。直接 `wails build` 出来的产物缺 fts5 模块，
`store.Open` 会按设计**优雅降级**——打一行 WARN 然后退回 LIKE 检索，进程照常启动。
也就是说全文检索会**静默失效**，不查日志根本发现不了。包装脚本把标签钉死，避免这个坑。

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

## 验收标准（§12 摘录）

| 项 | 指标 |
|---|---|
| 无自捕获 | 连续 100 次面板回贴，历史条目数增加为 **0** |
| 去重正确性 | 同一内容复制 100 次，库中始终 **1 行**且 `use_count = 100` |
| 断电安全 | 捕获过程中强杀进程，重启后库可正常打开且无半截记录 |
| 空闲内存（面板销毁态） | macOS ≤ 30 MB；Windows ≤ 25 MB |
| 检索延迟 | ≥3 字 < 50 ms @ 10 万条；1–2 字 LIKE < 300 ms |
