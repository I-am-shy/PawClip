# PawClip · 喵喵贴 · 跨平台剪贴板历史工具 · 设计规格（定稿）

> 定位：macOS + Windows 单机剪贴板历史管理器。无账号、无同步、无云端。
> 优先级：安装包体积最小化 > 常驻内存最小化 > 功能完整度。
> 数据主权：全部数据落在本地 SQLite + blob 文件，可一键导出为可被第三方工具读取的压缩包。

---

## 0. 项目基调

### 0.1 范围与标识

| 项 | 决定 |
|---|---|
| 平台 | macOS 12+（Intel + Apple Silicon 通用二进制）、Windows 10 1809+ / 11 |
| 架构 | 单机本地应用，无服务端、无账号体系、无多设备同步 |
| 数据出口 | `.clipbak` 压缩包（ZIP 容器 + JSON/YAML 清单 + 二进制 blob），见 `docs/BACKUP-FORMAT.md` |
| 暂不做 | Linux（预留后端骨架）、端到端同步、团队共享、App Store / Microsoft Store 上架 |
| 名称与标识 | 中文 **喵喵贴** · 英文 **PawClip**（Paw 猫爪 + Clip 剪贴）。bundle id / AppUserModelID / 仓库名 / 配置目录见 §15.1 |

### 0.2 免抢焦点面板 —— M0 实测结论 ✅

P0 核心体验（热键呼出**免抢焦点**面板 + ⌘/Ctrl + 1..9 直贴）依赖一项 Wails v2 官方不提供的能力，已于 2026-09-15 用最小工程实测验证。

**结论：可行**，但必须走一条特定路径。验证工程见 `poc/wails-panel/`，完整数据与复现步骤见 `poc/POC-RESULT.md`。

Wails v2 官方 `options` 确实没有任何窗口类控制（`mac.Options` 只有 `TitleBar` / `Appearance` / `WebviewIsTransparent` / `WindowIsTranslucent` / `ContentProtection` / `About`；`windows.Options` 也没有 `WS_EX_NOACTIVATE`）。但这不构成阻塞——我们不改 Wails 的配置，而是在运行时用 cgo 改造窗口对象。

#### 实测矩阵（三次独立复现，结果一致）

| 方案 | 窗口类 | 键盘焦点 | 抢走前台 | 结论 |
|---|---|---|---|---|
| 原样 Wails 窗口 | `WailsWindow` | 拿不到 | 否 | 不可用 |
| 只给 NSWindow 加 `nonactivatingPanel` 样式位 | `WailsWindow` | 拿不到 | 否 | **AppKit 明确拒绝** |
| `object_setClass` 换成 NSPanel 子类 | `PawClipPanel` | — | — | **崩溃 SIGTRAP** |
| **新建真 NSPanel + 接管 contentView + Accessory** | `PawClipPanel` | **稳定持有** | **从未** | ✅ **采用** |

#### 采用方案的两个必要条件

1. **必须"真正创建" NSPanel**，不能用 `object_setClass` 事后改类。
2. **必须运行在 Accessory 激活策略**下（`NSApplicationActivationPolicyAccessory`，等价 `Info.plist` 的 `LSUIElement = true`）。

**两个条件缺一不可**——实测中单独去掉任一条件都会立刻失败（去掉 Accessory 后，键盘焦点 0/6 次拿到）。

实现骨架（完整代码见 `poc/wails-panel/panel_darwin.m`）：

```objc
// ① 窗口类：borderless 面板默认 canBecomeKeyWindow = NO，必须覆写
@interface PawClipPanel : NSPanel @end
@implementation PawClipPanel
- (BOOL)canBecomeKeyWindow  { return YES; }
- (BOOL)canBecomeMainWindow { return YES; }
@end

// ② 激活策略（越早越好，最好在窗口创建之前）
[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];

// ③ 创建真 NSPanel
NSPanel *panel = [[PawClipPanel alloc]
    initWithContentRect:frame
              styleMask:(NSWindowStyleMaskNonactivatingPanel | NSWindowStyleMaskBorderless)
                backing:NSBackingStoreBuffered
                  defer:NO];

// ④ 把 Wails 的 contentView（含 WKWebView）搬过来
NSView *cv = [wailsWindow contentView];
[wailsWindow setContentView:[[[NSView alloc] initWithFrame:frame] autorelease]];
[panel setContentView:cv];

// ⑤ 面板配置
[panel setBecomesKeyOnlyIfNeeded:NO];   // 一显示就收键盘，不等点击
[panel setLevel:NSFloatingWindowLevel];
[panel setHidesOnDeactivate:NO];        // 失活时保持可见
[panel setCollectionBehavior:(NSWindowCollectionBehaviorCanJoinAllSpaces |
                              NSWindowCollectionBehaviorFullScreenAuxiliary)];

// ⑥ 显示并取焦点 —— 顺序不能反
[panel orderFrontRegardless];           // 窗口不可见时 makeKeyWindow 是空操作
[panel makeKeyWindow];
```

#### 判断"有没有抢焦点"的正确判据

**不能看 `NSApp.isActive`。** Accessory 策略下面板持有键盘焦点时 `isActive` 是 **`true`**，但 `NSWorkspace.frontmostApplication` 仍是原来那个 App——**菜单栏没有被抢走**。

正确判据是：**`frontmostApplication` 有没有被换成本 App**。

实测数据（`adopt` + Accessory，连续 6 次采样）：

| 指标 | 结果 |
|---|---|
| 面板持有键盘焦点（`isKeyWindow`） | 6/6 |
| 系统前台被抢走 | **0/6** |
| 面板保持可见 | 6/6 |

#### 三条被实测否定的路（不要重试）

| 路 | 现象 | 根因 |
|---|---|---|
| 给普通 `NSWindow` 加 `nonactivatingPanel` 位 | AppKit 打印 `NSWindow does not support nonactivating panel styleMask 0x80`，且 `styleMask` **实际未被修改**（32780 → 32780，bit 7 仍为 0） | 该 mask 的语义要求实例本身是 NSPanel |
| `object_setClass(win, NSPanel子类)` | 换类"成功"（类名变了），随后任何 `orderOut` 触发 **SIGTRAP** | Wails 窗口的真实类是 **`NSKVONotifying_WailsWindow`**（KVO 动态子类，488 字节），带 `userMinSize` / `userMaxSize` 两个 `NSSize` ivar；NSPanel 的 ivar 布局与之重叠，调用 `setFloatingPanel:` 等 NSPanel 专有方法会**写坏 Wails 的尺寸约束内存** |
| 用 `makeKeyAndOrderFront:` 兜底 | 会**真的激活 App**，随后被系统纠正、键盘焦点一并丢失 | 它改变了激活状态，违反 nonactivating 语义 |

#### 顺带验出的一条实现顺序

`makeKeyWindow` 在窗口**不可见**时是空操作。必须先 `orderFrontRegardless` 再 `makeKeyWindow`；反过来写会得到一个"可见但收不到键盘"的面板。

**门禁结论**：技术栈维持 **Go + Wails v2**，不需要上 Wails v3 alpha，也不需要接受"抢焦点 + 事后补偿"的降级方案。实测中暴露的两个实现注意点已写进 §16 的 M2 交付要求。

### 0.3 关键决策与代价

| # | 决策 | 已知代价（接受） |
|---|---|---|
| 1 | **不加密**：数据库与 blob 全明文 | 任何能读用户目录的进程都能读到剪贴板历史。省掉 SQLCipher 依赖（约 −1.5 MB）与主密码解锁流程 |
| 2 | **不签名**：不买 Apple 开发者账号（$99/年）与 Windows 证书（$200–400/年） | 首次打开需手动绕过 Gatekeeper / SmartScreen，README 写清步骤（§14 F 组） |
| 3 | **中英双语 + 跟随系统**：前端与后端都走 i18n | 维护两套字符串。**易漏项**见 §14 第 24 条 |
| 4 | **仅 GitHub Releases 分发**：Actions 矩阵构建，安装包附加到 Release | 无自动更新通道；构建产物不入库 |

