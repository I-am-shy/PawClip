# PawClip · 喵喵贴

跨平台（macOS + Windows）**单机**剪贴板历史管理器。**无账号、无同步、无云端。**

复制过的东西不用再翻窗口找第二遍：一个热键呼出面板，搜一下，回车就贴回去。
所有数据落在你自己的磁盘上（SQLite + blob 文件），可一键导出成第三方工具也能读的 `.clipbak` 包。

```
⌘⇧V 呼出 · 输入即搜 · ⏎ 回贴
```

---

## 功能

### 记录

- **文本 / 图片 / 文件**三类统一进历史：纯文本、HTML、RTF、PNG、以及在访达/资源管理器里复制的多个文件。
- **去重置顶**：同一内容复制 100 次，库里始终 **1 行**、使用次数 **100**。
- **默认不记敏感内容**：跳过带「请勿记录」标记的剪贴板内容（密码管理器的通行做法），
  并默认排除 1Password / 钥匙串这类应用（名单可在设置里改）。
- **超长 / 超大兜底**：图片超过 10 MB 不记录；文本超过 262144 字符只截断存储，
  但**指纹按完整内容计算**，所以截断不会造成误合并。

### 查找

- **中文能搜**：FTS5 trigram 全文检索；1–2 字（`文档`、`AI`、`5G`）自动走 `LIKE` 兜底，
  不会出现"两字词永远搜不到"。
- **拼音首字母**：输入 `wdgl` 能搜到「文档管理」，中英混排（`zimwd` → 「ZIM 文档」）也行。
- **筛选**：类型**单选**（全部 / 文本 / 图片 / 文件，选哪类就只显示哪类）、只看置顶、回收站。
- **搜不到时能问为什么**：搜索框旁的面板会告诉你这次走的是全文检索还是降级匹配。

### 取用

- **免抢焦点（macOS）**：`⌘⇧V` 呼出的面板**不会**夺走你正在打字的窗口焦点，贴回去不打断手头的事。
  > Windows 上 Win32 没有等价能力（`WS_EX_NOACTIVATE` 的窗口收不到键盘输入），
  > 所以那边会取一次前台，粘贴完成后把前台还给原窗口——这是平台的差异，不是取舍。
- **回贴**：`⏎` 回贴当前行，`⌘/Ctrl + 1..9` 直贴第 N 行（条数可配），双击条目也行。
- **右键菜单**：条目上右键 → **复制 / 查看**。
- **查看浮层**：右键「查看」弹出只读的全屏视图——文本可滚动、可选中，
  `⌘/Ctrl + 滚轮` 缩放字号；图片滚轮缩放（以指针为中心）、拖动平移、双击复位、
  一键「适应窗口」，大图打开时自动先给全貌。Esc 关闭。
- **每次呼出都回到历史页**：面板重新亮起时总是干净的剪贴板历史——
  不会停在上次的设置页 / 回收站 / 搜索结果上，搜索词与多选也会清掉。
- **两种粘贴模式**：默认「只复制到剪贴板」，可改成「直接粘贴到前台应用」。
  粘贴后默认**恢复你原来的剪贴板**（延迟可调）。
- **连续粘贴**：多选若干条排成队列，切到目标应用后每次 `⌘/Ctrl + ⏎` 取一条，按顺序贴完。
- **内容转换器**：JSON 美化/压缩、去空行、URL 去跟踪参数、Base64 编解码、大小写转换，
  外加「粘贴纯文本」（剥掉网页带进来的 HTML / RTF 格式）。
- **用完自己走**：点到面板以外的任何地方（别的应用、桌面、别的窗口）面板自动收起，
  静默一段时间也会收起；想让它留在屏幕上，把设置里的「点击面板以外时自动收起」关掉。

### 整理

- 置顶、批量多选（`⌘/Ctrl + 点击`）：删除 / 恢复 / 设过期时间。
- **三级保留策略**：默认条目留 30 天 → 到期移入**回收站** → 回收站再留 7 天；
  另有「最多 2000 条 / 最多 500 MB」的容量上限与后台回收。到期行为可配为
  移入回收站 / 直接删除 / 保留内容但不再自动过期。
