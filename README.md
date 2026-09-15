# PawClip · 喵喵贴

跨平台（macOS + Windows）剪贴板历史管理器。**单机、无账号、无同步。**

> 优先级：**安装包体积最小化 > 常驻内存最小化 > 功能完整度**
> 数据主权：全部数据落在本地 SQLite + blob 文件，可一键导出为第三方工具也能读的 `.clipbak` 压缩包。

---

## 当前状态

**设计已收敛，代码尚未开始。** 技术选型、数据模型、过期策略、备份格式、图标资源均已定稿落盘。

⚠️ **开工前有一个阻塞项**：Wails v2 不暴露 macOS 窗口类，而"免抢焦点面板"是 P0 核心体验。必须先做完 **M0 门禁**再写业务代码 —— 见 `DESIGN.md` **§0.1** 与 **§17**。

---

## 从这里开始

| 文档 | 内容 |
|---|---|
| **`DESIGN.md`** | 唯一权威设计规格 |
| `BACKUP-FORMAT.md` | `.clipbak` 备份包格式规范 v1 |
| `assets/icon/` + `scripts/` | 图标源图与构建脚本 |
| `.workbuddy/memory/` | 历次决策与踩坑记录（append-only 工作日志） |

**建议阅读顺序**：`DESIGN.md` **§0**（范围与技术栈）→ **§0.1**（M0 门禁，阻塞项）→ **§17**（里程碑与起步顺序）→ §2 架构 → §4 数据模型 → §5 过期生命周期 → §7/§8 平台实现要点 → **§15 优化清单（动手前务必扫一遍，能省大量返工）**。

---

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.26 |
| 桌面框架 | Wails v2 |
| 前端 | React + Vite + TypeScript |
| 数据库 | SQLite（`mattn/go-sqlite3`，**必须带 `sqlite_fts5` 构建标签**） |
| 原生桥 | macOS：cgo + Objective-C shim；Windows：`golang.org/x/sys/windows` |

---

## 图标资源

```bash
# 换源图后先探测新的几何常量（裁剪框 + 圆角半径），再重建
node scripts/probe-icon-source.cjs assets/icon/pawclip-source.png

# 生成全套：.icns / .ico / 9 档 PNG / 托盘模板图 / 4 张预览校验图
NODE_PATH=/Users/zego/.workbuddy/binaries/node/workspace/node_modules \
  node scripts/build-icons.cjs
```

- 源图 `assets/icon/pawclip-source.png` 是**不带 alpha** 的位图稿，四角为不透明近白、外围还有一圈环境阴影，因此只能靠**几何圆角遮罩**抠出轮廓 —— 所有灰度阈值/洪泛填充方案都必然失败，原因与实测量见 §16.3。
- 依赖 `sharp`。产物已入库（约 3.3 MB），所以**纯 Go 侧构建不需要 Node**。

---

## 验收标准（§12 摘录）

| 项 | 指标 |
|---|---|
| 无自捕获 | 连续 100 次面板回贴，历史条目数增加为 **0** |
| 去重正确性 | 同一内容复制 100 次，库中始终 **1 行**且 `use_count = 100` |
| 断电安全 | 捕获过程中强杀进程，重启后库可正常打开且无半截记录 |
| 空闲内存（面板销毁态） | macOS ≤ 30 MB；Windows ≤ 25 MB |
| 检索延迟 | ≥3 字 < 50 ms @ 10 万条；1–2 字 LIKE < 300 ms |