### 0.4 技术栈

| 层 | 选型 | 理由 |
|---|---|---|
| 后端 | **Go 1.26** | Windows 原生调用无需 cgo；goroutine 模型天然适配"监听 / 捕获 / GC"三条流水线 |
| 桌面框架 | **Wails v2** | 复用系统 WebView（WKWebView / WebView2），不捆绑 Chromium；v3 仍在 alpha，不采用 |
| 前端 | **React + Vite + TypeScript** | 不用 UI 组件库；视图数 ≤ 6，无需大型路由与状态库 |
| 数据库 | **SQLite（`mattn/go-sqlite3`）** | cgo 方案体积代价小；**必须带 `sqlite_fts5` 构建标签**，否则 FTS5 不可用 |
| 原生桥 | macOS：**cgo + Objective-C shim**；Windows：**`golang.org/x/sys/windows`** | 见 §2 |
| 图标 | 成品位图稿 → sharp 抠图 / 遮罩 / 缩放 → `.icns` / `.ico` | 见 §15 |

> **已排除的路线**（不必重新提议）：**Electron**（150 MB+ 安装包、150–300 MB 空闲内存，体积与内存双输）；**纯 Go 自绘 UI**（Fyne / Gio，需内嵌 CJK 字体再涨 8–15 MB，内存 80–150 MB）；**双原生 SwiftUI + WinUI3**（体积内存都不差，但要维护两套 UI 与两套数据访问层，成本翻倍）；**Rust + Tauri v2**（体积最优——安装后占用比 Go 方案小 5–8 MB——但语言不熟会拖慢进度，本项目选择开发速度优先）。

### 0.5 核心原则

**空闲内存的瓶颈是 WebView，不是后端语言。** Go 运行时本身约 10–18 MB，而 WebView 打开时要 60–90 MB。因此设计的核心是把 WebView 当作"用完即弃"的资源（面板闲置即销毁），而不是让它一直挂着。

---

## 1. 体积与内存预算

### 1.1 目标值（Go + Wails v2）

| 指标 | macOS | Windows |
|---|---|---|
| 安装包 | 6–9 MB（DMG，universal） | 5–8 MB（NSIS，LZMA） |
| 安装后磁盘占用 | ~16–20 MB | ~14–18 MB |
| **空闲常驻 RSS**（面板已销毁） | **20–32 MB** | **18–28 MB** |
| 面板打开期间 RSS | 75–115 MB | 85–135 MB |
| 空闲 CPU | < 1%（0.2s 轮询） | **≈ 0%**（事件驱动，无轮询） |
| 冷启动到可交互 | < 500 ms | < 600 ms |

### 1.2 达成手段

**Go 侧**

```bash
go build -trimpath -ldflags="-s -w -X main.version=$VERSION"
```

- `-s -w` 去掉符号表与 DWARF 调试信息，可省 25–30%
- `-trimpath` 去掉本地路径，同时保证构建可复现
- **不引入 SQLCipher**（已定不加密）；用 `mattn/go-sqlite3` + `sqlite_fts5` 构建标签
- **不要用 `modernc.org/sqlite`**（纯 Go 免 cgo），它会给二进制再加 8–10 MB
- 图片格式按需注册：只 `import _ "image/png"`、`_ "image/jpeg"`、`_ "golang.org/x/image/webp"`，不要引入全量图像框架
- 缩略图用 `nfnt/resize` 或 `disintegration/imaging` 二选一
- **不引入 ORM**（GORM 等），直接写 SQL；**不引入 Web 框架**，Wails 自带的绑定机制足够
- 日志用标准库 `log/slog`，不引入 zap / logrus

**前端侧**
- 前端资源全部内联进二进制，无独立 `dist` 目录
- 视图数 ≤ 6（面板 / 设置 / 分类管理 / 标签 / 备份 / 统计），无需大型路由与状态库
- 不引入 UI 组件库，手写样式

**WebView 侧（决定内存的关键）**
- 面板窗口**闲置超时即销毁**（默认 300s 无操作），而非仅隐藏
- 热键按下时重建窗口，冷启动 150–300 ms
- 可选优化：把 `300s` 调到 `60s` 换取更低均值内存，代价是频繁使用时更常触发重建
- Windows 额外优化：WebView2 支持 `ICoreWebView2_3::TrySuspendAsync()`，可在窗口保留的前提下挂起释放内存；macOS 无对应 API，只能销毁重建，因此**macOS 走销毁策略，Windows 走销毁 + suspend 双保险**

**Windows 安装器**

用 Wails 的 NSIS 产物（`wails build -nsis`），并在安装脚本中加入 WebView2 运行时检测：缺失时下载 Evergreen Bootstrapper（约 +1.8 MB）。**不要内嵌 full 运行时**（+130 MB）或 fixed runtime（+180 MB）。Windows 11 与已更新过的 Windows 10 默认自带 WebView2，实际触发下载的比例很低。

---

## 2. 跨平台架构

平台差异被收敛到一个 `Backend` 接口后面，上层全部平台无关。

```go
// clipboard/backend.go
package clipboard

// Backend 是平台原生的剪贴板后端。macOS 与 Windows 各有一个实现，
// 通过 build tag 选择编译，上层完全平台无关。
type Backend interface {
	// Start 启动监听，变更时向 ch 推送信号（只推信号，不推内容）
	Start(ch chan<- Tick) error
	Stop()
	// Read 读取剪贴板全部可用表示。Windows 实现内部自带重试。
	Read() (*Raw, error)
	// Write 写回内容
	Write(p *Payload) error
	// IsPrivate 平台原生"请勿记录"标记检测
	IsPrivate(r *Raw) bool
}

// Tick 是变更信号。真正的读取由捕获 goroutine 去做
// （Windows 需要重试与延迟，不能在这里做）。
type Tick struct {
	AtMs int64
	Seq  uint64
}

// Raw 是归一化后的剪贴板快照。
type Raw struct {
	RawTypes      []string // 原始类型名，用于保密判定与诊断
	Text          *string  // UTF-8
	HTML          *string  // 已剥离平台包装
	RTF           []byte   // 原始 RTF 字节
	Image         *Image   // 已转 PNG + 像素尺寸
	Files         []string
	SourceAppID   string   // macOS: bundle id / Windows: exe 路径或 AppUserModelID
	SourceAppName string
}

// Image 是归一化后的位图。
type Image struct {
	PNG    []byte
	Width  int
	Height int
}
```

goroutine 模型（共 4 条）：

```
[监听 goroutine]  macOS: 0.2s / 1s 自适应轮询  |  Windows: 消息窗口收 WM_CLIPBOARDUPDATE
      │ 向 channel 推送 Tick（带去抖）
      ▼
[捕获 goroutine]  Read() → filter() → normalize() → fingerprint() → 事务写库
      │ channel
      ▼
[GC goroutine]    每 60s：TTL 过期 → 回收站 → 硬删；条数 / 容量淘汰
      │
[Wails 主 goroutine] 绑定方法：搜索、回贴、CRUD、导入导出
```

Go 的并发模型在这里很省心：四个 `go func()` + `chan` 就够，不需要手写状态机。

**但有一条 Go + SQLite 的硬约束**：SQLite 的写操作必须串行化。用**单个写 goroutine 消费 channel**，或给 `*sql.DB` 设 `SetMaxOpenConns(1)`。否则并发写会撞 `SQLITE_BUSY`。推荐前者——写 goroutine 还能顺便实现 §14 的合批提交。

### 去抖设计

连续的剪贴板变更（如某些应用一次复制写多个格式、或用户连点）要合并：同一指纹在 `debounceMs`（默认 120ms）内的重复 tick 丢弃。

### 自写入守卫（跨平台统一）

这是我们写回剪贴板后又被自己捕获的问题，两个平台都会发生。统一用一个守卫解决：