- **统计面板**：条目数、实际磁盘占用、来源应用 Top、回收轮次，以及本进程的常驻内存实测。

### 数据与系统

- **导出 / 导入 `.clipbak`**：ZIP 容器 + 纯文本清单，第三方工具也能读。可整库导出、可只导出收藏、
  可嵌入图片文件。导入前有**预检**（重复 / 已过期 / 格式非法都会先列出来），
  冲突策略（合并 / 跳过 / 覆盖）与过期策略（保留 / 重算 / 清除）可选，导入后还能**回滚**。
- **断电安全**：捕获途中强杀进程，重启会走「标记 → 完整性检查 → 无悬空文件」。
- **中英双语**；外观跟随系统 / 浅色 / 深色。
- **菜单栏图标 + 开机自启**（macOS 走 LaunchAgent，Windows 写 HKCU 的 Run 键，都不需要提权）。

尚未实现的（隐私暂停、敏感内容识别、数据库加密、OCR、片段库、Linux 后端）见
[`docs/README.md`](docs/README.md) 的功能分期。

---

## 安装

分发方式是 **Releases 里的压缩包 / 安装程序**（仓库里的流水线在打 tag 时自动产出）；
也可以自己构建，见下一节。

### macOS

1. 解压 `PawClip-<版本>-macos-universal.zip`，把 `PawClip.app` 拖进「应用程序」。
2. **首次打开会被 Gatekeeper 拦下**——产物是 ad-hoc 签名、未做公证（本项目的既定取舍：
   不买 Apple 证书）。两种绕法任选：

   ```bash
   xattr -dr com.apple.quarantine /Applications/PawClip.app
   ```

   或者在访达里**右键 → 打开**，再点一次「打开」。
3. 想让 `⏎` 直接贴进前台应用，需要授权：
   **系统设置 → 隐私与安全性 → 辅助功能** 里勾选 PawClip。
   不授权也能用，只是降级成「只复制到剪贴板」（面板里会如实说明）。

### Windows

1. 运行 NSIS 安装程序。
2. SmartScreen 提示时点「更多信息」→「仍要运行」（同样未做代码签名）。
3. 自动粘贴**不需要**额外授权。

---

## 怎么用

### 呼出面板

- 默认全局热键 **`⌘⇧V`**（Windows 上是 `Ctrl+Shift+V`），可在设置里改。
- 改热键**不用背键名写法**：点设置里的热键框，直接按下想要的组合——组合先写进
  草稿、点「确认」才生效；「取消」（或 Esc）放弃本次，清空后确认即取消快捷键。
  输入的那几秒全局热键会临时让出，不然系统会把按键拦走、界面毫无反应。
  界面上的提示（搜索框里的"⌘⇧V 呼出"）跟着设置走；清空后不再提热键，
  只能从托盘呼出。
- 或点菜单栏 / 通知区域的 PawClip 图标 → **显示 PawClip**。

> ⚠️ 全局热键会吞掉所有应用里的这个组合——`⌘⇧V` 在很多编辑器里是「粘贴并匹配样式」。
> 冲突的话在设置里换一个；换的时候如果新组合被别的程序占用，设置不会生效、旧热键保持原样。

### 面板键位

下表里的 `⌘` 在 Windows 上是 `Ctrl`。

| 键 | 行为 |
|---|---|
| `↑` `↓` | 上下移动光标行 |
| `⏎` | **回贴**当前行（按上面选的粘贴模式） |
| `⌘ + 1..9` | 直贴第 N 行（条数在设置里配，默认 9） |
| `⌘ + ⏎` | 连续粘贴队列：取下一项 |
| `空格` | 预览当前行（焦点不在输入框时才生效） |
| `⌫` / `Delete` | 删除当前行；在回收站视图里是**彻底删除** |
| `Esc` | 逐级退出：关预览 → 清搜索与选择 → 收起面板 |
| `⌘ + 点击` | 多选 |
| `⌘ + ,` | 打开设置 |

### 菜单栏 / 托盘菜单