```go
// clipboard/guard.go
package clipboard

// selfWriteWindow 是"我们自己刚写完剪贴板"的有效判定窗口。
const selfWriteWindow = 2 * time.Second

// SelfWriteGuard 解决"我们写回剪贴板后又被自己捕获"的问题。
// 比记录 changeCount 更稳：Windows 上拿不到自身写入对应的序号，
// 指纹比对天然跨平台。
type SelfWriteGuard struct {
	mu       sync.Mutex
	expected string
	armedAt  time.Time
}

// Arm 在写剪贴板之前调用。
func (g *SelfWriteGuard) Arm(fingerprint string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expected = fingerprint
	g.armedAt = time.Now()
}

// ShouldDrop 在捕获到内容之后调用，返回 true 表示这次事件应被丢弃。
func (g *SelfWriteGuard) ShouldDrop(fingerprint string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.expected == "" {
		return false
	}
	// 指纹一致且在窗口内 → 是我们自己写的
	if g.expected == fingerprint && time.Since(g.armedAt) < selfWriteWindow {
		g.expected = ""
		return true
	}
	// 标记已陈旧，清掉后放行
	if time.Since(g.armedAt) >= selfWriteWindow {
		g.expected = ""
	}
	return false
}
```

比"记 changeCount"更稳：Windows 上没法可靠拿到自身写入对应的序号，指纹比对天然跨平台。

---

## 3. 平台能力对照

| 能力 | macOS | Windows |
|---|---|---|
| **变更检测** | ❌ 无通知，只能轮询 `NSPasteboard.changeCount` | ✅ `AddClipboardFormatListener` → `WM_CLIPBOARDUPDATE`，事件驱动 |
| **保密内容标记** | `org.nspasteboard.ConcealedType`、`org.nspasteboard.TransientType`、`org.nspasteboard.AutoGeneratedType` | `Clipboard Viewer Ignore`、`ExcludeClipboardContentFromMonitorProcessing`、`CanIncludeInClipboardHistory`(DWORD=0)、`CanUploadToCloudClipboard`(DWORD=0) |
| **免抢焦点窗口** | `NSPanel` + styleMask `.nonactivatingPanel` + 覆写 `canBecomeKey` | 窗口样式加 `WS_EX_NOACTIVATE` |
| **自动粘贴** | `CGEvent` 模拟 ⌘V，**需辅助功能授权** | `SendInput` 模拟 Ctrl+V，**无需授权**，但受 UIPI 限制 |
| **全局热键** | Carbon `RegisterEventHotKey` | `RegisterHotKey` |
| **图片读取** | pasteboard `public.tiff` / `public.png` | `CF_DIBV5` → **必须手动转 PNG** |
| **HTML 读取** | `public.html`，纯 HTML | `"HTML Format"`，**带 `Version:0.9\r\nStartHTML:...` 头部，必须剥离** |
| **文件列表** | `public.file-url` / `NSFilenamesPboardType` | `CF_HDROP` |
| **开机自启** | `SMAppService`（macOS 13+）/ LaunchAgent | 注册表 `HKCU\...\Run` |
| **托盘** | `NSStatusItem` | 通知区图标 + 右键菜单 |
| **签名成本** | Developer ID + 公证，Apple 开发者账号 $99/年 | OV/EV 代码签名证书 $200–400/年 |

---

## 4. 数据模型

### 4.1 建表 DDL

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA user_version = 1;          -- schema 版本，每次迁移 +1

CREATE TABLE items (
  id              INTEGER PRIMARY KEY,
  kind            TEXT    NOT NULL,    -- text|html|rtf|image|files|mixed
  text_content    TEXT,                -- 纯文本，搜索与回贴主用
  html_content    TEXT,
  rtf_path        TEXT,                -- 大对象外置，存相对 blobs/ 的路径
  image_path      TEXT,
  thumb_path      TEXT,                -- 列表缩略图（长边 <= 160px）
  file_paths      TEXT,                -- JSON 数组字符串
  preview         TEXT,                -- 列表摘要，截断 200 字符
  fingerprint     TEXT    NOT NULL,    -- sha256:<hex>
  byte_size       INTEGER NOT NULL DEFAULT 0,
  source_app_id   TEXT,                -- 平台原生应用标识
  source_app_name TEXT,
  source_url      TEXT,
  category_id     INTEGER REFERENCES categories(id) ON DELETE SET NULL,
  pinned          INTEGER NOT NULL DEFAULT 0,
  first_seen_at   INTEGER NOT NULL,    -- 首次复制时间（置顶更新时不变）
  expires_at      INTEGER,             -- UNIX 秒；NULL = 永不过期
  ttl_source      TEXT,                -- global|category|item|never
  created_at      INTEGER NOT NULL,    -- 排序键：重复复制时被更新为 now
  last_used_at    INTEGER,
  use_count       INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER,             -- 软删除（回收站）
  import_id       INTEGER REFERENCES imports(id) ON DELETE SET NULL
);

CREATE INDEX idx_items_alive   ON items(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_items_expire  ON items(expires_at)       WHERE expires_at IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_items_app     ON items(source_app_id);
CREATE INDEX idx_items_cat     ON items(category_id);
CREATE INDEX idx_items_import  ON items(import_id);
-- 只对"存活条目"做指纹唯一，删掉后可重新录入同一内容
CREATE UNIQUE INDEX uq_items_fp_alive ON items(fingerprint) WHERE deleted_at IS NULL;

CREATE TABLE categories (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  color       TEXT,
  icon        TEXT,
  rule        TEXT,        -- JSON 自动归类规则
  ttl_seconds INTEGER,     -- 该分类默认存活秒数；NULL = 跟随全局
  sort_order  INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);

CREATE TABLE tags (
  id    INTEGER PRIMARY KEY,
  name  TEXT NOT NULL UNIQUE,
  color TEXT
);

CREATE TABLE item_tags (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  tag_id  INTEGER NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
  PRIMARY KEY (item_id, tag_id)
);

-- 导入批次，用于"一键回滚上次导入"
CREATE TABLE imports (
  id            INTEGER PRIMARY KEY,
  source_name   TEXT,          -- 原始 .clipbak 文件名
  manifest_hash TEXT,          -- 幂等性校验
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER,
  imported      INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  failed        INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL  -- running|ok|partial|rolled_back
);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,   -- JSON 编码的值
  updated_at INTEGER NOT NULL
);

CREATE VIRTUAL TABLE items_fts USING fts5(
  text_content,
  preview,
  content       = 'items',
  content_rowid = 'id',
  tokenize      = 'trigram'
);
```

FTS 同步靠 `AFTER INSERT / AFTER UPDATE / AFTER DELETE ON items` 三个触发器维护。

### 4.2 中文搜索的两段式策略

| 查询长度 | 走哪条路径 | 目标耗时 |
|---|---|---|
| ≥ 3 字符 | `items_fts MATCH ?`（trigram） | < 50 ms @ 10 万条 |
| 1–2 字符 | `LIKE '%?%'` 兜底，配合 `deleted_at IS NULL` 偏索引 + `LIMIT 200` | < 300 ms @ 10 万条 |

**为什么必须有兜底**：FTS5 默认的 `unicode61` 分词器按空格切词，整段中文会变成单个 token，搜"文档"永远不命中。`trigram` 分词器把文本切成三字符滑窗解决了这个问题，但它**要求查询词至少 3 个字符**。用户搜"链接""图片"这类两字词时若不兜底就是空结果——这是同类工具在国内最常见的差评来源。单测必须覆盖 1 字 / 2 字 / 3 字 / 中英混排四种输入。

**分界实测（SQLite 3.50.4，已在本机验证）**：trigram 的门槛是 **3 个字符**，中日韩与 ASCII 一视同仁——

| 查询 | 字符数 | trigram 命中 |
|---|---|---|
| `中文`、`文档`、`链接` | 2 | **0**（必须走 LIKE 兜底） |
| `中文文` | 3 | ✅ |
| `wor` | 3 | ✅ |
| `hello` | 5 | ✅ |

推论：**英文短查询同样会漏**。`AI`、`js`、`5G`、`C++` 这类 1–2 字符的 ASCII 查询必须和中文两字词走同一条兜底路径——实现时最容易只给中文做兜底，然后在英文环境里翻车。

### 4.3 去重与置顶

指纹命中存活条目时**不新增行**，执行：

```sql
UPDATE items
   SET last_used_at = :now,
       use_count    = use_count + 1,
       created_at   = :now      -- 置顶到时间线最前
 WHERE fingerprint = :fp AND deleted_at IS NULL;