| 项 | 行为 |
|---|---|
| 显示 PawClip | 呼出面板 |
| 暂停记录 / 继续记录 | 勾选态表示**当前已暂停**；暂停时剪贴板不再进历史 |
| 设置… / 统计… | 打开对应视图 |
| 导出 / 导入… | `.clipbak` 的导出导入 |
| 关于 PawClip | 版本号 |
| 退出 | 正常退出（会清掉 `clean_shutdown` 标记） |

### 数据与配置放在哪

| 平台 | 目录 |
|---|---|
| macOS | `~/Library/Application Support/PawClip/` |
| Windows | `%AppData%\PawClip\` |

目录里就四样东西：

| 文件 | 内容 |
|---|---|
| `pawclip.db` | SQLite 库（历史条目、分类、标签、设置） |
| `blobs/` | 图片与 RTF 的原始文件，按 sha256 前两位分片存放 |
| `config.toml` | **引导配置**，只有三项：库路径、界面语言、日志级别 |
| `clean_shutdown` | 正常退出时删除的标记；残留即代表上次异常退出 |

> 除这三项以外的所有设置（捕获类型、排除应用、保留策略、热键、主题……）都保存在数据库里、
> 由界面修改，**不会**回写到 `config.toml`。设置页底部有「在访达 / 资源管理器中打开」的入口。

---

## 隐私

- **不加密**：历史以明文 SQLite + blob 存放，本机任何进程都能读到。这是刻意的取舍
  （见 [`docs/DESIGN.md`](docs/DESIGN.md) §0 的已锁定决策）；要挡的话请用磁盘加密（FileVault / BitLocker）。
- **不联网**：无账号、无同步、无遥测、无自动更新。唯一的网络活动是你自己去下载新版本。
- **不记来源不明的内容**：带「请勿记录」标记的剪贴板内容（密码管理器的标准做法）直接跳过。
- **不自我循环**：从面板回贴的内容不会被再记一次——连续回贴 100 次，历史条目数不变。

---

## 从源码构建

### 环境要求

| 项 | 要求 |
|---|---|
| Go | 1.26+ |
| Wails CLI | v2.16+（`go install github.com/wailsapp/wails/v2/cmd/wails@latest`） |
| Xcode CLT | 必需（macOS 侧 cgo 编译） |
| Node | 20+（仅前端构建需要；图标产物已入库，纯 Go 侧构建不需要） |

### 本地开发（改代码时看效果）

**能直接热重载开发，不必每次打包安装。** 一条命令：

```bash
scripts/dev.sh            # 启动开发模式（带热重载）
scripts/dev.sh -browser   # 额外参数透传给 wails dev
```

起来之后：

| 你改了什么 | 会发生什么 |
|---|---|
| 前端 `frontend/src/**` | Vite HMR，存盘即生效，**不用重启** |
| Go `**/*.go` | Wails 自动重新编译并重启 App（默认只监听 `.go`） |

两个本地端口：

| 地址 | 是什么 |
|---|---|
| `http://localhost:5173` | Vite 的站点，只有前端资源 |
| `http://localhost:34115` | Wails dev server：**在浏览器里也能调绑定的 Go 方法**，调 UI 交互时很方便 |

> 浏览器里没有原生部分——`NSPanel`、`⌘⇧V` 热键、托盘只有 App 进程里才有。
> 面板行为（免抢焦点、失焦收起）必须在 App 里验。
>
> **没有 Wails 也要能调界面**：直接开 `http://localhost:5173/harness.html`。
> 它先塞一份假的 `window.go`（设置/列表都是内存数据，`SetSetting` 会记进
> `window.__log`，还带三条假的文本/图片/文件数据与 `window.__emit` 事件入口）
> 再渲染应用，专门用来验"纯前端"的交互——热键录制、右键菜单、查看浮层、
> 提示与设置的联动就是用它验的。它是 dev-only 资产，`vite build` 不会打进产物。