```

`created_at` 被复用为排序键（列表按 `created_at DESC`）。`first_seen_at` 单独保留首次复制时间供统计用。

---

## 5. 过期与生命周期

### 5.1 三级优先级

```
① items.expires_at 显式设定（单条右键「到此时间点过期」）
        ↓ 未设定
② categories.ttl_seconds（分类默认策略，如「临时」= 1 小时）
        ↓ 未设定
③ settings['retention.defaultTtlSec']（全局默认，默认 30 天）
        ↓ 未设定
④ NULL → 永不过期
```

- **`pinned = 1` 强制凌驾全部规则**，收藏内容永不回收
- 最终计算结果写回 `expires_at`，并把来源记入 `ttl_source`，UI 才能解释清"它为什么 6 天后会消失"
- 修改分类 TTL 时需批量重算该分类下 `ttl_source = 'category'` 的条目（`ttl_source = 'item'` 的单条显式设定不动）

### 5.2 GC 流程

每 `retention.gcIntervalSec`（默认 60s）执行一次，导出期间暂停。

```
1. 保护     UPDATE items SET expires_at = NULL, ttl_source='never'
             WHERE pinned = 1 AND expires_at IS NOT NULL
2. 到期回收  expires_at <= now AND deleted_at IS NULL
             → 按 retention.onExpire 处理：
                 "trash"  → 写 deleted_at = now（默认）
                 "delete" → 直接物理删除
                 "archive"→ 移入指定分类并把 expires_at 置 NULL
3. 硬删     deleted_at <= now - retention.trashTtlSec（默认 7 天）
             → 删行 + 删关联 blob 文件（含缩略图）
4. 条数淘汰  存活条数 > retention.maxItems（默认 2000）
             → 按 (pinned ASC, last_used_at ASC) 淘汰到限额内
5. 容量淘汰  blobs/ 总大小 > retention.maxDiskBytes（默认 500 MB）
             → 同上，优先淘汰大体积图片