**为什么开发也要走 `scripts/dev.sh`，不能直接 `wails dev`：** 同样是那个构建标签——
不带 `sqlite_fts5` 时全文检索会**静默降级**成逐行匹配，于是开发时最容易得出的错误结论
就是"搜索怎么感觉不准"，然后去改检索代码，而真正的问题是构建标签。另外 `wails dev`
默认读写**真实数据目录**（`~/Library/Application Support/PawClip`），开发时反复重启会
一边写你的真实剪贴板历史、一边把 debug 日志刷满真实剪贴板内容；包装脚本默认给一个隔离
的运行目录（`.workbuddy/tmp/pawclip-dev/`）。想对着真实数据调试就
`PAWCLIP_DEV_REAL=1 scripts/dev.sh`。

<details>
<summary>启动时那行 <code>flag provided but not defined: -config</code> 是正常的</summary>

`wails dev` 用 `-appargs` 把参数转给 App，同时它自己的 dev flagset 也会去解析
`os.Args`。Wails 源码在那处的注释写的是 "Parse args but ignore errors in case
`-appargs` was used to pass in args for the app" ——也就是说这个报错是**有意容忍**的，
发生在你的 `-config` 已经生效之后，不影响运行。确认方式：日志里能看到
`db=…/.workbuddy/tmp/pawclip-dev/pawclip.db`。

</details>

> 只想改样式、不需要后端数据时，也可以 `cd frontend && npm run dev` 单独跑 Vite。
> 但拿不到绑定的 Go 方法（列表/搜索都是空的），日常开发还是用 `scripts/dev.sh`。

### 打包

```bash
# 构建 .app —— 一定用这个包装脚本，别直接 wails build
scripts/build.sh                                     # 默认 darwin/arm64
scripts/build.sh -platform darwin/universal           # 通用二进制（Intel + Apple Silicon）
scripts/build.sh -platform windows/amd64 -nsis        # Windows + 安装程序
scripts/build.sh -platform darwin/universal -clean    # 清干净重来

# 打包成可分发压缩包
ditto -c -k --sequesterRsrc --keepParent build/bin/pawclip.app build/dist/PawClip-macos-universal.zip
```

### 测试与验收

```bash
test/run.sh                    # 一条命令跑全部：格式 → 静态检查 → 单测 → 前端检查
test/run.sh --short            # 跳过规模测试（10 万条检索延迟、5 MB 图片）
test/run.sh --build --accept   # 发布前的完整口径：重新出包 + 对产物做真机验收
```

`test/run.sh --help` 是权威用法，分层说明与"为什么 Go 单测不在 `test/` 目录"见
[`test/README.md`](test/README.md)。**`accept.sh` 会覆盖你当前的系统剪贴板。**

**为什么要用 `test/run.sh` 而不是手敲 `go test`：** 它跑的每条命令都和 CI 一模一样
（同一个标签、同一个 `-p 1`、同一个 `-count=1`）。本机绿了 CI 就该绿——"我本地明明是好的"
这类分歧，多半来自两边参数不一致。

**为什么构建必须走 `scripts/build.sh`：**
FTS5 全文索引依赖 `sqlite_fts5` 构建标签，而 `wails.json` 的 schema **没有**任何字段能持久化
Go 构建标签（只有 CLI 的 `-tags`）。直接 `wails build` 出来的产物缺 fts5 模块，程序会按设计
**优雅降级**——打一行 WARN、退回逐行匹配，界面照常打开。也就是说**全文检索会静默失效**，
不查日志根本发现不了。包装脚本把标签钉死，`.github/workflows/` 里的两个流水线同理。

**为什么测试要带 `-p 1`：** 仓库里有两类按墙上时间取证的规模测试（10 万条检索延迟、
5MB 图片捕获延迟）。`go test` 默认并行跑包，CPU 被抢满时这些数字会飙到 2–3 倍——那是机器被占满，
不是产品退化。串行让度量条件稳定，总耗时基本不变。

**`test/accept.sh` 与 `go test` 是两件事，缺一不可：**
单测证明「逻辑正确」（注入假后端、可重复）；`test/accept.sh` 证明「这个产物真能跑」——
签名坏了、`Info.plist` 丢了 `LSUIElement`、构建漏了标签，这些**一条单测都不会红**，
但用户一定打不开或功能少一半。实测结果见 [`docs/ACCEPTANCE.md`](docs/ACCEPTANCE.md)。

---

## 目录结构

```
pawclip/
├─ main.go / app.go / bindings.go      # Wails 入口、生命周期、前端可调方法
├─ capture/                            # 捕获流水线（去抖 → 归一 → 去重 → 落库）
├─ clipboard/                          # 平台原生剪贴板层（cgo shim / Win32 消息）
├─ store/                              # SQLite：schema、迁移、items、FTS5、blob、设置
├─ retention/  backup/  transform/  pinyin/   # 过期回收、.clipbak 导出导入、转换器、拼音
├─ writer.go / tray.go / autostart.go / i18n.go   # 回贴、托盘、开机自启、后端文案
├─ panel/                              # 免抢焦点面板与全局热键的平台实现
├─ frontend/                           # React + Vite + TypeScript（无 UI 组件库）
├─ assets/icon/                        # 图标源图与生成物
├─ scripts/                            # build.sh（构建）· dev.sh（本地开发）· 图标工具
├─ test/                               # run.sh（总入口）· accept.sh（产物验收）· check-i18n.mjs
├─ poc/                                # M0 技术门禁验证（含可直接复用的面板 cgo 代码）
├─ docs/                               # 开发文档：设计规格 / 格式规范 / 验收报告
└─ README.md                           # 本文件：功能 · 安装 · 使用 · 构建
```

> `*_test.go` 仍与被测的包**同目录**（Go 工具链的要求，也是访问包内未导出符号的前提），
> 所以 `test/` 里没有单测；它收的是可独立执行的测试资产。详见 [`test/README.md`](test/README.md)。

---

## 已知限制

| 项 | 现状 | 影响 |
|---|---|---|
| 空闲内存 54 MB（目标 30 MB） | 面板"闲置销毁"在 Wails v2.16 的单窗口模型下做不了（只导出 `Show/Hide/Quit`，无窗口重建 API），当前实现是**闲置收起**而不是销毁 | 面板收起后 WebView 内存仍在。这是 §12 里唯一未达标的指标，归因见 [`docs/ACCEPTANCE.md`](docs/ACCEPTANCE.md) |
| Windows 只在 CI 构建，未在真机验证 | 有 `CGO_ENABLED=0` 交叉编译烟测与 `windows-2022` 上的正式构建 | 能证明"编得出来"；剪贴板 / 热键 / 托盘的真机行为未实测 |
| 未做代码签名与公证 | 既定取舍（不买证书） | 首次打开要手动绕过 Gatekeeper / SmartScreen |
| Linux | 只有接口骨架，不参与编译 | 按设计边界，本期不做 |
| 无自动更新 | 只从 Releases 手动下载 | 靠用户自己留意新版本 |

---

## 开发文档

| 文件 | 内容 |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | **唯一权威设计规格**（16 节 + 附录 A）：架构、数据模型与 DDL、过期策略、双平台踩坑、里程碑、优化清单 |
| [`docs/README.md`](docs/README.md) | 开发文档索引：技术栈、已锁定决策、功能分期、开工三坑、构建与验收口径 |
| [`docs/ACCEPTANCE.md`](docs/ACCEPTANCE.md) | §12 全项验收的实测结果、归因与已知遗留 |
| [`test/README.md`](test/README.md) | 测试分层与验收口径：各层证明什么、`accept.sh` 的三种结论、三条硬口径 |
| [`docs/BACKUP-FORMAT.md`](docs/BACKUP-FORMAT.md) | `.clipbak` 备份包格式规范（含第三方消费指南） |
| [`docs/HANDOFF-PROMPT.md`](docs/HANDOFF-PROMPT.md) | M1 开工时的提示词存档（历史文件） |
| `poc/` | M0 技术门禁验证：报告 + 可运行工程 |
| `.workbuddy/memory/` | 决策与踩坑的工作日志 |

---

## 技术栈

Go 1.26 · Wails v2 · React + Vite + TypeScript · SQLite（`mattn/go-sqlite3` + FTS5 trigram）；
macOS 走 cgo + Objective-C shim，Windows 走 `golang.org/x/sys/windows`（无需 cgo）。
选型理由与被排除的路线见 [`docs/DESIGN.md`](docs/DESIGN.md) §0.4。