6. 一致性    VACUUM；扫描 blobs/ 中的孤儿文件（无 items 引用且 mtime > 1 天）删除
```

**第 2 步的软删除不能省。** 剪贴板历史属于"误删代价极高"的数据，一次直接物理删除就会让用户彻底不再信任这个工具。

### 5.3 blob 存储布局

```
blobs/
├─ 9f/2a/9f2a1c...e4.png              # sha256 前 2 位 / 第 3-4 位分片
├─ 9f/2a/9f2a1c...e4.thumb.png
└─ 3b/07/3b07d9...11.rtf
```

按前 4 位十六进制分两级，最多 65536 个目录，避免单目录堆积万级文件拖慢 Windows 上的目录枚举。写入采用**临时文件 + `rename` 原子替换**，防止崩溃留下半截文件。

---

## 6. 备份与迁移包

完整规范见 **`docs/BACKUP-FORMAT.md`**。要点：

- 扩展名 `.clipbak`，容器为 **ZIP**（不像 `tar.zst` 那样需要额外工具，双击即可用系统解压器查看，符合"数据主权"主张）
- 内部结构：`manifest.json` 或 `manifest.yaml` + `blobs/<a>/<b>/<sha256>.<ext>` + `README.txt`
- 压缩策略：清单用 deflate；blob 中**文本类（RTF/HTML）用 deflate，图片/音视频用 store**（已压缩内容再压是纯浪费时间）
- **导出全程流式**：内存占用与条目数无关（O(1)），一次只在内存里持有一条记录 + 一个 64 KB 块
- **时间语义**：同时导出绝对时间 `expiresAt` 与相对 `ttlSeconds`，导入方二选一，默认按绝对时间（避免迁移后凭空续命）
- **ID 重映射**：导入绝不能沿用原 ID，必须建立 `旧 id → 新 id` 映射并重写 `categoryId` / `tagIds`
- **幂等**：靠指纹唯一索引，同一包重复导入不会产生重复数据
- **可回滚**：整批导入带 `import_id`，支持一键撤销上次导入

---

## 7. macOS 实现要点

| # | 问题 | 处理 |
|---|---|---|
| 1 | 无剪贴板变更通知 API | 轮询 `NSPasteboard.general.changeCount`。**自适应间隔**：用 `CGEventSource.secondsSinceLastEventType` 检测空闲，活跃 0.2s、空闲 > 60s 降到 1.0s |
| 2 | 来源 App 判定时机 | 轮询时前台 App 可能已切换。必须在检测到变更的**同一帧**读 `NSWorkspace.shared.frontmostApplication`，否则来源永远错 |
| 3 | 自动粘贴抢焦点 | 面板必须**真正创建** `NSPanel`（`NSWindowStyleMaskNonactivatingPanel`）并覆写 `canBecomeKeyWindow = true`；同时 App 必须跑在 Accessory 激活策略下。**两个条件缺一不可**。显示顺序必须是 `orderFrontRegardless` → `makeKeyWindow`。**⚠️ 不要用 `object_setClass` 事后换类，实测必崩**。完整方案与实测数据见 §0.2 与 `poc/POC-RESULT.md` |
| 4 | 自动粘贴权限 | 需 `AXIsProcessTrusted()`。流程：写剪贴板 → 记录旧内容 → `CGEvent` 模拟 ⌘V → 延迟 `ui.restoreDelayMs`（默认 250ms）恢复旧剪贴板。**⚠️ TCC 授权绑定代码签名**：ad-hoc 签名每次构建 cdhash 都变，系统视为新 App，**每次重新编译都要重新授权**（见 §14 F 组） |
| 5 | 剪贴板恢复副作用 | 部分应用异步读剪贴板，恢复太快会拿到错误内容。提供开关 `ui.restoreClipboard`（默认 true） |
| 6 | 分发方式 | **不做 Developer ID 签名、不做公证**（见 §0）。走 ad-hoc 签名 + README 说明 Gatekeeper 绕过；**不上架 App Store**（沙箱下无法任意落盘、全局热键受限） |
| 7 | 隐藏 Dock 图标 | 必须把激活策略设为 Accessory。两条路径：① `build/darwin/Info.plist` 加 `LSUIElement = true`（**推荐**，进程启动前生效，避免开机抢一次焦点）；② cgo 调 `NSApp.setActivationPolicy(NSApplicationActivationPolicyAccessory)`。**注意：这不只是"隐藏 Dock 图标"的美观需求——M0 实测证明它是免抢焦点面板的必要条件**（见 §0.2） |
| 8 | 通用二进制 | `wails build -platform darwin/universal`；同时设 `LSMinimumSystemVersion = 12.0` |
| 9 | 数据库损坏 | WAL + `clean_shutdown` 标记文件（正常退出删除；启动时**发现标记存在才**跑 `PRAGMA integrity_check`，见 §14 第 6 条）+ 每日备份到 `backups/` 保留 7 份 |

---

## 8. Windows 实现要点

| # | 问题 | 处理 |
|---|---|---|
| 1 | 需要一个窗口来收消息 | 在**独立线程**创建 message-only 窗口（`HWND_MESSAGE`）并跑自己的消息泵，`AddClipboardFormatListener` 挂在它上面。不要复用 Wails 主窗口的 hwnd，避免和 UI 线程的事件循环耦合 |
| 2 | `OpenClipboard` 会失败 | 剪贴板是全局互斥资源，其他程序占用时返回 `ERROR_ACCESS_DENIED`。**必须重试**：3 次 × 20ms 退避，仍失败则记为一次失败并跳过 |
| 3 | 事件早于数据就绪 | `WM_CLIPBOARDUPDATE` 可能在所有者写完全部格式前就到达，读取前延迟 25ms |
| 4 | UIPI 阻断自动粘贴 | 目标进程以管理员权限运行、而本程序没有时，`SendInput` 会被静默丢弃。启动时检测目标进程完整性级别，不一致则降级为"只复制到剪贴板"并在 UI 明示原因 |
| 5 | 面板抢焦点 | 窗口样式加 `WS_EX_NOACTIVATE`；不要调用 `SetForegroundWindow`。**Wails v2 的操作路径**：在 `windows.Options` 里设 `WindowClassName`（默认 `wailsWindow`），启动后 `FindWindow(className, nil)` 拿到 HWND，再用 `SetWindowLongPtr(hwnd, GWL_EXSTYLE, ...\|WS_EX_NOACTIVATE)`。Windows 侧不需要 cgo |
| 6 | 图片格式 | 只有 `CF_DIBV5`，**必须手动转 PNG**（DIB 每行按 4 字节对齐，且带 alpha 通道处理，容易踩空） |
| 7 | HTML 头部 | `"HTML Format"` 带 `Version:0.9 / StartHTML / EndHTML / StartFragment / EndFragment` 头部，必须按偏移量剥离出真正的 HTML 片段 |
| 8 | 自身写入触发 OS 剪贴板历史 | 写回时附加 `CanIncludeInClipboardHistory`(DWORD=0) 与 `CanUploadToCloudClipboard`(DWORD=0)，避免把工具内部操作写进系统剪贴板历史和云剪贴板 |
| 9 | WebView2 依赖 | `embedBootstrapper` 安装模式；首次启动检测运行时缺失则提示 |
| 10 | 安装器选型 | 用 NSIS，不用 MSIX（MSIX 需签名且带沙箱限制，与本地数据库路径自由冲突） |

---

## 9. 设置项清单

### 9.0 设置的存储归属（唯一真源）

**SQLite `settings` 表是运行时设置的唯一真源**（§4.1 已建该表）。配置文件只承担"打开数据库之前就必须读到"的引导项，两者职责不重叠、不存在双写。

只保留一个极小的 TOML 作为**引导配置**（`%APPDATA%\PawClip\config.toml` / `~/Library/Application Support/PawClip/config.toml`），并且只允许放"打开数据库之前就必须读到"的三项：

| 引导项 | 默认 | 为什么必须在 TOML |
|---|---|---|
| `database.path` | 平台默认目录下的 `pawclip.db` | 决定数据库开在哪——读它的时候库还不存在 |
| `ui.language` | `system` | 首屏文案要在建库前就能定 |
| `log.level` | `info` | 建库失败时仍然需要日志 |

**其余所有 key（下表全部）一律存 `settings` 表**，UI 改设置即写库，`config.toml` 不参与。

TOML 解析用 `github.com/BurntSushi/toml`（约 100 KB、无传递依赖）。**不要引入 YAML 解析库**——YAML 只用于导出清单，那里有自研流式发射器（见 §6）。

下表 `key` 均指 `settings.key`，`value` 列为 JSON 编码。

| key | 默认值 | 说明 |
|---|---|---|
| `capture.enabled` | `true` | 捕获总开关 |
| `capture.types` | `["text","image","files"]` | 捕获的内容类型 |
| `capture.imageMaxBytes` | `10485760` | 超过 10 MB 的图片不记录 |
| `capture.textMaxChars` | `262144` | 超长文本截断存储（保留前 256K 字符） |
| `capture.pollIntervalActiveMs` | `200` | macOS 专用 |
| `capture.pollIntervalIdleMs` | `1000` | macOS 专用 |
| `capture.idleThresholdSec` | `60` | macOS 专用，判定空闲的阈值 |
| `capture.debounceMs` | `120` | 连续变更合并窗口 |
| `exclude.apps` | `["com.1password.*","com.apple.keychainaccess","com.agilebits.*"]` | 应用黑名单，支持通配 |
| `exclude.privateTypes` | `true` | 跳过带保密标记的内容 |
| `retention.defaultTtlSec` | `2592000` | 全局默认 30 天 |
| `retention.onExpire` | `"trash"` | `trash` / `delete` / `archive` |
| `retention.trashTtlSec` | `604800` | 回收站保留 7 天 |
| `retention.maxItems` | `2000` | 存活条数上限 |
| `retention.maxDiskBytes` | `524288000` | blob 目录 500 MB 上限 |
| `retention.gcIntervalSec` | `60` | GC 周期 |
| `ui.hotkey` | `"CmdOrCtrl+Shift+V"` | 全局呼出热键。**注意**：全局注册会**吞掉所有 App 里的 ⌘⇧V**（即"粘贴并匹配样式"），设置界面必须显式提示，或默认改用不冲突的组合 |
| `ui.pasteMode` | `"clipboard"` | `clipboard`（只复制）/ `autoPaste`（自动粘贴） |
| `ui.restoreClipboard` | `true` | 自动粘贴后恢复原剪贴板 |
| `ui.restoreDelayMs` | `250` | 恢复延迟 |
| `ui.windowIdleDestroySec` | `300` | 面板静默多久后销毁以释放内存 |
| `ui.quickPasteCount` | `9` | ⌘/Ctrl + 1..N 直贴 |
| `ui.language` | `"system"` | `system` / `zh-CN` / `en`。解析顺序：本机设置 → 系统区域 → 回退 `en` |
| `ui.theme` | `"system"` | `light` / `dark` / `system` |
| `storage.cleanShutdownMarker` | `true` | 正常退出时删除标记文件，启动时发现标记存在才做完整性检查 |
| `storage.walCheckpointEvery` | `1000` | 每 N 次写入或每 1 小时执行 `wal_checkpoint(TRUNCATE)` |
| `backup.manifestFormat` | `"json"` | `json` / `yaml` |
| `backup.includeExpired` | `false` | 导出是否包含已过期条目 |

---

## 10. 目录结构

```
pawclip/
├─ frontend/                            # React + Vite + TypeScript
│  ├─ src/
│  │  ├─ views/        panel · settings · categories · tags · backup · stats
│  │  ├─ components/   list · searchbar · preview · ttl-badge · category-tree
│  │  ├─ i18n/         zh-CN.ts · en.ts
│  │  └─ main.tsx
│  └─ vite.config.ts
├─ clipboard/                           # 平台原生剪贴板层
│  ├─ backend.go                        # Backend 接口 + Tick / Raw / Payload
│  ├─ darwin.go
│  ├─ pasteboard_darwin.m               # cgo Objective-C shim
│  ├─ pasteboard_darwin.h
│  ├─ windows.go                        # AddClipboardFormatListener
│  ├─ linux.go                          # 预留骨架，不参与编译
│  ├─ normalize.go                      # DIB→PNG、CF_HTML 剥离、类型归并
│  ├─ filter.go                         # 保密标记 / 应用黑名单 / 类型开关
│  └─ guard.go                          # SelfWriteGuard
├─ store/
│  ├─ schema.go                         # 建表 + user_version 迁移
│  ├─ items.go  categories.go  tags.go  settings.go
│  ├─ fts.go                            # trigram + LIKE 兜底两段式
│  └─ blobs.go                          # 分片路径 + 原子写 + 缩略图
├─ retention/gc.go
├─ backup/
│  └─ manifest.go  writer.go  reader.go  yaml.go
├─ writer.go                            # 回写 + 自动粘贴
├─ autostart.go
├─ tray.go
├─ app.go                               # Wails App 绑定（前端可调方法都在这）
├─ config.go
├─ i18n.go                              # 后端字符串（托盘 / 通知 / 报告）
├─ main.go
├─ go.mod
├─ wails.json
├─ build/
│  └─ appicon.png                       # 由 assets/icon/dist/pawclip-1024.png 复制
├─ assets/icon/
│  ├─ pawclip-source.png                # 成品图标稿（822×782，无 alpha）
│  └─ dist/                             # 生成物，见 §15
├─ scripts/
│  ├─ build.sh                          # 唯一构建入口（钉死 -tags sqlite_fts5）
│  ├─ accept.sh                         # 成品真机验收
│  ├─ demo-m1.sh                        # 捕获链路演示
│  ├─ build-icons.cjs                   # 图标资源构建
│  └─ probe-icon-source.cjs             # 源图几何探测（换图后必跑）
├─ .github/workflows/                   # ci.yml + release.yml
├─ docs/                                # 开发文档（与产品源码分离）
│  ├─ DESIGN.md                         # 本文件
│  ├─ BACKUP-FORMAT.md                  # .clipbak 格式规范
│  └─ HANDOFF-PROMPT.md                 # 新会话开工提示词
└─ README.md                            # 面向使用者：功能 / 构建 / 使用
```

> 开发文档统一放 `docs/`，仓库根只留 `README.md`。源码注释里引用设计条款一律写成
> `docs/DESIGN.md §N`，保证从任意目录都能直接定位到文件。

### 构建命令

| 目的 | 命令 |
|---|---|
| 开发（热重载） | `wails dev` |
| macOS 通用二进制 | `scripts/build.sh -platform darwin/universal` |
| Windows + NSIS 安装包 | `scripts/build.sh -platform windows/amd64 -nsis` |
| 体积优化 | `go build -trimpath -ldflags="-s -w"` |
| 重建图标 | `NODE_PATH=<node_modules> node scripts/build-icons.cjs` |
| 重新探测源图几何 | `NODE_PATH=<node_modules> node scripts/probe-icon-source.cjs assets/icon/pawclip-source.png` |

> 构建一律走 `scripts/build.sh` 而不是直接 `wails build`：FTS5 依赖 `sqlite_fts5`
> 构建标签，而 `wails.json` 没有任何字段能持久化 Go 构建标签，漏掉它产物里的
> 全文检索会**静默降级**成逐行匹配（只打一行 WARN，进程照常启动）。

---

## 11. 功能分期

### P0 · MVP（能自己天天用）

- 监听文本 / 图片 / 文件，跨平台后端抽象到位
- SQLite 落库 + SHA-256 去重置顶
- 全局热键呼出**免抢焦点**面板；闲置销毁释放内存
- FTS5 + trigram 检索，两字词 LIKE 兜底
- 回车回写剪贴板；⌘/Ctrl + 1..9 直贴
- 删除、清空、置顶
- 应用黑名单 + 保密类型跳过 + 自写入守卫
- 托盘图标 + 开机自启
- macOS ad-hoc 签名产物 + Windows NSIS 安装包（**均不做公证与代码签名**，见 §0）

### P1 · 可用性补全

- 三级 TTL 过期 + 回收站 + `onExpire` 三种行为
- 分类管理：增删改、配色、图标、**自动归类规则**
- 标签系统（多对多）
- 图片缩略图 + 空格快速预览
- 批量操作：多选删除 / 改分类 / 改过期
- `.clipbak` 导出导入（JSON/YAML 可选、选择性导出、一键回滚）
- 存储统计面板（条数 / 占用 / Top 来源应用）

### P2 · 效率增强

- 连续粘贴模式（多选排队，逐个消费）
- 拼音首字母搜索（`zgd` → "文档"）
- 搜索语法：`类型:图片 应用:Chrome 今天 报告`
- 内容转换器：去格式贴纯文本 / JSON 美化 / Base64 / URL 去 `utm_*` / 大小写 / 去空行
- 正则提取：从长文本批量抽邮箱、手机号、链接，直接产出多行结果
- 图片 OCR 提取文字
- 颜色值识别（`#RRGGBB` / `rgb()` 渲染色块）
- 片段库（Snippets，支持 `${date}` `${clipboard}` 占位符）
- 时间轴视图

### P3 · 可选

- 隐私暂停（一键停录 5 / 15 / 60 分钟）
- 敏感内容识别（身份证 / 银行卡 / 手机号 → 自动缩短 TTL 或高亮）
- 本地数据库加密（SQLCipher + 主密码）
- Linux 后端落地（X11 XFixes + Wayland `wlr-data-control`）

---

## 12. 验收指标

| 项 | 指标 |
|---|---|
| 捕获延迟 | 文本 < 30 ms；5 MB 图片从复制到落库 < 200 ms |
| 检索延迟 | 3 字以上 < 50 ms @ 10 万条；2 字 LIKE < 300 ms |
| 空闲内存 | macOS ≤ 30 MB；Windows ≤ 25 MB（面板销毁态） |
| 空闲 CPU | macOS < 1%；Windows ≈ 0% |
| 无自捕获 | 连续 100 次面板回贴，历史条目数增加为 0 |
| 去重正确性 | 同一内容复制 100 次，库中始终 1 行，`use_count = 100` |
| 搜索正确性 | 1 / 2 / 3 字中文、英文、中英混排、大小写、特殊字符全部命中符合预期 |
| 导出完整性 | 导出→清库→导入，条目数、内容、分类、标签、置顶、过期时间全部一致 |
| 导入幂等 | 同一包导入两次，第二次全部走跳过，库中无重复 |
| 断电安全 | 捕获过程中强杀进程，重启后库可正常打开且无半截记录 |

---

## 13. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| macOS 轮询功耗 | 笔记本续航下降，用户卸载 | 自适应间隔；提供"暂停记录"开关 |
| WebView 内存偏高 | 常驻内存超预期 | 面板闲置销毁；Windows 叠加 `TrySuspendAsync`；把 `windowIdleDestroySec` 暴露给用户 |
| 辅助功能授权被拒（mac） | 自动粘贴不可用 | 优雅降级为"只复制"，UI 明示原因与开启路径 |
| UIPI 阻断（win） | 管理员窗口内粘贴失败 | 检测完整性级别，降级并提示 |
| 剪贴板恢复副作用 | 个别 App 粘贴内容错误 | 提供关闭恢复的开关，默认延迟 250 ms |
| 中文搜索失灵 | 核心功能不可用 | trigram + 两字 LIKE 兜底，单测强制覆盖 |
| FTS5 未编入 SQLite | 搜索直接报错 | 启动自检 `SELECT * FROM items_fts LIMIT 1`，失败则整体退化为 LIKE 模式 |
| 大图片撑爆磁盘 | 占用失控 | 单条大小上限 + 容量上限淘汰 + 统计面板可视化 |
| `.clipbak` 恶意包 | ZIP Slip / 解压炸弹 | 解压前校验条目名（拒绝 `..` 与绝对路径）与声明的解压总大小上限 |
| 无签名导致首次打开被拦截 | 下载者以为程序损坏 | README 写清 Gatekeeper / SmartScreen 绕过步骤；macOS 产物必须确认带 ad-hoc 签名，否则 Apple Silicon 上内核直接拒绝执行 |
| Linux 后端差异大 | 未来扩展成本 | 本期只留 `Backend` 接口骨架，不投入实现 |

---

## 14. 优化清单（按收益排序）

### A. IPC 传输 —— 最容易踩且代价最大的坑

1. **列表只传元数据，图片绝不走 IPC。** 图片/缩略图通过自定义协议直接交给 WebView 加载（Wails 静态资源服务）。把 100 张缩略图 base64 塞进 IPC 会让首屏卡 1 秒以上、内存翻倍。
2. **搜索输入防抖 120 ms，并取消上一次未完成的查询**（请求序号比对或 `AbortController`）。用户快速输入时并发打 FTS 会拖垮响应。

### B. 数据库

3. **分页用 keyset 而非 OFFSET**：`WHERE created_at < :last ORDER BY created_at DESC LIMIT 50`。OFFSET 在万级时会线性变慢。
4. **合批提交**。捕获线程收到事件后不立刻 commit，攒 50 ms 或 20 条一起提交，显著减少 fsync 次数。
5. **WAL 治理**。每 1000 次写入或每小时 `PRAGMA wal_checkpoint(TRUNCATE)`，否则 WAL 文件会涨到几百 MB。
6. **启动时不要每次都做 `PRAGMA integrity_check`**（万级库要几秒）。改为标记文件方案：正常退出时删除标记，启动时发现标记存在才做完整性检查。
7. **列表查询绝不 `SELECT text_content`**，只取 `preview`（前 200 字符）。否则 2000 条全文进内存能到几十 MB。
8. **FTS5 必须保留默认 `detail=full`，不要用 `detail=none` / `detail=column`。** 这两个选项与 trigram 分词器**不兼容**：建表与回填都能成功，但查询会直接抛 `OperationalError`——实测 `MATCH '中文文档'`、`MATCH 'hello'` 均报错，只有 3 字符以上的纯 ASCII 查询侥幸命中。（SQLite 3.50.4 实测）

### C. 内存

9. **轮询热路径上绝不分配。** macOS 每 0.2 s 只读一个 `NSInteger changeCount`，不要构造 `NSArray` / `NSString` 等 Objective-C 对象。Go 里尤其注意循环内不要 `defer`、不要新建结构体。这是决定空闲 CPU 与内存增长率的关键。
10. **验证"销毁"是真的。** 面板窗口销毁后要 log 一次实际 RSS，确认 WebView 子进程（Windows 多个 WebView2 进程 / macOS `com.apple.WebKit.WebContent`）确实退出。否则销毁只是假象。
11. Windows 复用 `TrySuspendAsync` 时要注意：挂起后再恢复有延迟，不要用在"刚关闭立刻再打开"的场景。

### D. 前端体积

12. 不用 UI 组件库；不用 `react-router` / `vue-router`（视图切换用一个状态变量即可）；不用 Redux / Pinia（`useState` / `reactive` 足够）。
13. 调高 Vite `assetsInlineLimit`；图标用内联 SVG 而非图标字体。
14. 目标：前端产物 gzip 后 < 150 KB。

### E. 捕获正确性

15. **缩略图在捕获时同步生成**（长边 160 px，`nfnt/resize` 或 `disintegration/imaging`，10 MB 图约 20–40 ms）。异步生成会让列表出现缩略图空窗，体验明显变差。
16. **超大文本截断存储但要保留完整指纹**：对完整内容做 sha256，只截断存进 `text_content` 的前 256K 字符。否则去重会漏。
17. `filter.go` 的顺序应为：保密标记 → 应用黑名单 → 类型开关 → 自写入守卫 → 尺寸上限。**把最便宜的检查放最前面**，避免对大图片做无用功。

### F. 无签名分发的具体操作

18. **macOS 产物必须带 ad-hoc 签名**，否则 Apple Silicon 上内核直接拒绝执行。`go build` 链接出的**可执行文件**自带 ad-hoc 签名，但 **`.app` 包本身仍需显式签**：`codesign --sign - --force --deep PawClip.app`。
    - **⚠️ ad-hoc 的副作用（开发期必踩）**：TCC 辅助功能授权绑定代码签名，ad-hoc 每次构建 cdhash 都变，macOS 视为另一个 App → **每次重新编译都要重新授权**。缓解办法是生成一张**固定的自签证书**（Keychain 自建，免费）并改用它签名，cdhash 稳定后只需授权一次。这不违反 §0"不买证书"的前提。
19. 下载后 Gatekeeper 拦截，README 写明：`xattr -dr com.apple.quarantine /Applications/PawClip.app`。
20. Windows SmartScreen → "更多信息" → "仍要运行"。
21. GitHub Actions 矩阵：`macos-14`（arm64）与 `macos-13`（x86_64）分别构建，或在单个 `macos-14` runner 上直接 `wails build -platform darwin/universal` 出通用二进制；`windows-2022` 出 NSIS 安装包。
22. `.gitignore` 排除 `build/bin/`（Wails 产物）、`frontend/dist/`、`node_modules/`、`*.dmg` / `*.exe` / `*.msi`、`*.clipbak`、本地 `pawclip.db*` 与 `blobs/`。**发布产物只进 Release，不进仓库。**
    - **例外**：`assets/icon/dist/`（约 3.3 MB）**要入库**——它是 Wails 的构建**输入**（appicon + 托盘图都从这里取），不是发布产物。入库才能保证 `git clone && wails build` 开箱即用、不依赖 Node 工具链。若将来嫌体积大，可改为忽略整个目录并在构建前跑一次 `node scripts/build-icons.cjs`（§15.4）。

### G. i18n 落地

23. 前端 `react-i18next` / `vue-i18n`；后端一个极小的 `t(key)` + 两张语言 JSON，编译期嵌入。
24. **易漏清单**：托盘菜单项、通知标题与正文、导入/导出结果报告、错误提示、备份包内 `README.txt`、导出文件名中的分类名。

---

## 15. 品牌与图标资源

### 15.1 命名与标识

| 项 | 值 |
|---|---|
| 中文名 | 喵喵贴 |
| 英文名 | PawClip（Paw 猫爪 + Clip 剪贴） |
| 命名逻辑 | 猫爪抓取剪贴历史，暗示"快速拾取复制内容"；可爱但不幼稚，适配开发者 / 上班族工具 |
| 图形 | 剪贴板轮廓 + 猫脸 + 绿色对勾，深灰线稿 + 绿色点缀 |
| macOS bundle id | `com.pawclip.app` |
| Windows AppUserModelID | `PawClip.Clipboard` |
| 仓库名 | `pawclip` |
| 配置 / 数据目录 | macOS `~/Library/Application Support/PawClip`；Windows `%APPDATA%\PawClip`（内含 `pawclip.db` + `blobs/` + `thumbs/`） |

### 15.2 源图与几何常量

图标源是**成品位图** `assets/icon/pawclip-source.png`（822×782），不是矢量稿，所以构建脚本依赖从源图实测出的几何常量：

| 常量 | 值 | 含义 |
|---|---|---|
| `CROP` | `{left:49, top:16, width:737, height:744}` | 圆角方块外接框 |
| 方块圆角半径 | ≈129.6 px（占宽度 17.6%） | 实测值 |
| `MASK_RADIUS_RATIO` | `0.19` | 遮罩圆角半径占比（实测值 + 1.4% 余量） |
| `APP_FILL` | `824` | Apple 图标栅格：圆角方块占 1024 画布的 824（80.47%） |

**换图后必须重跑 `scripts/probe-icon-source.cjs` 并更新这些常量**，否则遮罩会切错位置。

### 15.3 三个必须知道的坑

**坑一：源图没有 alpha 通道。** 四角是不透明近白（250,250,248），直接用会得到一张铺满全幅的白方块，而不是圆角图标。必须人工生成 alpha 遮罩。

**坑二：不能用灰度阈值或洪泛填充抠背景。** 方块之外不是纯白，而是一圈**柔和的环境阴影**——左上角灰度 250–252，右下角低至 212；而方块填充是 244–250。**阴影比填充更暗，两者灰度区间重叠**，于是：

| 天真方案 | 实际后果 |
|---|---|
| 全局阈值 `lum < 251` 判为方块 | 右下角的深色阴影被当成方块内部保留下来 |
| 从四角洪泛填充背景 | 四角灰度本身就是 251 / 250 / 248 / 241，达不到阈值，**一个像素都填不出去**（实测背景占比 0.00%） |
| 反向阈值判背景 | 剪贴板内部的白色（255）被误判为背景而挖空 |

唯一可靠的手段是**按几何形状生成圆角矩形遮罩**。

**坑三：遮罩半径必须 ≥ 源图实际半径。** 源图圆角比同半径正圆略"紧"（45° 方向上靠内约 3–4px）。遮罩半径若小于源图，四角会漏出背景或阴影——右下角漏出的还是**深色阴影**，在深色桌面上非常刺眼。故取 19%（= 源图 17.6% + 1.4% 余量），代价是圆角略圆，视觉上无法分辨。

**验收方式**：`dist/preview-mask-check.png` 把抠好的方块叠在品红底上做四角放大——**任何品红穿透都说明遮罩没切干净**。

### 15.4 构建产物

| 文件 | 用途 |
|---|---|
| `pawclip-master.png` | 1024² 主图（824 方块 + 透明留白），所有尺寸的母版 |
| `pawclip-{16,24,32,48,64,128,256,512,1024}.png` | 通用 PNG 集 |
| `pawclip.icns` | macOS，10 档 iconset（16→1024，含 @2x） |
| `pawclip.ico` | Windows，7 档内嵌 PNG（16/24/32/48/64/128/256） |
| `tray/trayTemplate{,@2x,@3x,@4x}.png` | macOS 菜单栏模板图（16/32/48/64） |
| `tray/tray.ico` | Windows 通知区 |
| `preview-icon.png` | 尺寸阶梯 + 明暗底对比 + 16px 四倍放大 |
| `preview-dock.png` | Dock 相对尺寸模拟（校验 80.47% 栅格占比） |
| `preview-tray.png` | 托盘图明暗栏效果（含深色栏自动反转模拟） |
| `preview-mask-check.png` | 遮罩穿透校验 |

### 15.5 托盘图：由主图派生单色模板

托盘图**不单独设计**，直接从主图推导：取已抠好的圆角方块，按亮度→alpha 映射成纯黑 + alpha 的模板图（`TRAY_BG_LUM=242` 把浅灰卡片视为背景→透明，只留下深灰线稿与绿色对勾，并乘 `TRAY_BOOST=1.7` 让灰度≈130 的对勾也达到全不透明）。

选择理由：**单一图形语言**。Dock / 启动台 / 菜单栏 / 通知区用同一个标记，不会出现"启动台是剪贴板+猫脸、菜单栏是爪印方块"的割裂。

代价是 16px 下细节会糊——但实际渲染尺寸不是 16px：macOS 菜单栏是 16pt **@2x = 32px**（Retina 起步），该档位下剪贴板轮廓与猫脸均清晰可辨；16px 档（`trayTemplate.png`）只用于非 Retina 与 Windows 小图标场景，作为兜底可接受。

> 若未来实测发现 16px 确实不可用，正确做法是**从同一张主图简化轮廓**（加粗线稿、去掉对勾等细节），而不是另起一套图形语言。

**macOS 模板图注意**：模板图只由 alpha 承载形状，颜色由系统按菜单栏明暗渲染（浅色栏黑、深色栏白）。所以源图必须是**纯黑 + alpha**；做成彩色会在深色菜单栏下完全不可见。`preview-tray.png` 的深色栏已按系统反转后的真实效果渲染。

### 15.6 已知观感问题

图标主调是浅灰卡片（填充 244–250）配深灰线稿，**在浅色背景（浅色桌面 / 浅色 Dock / 白色网页）上对比度极低**——实测 48px 以下几乎只剩线稿可见；深色背景下表现很好。

这是浅底卡片式图标的固有特性，不是构建问题。若想改善，最小改动是给卡片加 1px 描边或加重投影，但会偏离当前设计，暂不做。

---

## 16. 里程碑与起步顺序

M0 是门禁，M1–M4 每步都产出**可独立验证**的产物。不要跳步把 UI 全做完再联调。

### M0 · 技术门禁 —— ✅ **已完成（2026-09-15）**

| # | 目标 | 状态 |
|---|---|---|
| 1 | 冻结工程骨架：`git init` + `.gitignore` + Go module + Wails 脚手架 + React/Vite | 文档、图标资源、`git` 已就绪；Go module 与 Wails 脚手架随 M1 建立 |
| 2 | 解决免抢焦点面板（§0.2） | ✅ **已通过实测**（三次复现）。方案 = 真正创建 NSPanel 并接管 contentView + Accessory 激活策略。详见 `poc/POC-RESULT.md` |

### M1 · 捕获链路 + 落库（纯后端，无 UI —— 先跑通正确性）

**交付**：`clipboard/` 双平台实现 + `store/` schema 与写入路径 + 单写 goroutine（合批提交）+ `SelfWriteGuard`。

**验收**（§12 节选）：
- 「连续 100 次回贴，历史条数增加为 0」——此时还没有面板，用代码直接循环调 `Write()` 模拟
- 「同一内容复制 100 次，库中始终 1 行且 `use_count = 100`」
- 「捕获过程中强杀进程，重启后库可正常打开且无半截记录」
- 手工抽查：文本 / 图片 / 文件三类都能落库，图片缩略图正确

**建议实现顺序**：schema → 写入路径 → **Windows 后端**（事件驱动、不涉及轮询，先跑通更省事）→ macOS 后端（轮询 + 自适应间隔 + 同帧读前台 App）→ 过滤与守卫。

### M2 · 检索 + 面板（第一个能天天用的版本）

**交付**：FTS5 + trigram + LIKE 兜底（§4.2）、keyset 分页、Wails 绑定方法、面板 UI（列表 / 搜索 / 预览 / ⌘1..9）、全局热键、托盘。

**验收**：
- 「搜索正确性」全表通过（1/2/3 字中文、英文、中英混排、大小写、特殊字符）
- 检索延迟 < 50 ms @ 10 万条（写脚本灌数据实测）
- 面板闲置销毁后实测 RSS 达标（macOS ≤ 30 MB / Windows ≤ 25 MB），**并且确认 WebView 子进程真的退出**（§14 第 10 条——销毁可能是假象）

**⚠️ 面板实现必须处理的两个遗留点（源自 M0 实测）**：

1. **Wails 启动时会激活一次 App** —— 实测首次显示窗口时 `frontmost` 变成自己。用 `StartHidden: true` + 首次呼出才显示，避免开机抢一次焦点。
2. **Wails 仍持有原窗口引用** —— 它后续的 `SetSize` / `SetTitle` 等调用会作用在已被掏空、隐藏的原窗口上。面板尺寸必须走我们自己的 NSPanel，M2 需逐项确认这些调用对面板无影响。

### M3 · 生命周期与数据出口

**交付**：三级 TTL + 回收站 + GC、分类与标签、`.clipbak` 导出导入 + 一键回滚、统计面板、开机自启。

**验收**：§12 的「导出完整性」+「导入幂等」两项。

### M4 · 打磨与发布

**交付**：i18n 补全（§14 第 24 条那 6 个易漏位置）、内容转换器、连续粘贴、拼音首字母搜索、GitHub Actions 矩阵构建 + Release。

**验收**：两台机器真机走一遍「下载 → 绕过 Gatekeeper / SmartScreen → 使用 → 导出 → 换机导入」。

---

## 附录 A：Go 平台层实现要点

### A.1 两个平台的接入方式

**macOS：cgo + 一个极薄的 Objective-C 桥**

```objc
// pasteboard_darwin.m
#import <AppKit/AppKit.h>

long cb_change_count(void) {
    return [NSPasteboard generalPasteboard].changeCount;
}
```

```go
//go:build darwin && cgo
package clipboard

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation
#include "pasteboard_darwin.h"
*/
import "C"
```

约 150 行 `.m` 文件暴露 C 接口即可。

免 cgo 的替代方案是 `github.com/ebitengine/purego`，它能直接调用 `objc_msgSend`，纯 Go 且可交叉编译。但每次消息发送都要手写类型编码，冗长易错，**不推荐**——既然本来就要为 Windows 写平台分支，多一个 `.m` 文件不算负担。

**Windows：完全不需要 cgo**

`golang.org/x/sys/windows` + `syscall.NewCallback` 就能创建 message-only 窗口、注册 `AddClipboardFormatListener`、读 `CF_DIBV5` / `CF_HDROP`，全程无需 cgo。

### A.2 不要用的库

**不要用 `golang.design/x/clipboard`。** 它只暴露 `text` / `image`，**不暴露 `changeCount`、不暴露原始 pasteboard types**。而我们需要 `changeCount` 做变更检测、需要原始 types 判断 `org.nspasteboard.ConcealedType` 保密标记、还需要 HTML / RTF。平台层必须自己写。

**不要用 `modernc.org/sqlite`。** 纯 Go 免 cgo，但会给二进制**再加约 8–10 MB**。选 `mattn/go-sqlite3`（cgo，+2–3 MB）；**FTS5 必须加 `sqlite_fts5` 构建标签**才能启用，trigram 分词器随 FTS5 一起可用。

### A.3 语言无关性

§2 的 `Backend` 接口抽象、§3 平台能力对照、§4 数据模型、§5 生命周期、`docs/BACKUP-FORMAT.md` 全部与语言无关——只有 `clipboard/` 与 `store/` 两个目录是语言相关的实现。这是换语言成本可控的原因。

