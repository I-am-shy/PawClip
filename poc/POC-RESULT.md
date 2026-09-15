# M0 门禁验证报告：Wails v2 能否做免抢焦点面板

**日期**：2026-09-15
**结论**：✅ **可行**。但必须走一条特定路径，且需要两个条件同时成立。
**验证工程**：`poc/wails-panel/`（Wails v2.16.0 + Go 1.26.3 + cgo）

---

## 1. 验证目标

`DESIGN.md §0.1` 把"热键呼出**免抢焦点**面板 + ⌘1..9 直贴"列为 P0 核心体验，但它依赖一个 Wails v2 官方不支持的能力：

- `mac.Options` 只暴露 `TitleBar` / `Appearance` / `WebviewIsTransparent` / `WindowIsTranslucent` / `ContentProtection` / `About`——**没有任何窗口类控制**。
- macOS 上要同时满足"窗口能成为 key（否则搜不了词）"和"不激活本 App"，只有 `NSPanel` + `NSWindowStyleMaskNonactivatingPanel` 能做到。

本验证回答：**用 cgo 在运行时改造窗口，能不能补上这个能力？**

---

## 2. 验证环境

| 项 | 值 |
|---|---|
| macOS | 14.1.1 (23B81) |
| 芯片 | Apple M1 / arm64 |
| Go | 1.26.3 |
| Wails | v2.16.0 |
| Xcode CLT | 已装（cgo 必需） |
| 窗口对象真实类 | `NSKVONotifying_WailsWindow`（488 字节） |
| `NSWindow` / `NSPanel` 实例大小 | 448 / 448 字节 |

---

## 3. 方法：五组对照

PoC 用 `-mode=` 切换，每组独立进程运行：

| 模式 | 做法 |
|---|---|
| `baseline` | 原样 Wails 窗口，不做任何处理（对照组） |
| `mask-only` | 只给 `NSWindow` 加 `nonactivatingPanel` 样式位 |
| `panel` | `object_setClass` 换成 NSPanel 子类 + 样式位 + 覆写 `canBecomeKey` |
| `panel-min` | 同上，但跳过 NSPanel 专有方法（用于定位崩溃点） |
| `adopt` | **新建真 NSPanel，接管 Wails 的 contentView** |

另有 `-accessory` 开关，控制是否把激活策略设为 `NSApplicationActivationPolicyAccessory`。

每组的探测流程：

```
A0  setup 完成
A1  orderFrontRegardless + makeKeyWindow（不 activate）
A2  显式 [NSApp activateIgnoringOtherApps:YES] 后再取 key
A3  把前台让给 Finder —— 先看 key 会不会被系统收走
A4  在 App 非激活状态下取 key  ← 真实使用路径
A5  连续 6 次采样，看状态能否稳定持住
```

---

## 4. 结果

### 4.1 五种模式的对比

| 模式 | 窗口类 | A4 键盘焦点 | A5 稳定性 | 抢走前台 | 结论 |
|---|---|---|---|---|---|
| `baseline` | `WailsWindow` | ✗ | 0/6 | 否 | 不可用 |
| `mask-only` | `WailsWindow` | ✗ | 0/6 | 否 | **被 AppKit 拒绝** |
| `panel` | `PawClipPanel` | — | — | — | **SIGTRAP 崩溃** |
| `panel-min` | `PawClipPanel` | — | — | — | **SIGTRAP 崩溃** |
| `adopt`（无 accessory） | `PawClipPanel` | ✗ | 0/6 | 否 | 拿不到键盘 |
| **`adopt` + accessory** | `PawClipPanel` | **✓** | **6/6** | **0/6** | ✅ **PASS** |

### 4.2 `adopt` + Accessory 的详细轨迹（三次复现一致）

```
A0  active=true   key=false  visible=false
A1  active=true   key=true   visible=true   前台=wails-panel
A2  active=true   key=true   visible=true   前台=wails-panel
A3  active=false  key=false  visible=true   前台=访达
A4  active=true   key=true   visible=true   前台=访达   ← 关键
A5  6 次采样：有焦点且未抢前台 6/6，抢走前台 0/6
```

`styleMask = 128`（仅 `nonactivatingPanel`，borderless 不占位），实例大小 448 字节。

---

## 5. 三条被否定的路（含根因）

### 5.1 给普通 `NSWindow` 加样式位 —— AppKit 直接拒绝

系统打印：

```
NSWindow does not support nonactivating panel styleMask 0x80
```

且 `styleMask` **实际未被修改**：设置前后都是 `32780`（`0x800C`），bit 7 仍是 0。

> 根因：该 mask 的语义要求实例本身是 `NSPanel`。

### 5.2 `object_setClass` 换类 —— SIGTRAP 崩溃

类名换成功了（`setup` 全程无异常，`class=PawClipPanel`，能显示、能取 key），但随后任何 `orderOut` 都会触发 `SIGTRAP`。

> 根因：Wails 窗口的真实类是 **`NSKVONotifying_WailsWindow`**（KVO 动态子类），在 NSWindow 之后带 `userMinSize` / `userMaxSize` 两个 `NSSize` ivar（488 − 448 = 40 字节）。
> `NSPanel` 的 ivar 布局与这块内存**重叠**。调用 `setFloatingPanel:` / `setBecomesKeyOnlyIfNeeded:` 时，NSPanel 会读写自己的 ivar，实际写坏的是 Wails 的尺寸约束。
> 崩溃点被逐步日志精确定位在 `setup complete` 之后的 `orderOut`，`panel` 与 `panel-min` 两组崩溃位置一致，说明元凶是换类本身，不是某个 NSPanel 方法。

### 5.3 `makeKeyAndOrderFront:` 兜底 —— 会真的激活 App

加上兜底后出现 `key=true`，但同一时刻 `active=true`；0.7 秒后系统纠正回 `active=false` 并把 key 一起收走。

> 根因：它改变了激活状态，违反 nonactivating 语义。

---

## 6. 一条重要的认知修正

### 判断"有没有抢焦点"不能看 `NSApp.isActive`

Accessory 策略下面板持有键盘焦点时，`isActive` 是 **`true`** —— 但 `NSWorkspace.frontmostApplication` 仍是原来那个 App，**菜单栏没有被抢走**。

**正确判据是 `frontmostApplication` 有没有被换成本 App。**

初版 PoC 把 `active == false` 当成通过条件，因此把正确答案误判成 FAIL。修正判据后同一份数据立刻显示 PASS。

---

## 7. 两条实现顺序约束

1. **`makeKeyWindow` 必须在 `orderFrontRegardless` 之后调用。** 窗口不在屏幕上时 `makeKeyWindow` 是空操作；顺序反了会得到一个"可见但收不到键盘"的面板。
2. **激活策略要尽早设置**，最好在窗口创建之前。

---

## 8. 遗留问题（交给 M2）

1. **Wails 启动时会激活一次 App**（A1 时 `frontmost` 变成自己）。需要用 `StartHidden: true` + 首次呼出才显示，避免开机抢一次焦点。
2. **Wails 仍持有原窗口引用**，它后续的 `SetSize` / `SetTitle` 等调用会作用在隐藏的空窗口上。面板尺寸需走我们自己的 NSPanel，或确认这些调用对面板无影响。

---

## 9. 复现步骤

```bash
cd poc/wails-panel
export PATH="$HOME/go/bin:$PATH"

# 构建
wails build -platform darwin/arm64

# 关键一组（期望 PASS）
PAWCLIP_PROBE_OUT=/tmp/probe.json \
  ./build/bin/wails-panel.app/Contents/MacOS/wails-panel -mode=adopt -accessory

# 对照组：去掉 accessory 应当立刻失败
PAWCLIP_PROBE_OUT=/tmp/probe2.json \
  ./build/bin/wails-panel.app/Contents/MacOS/wails-panel -mode=adopt
```

其余模式：`-mode=baseline | mask | panel | panelmin`。
运行期间窗口会自动跑完探测流程、打印结论、5 秒后退出。

---

## 10. 对设计文档的影响

| 位置 | 变更 |
|---|---|
| `DESIGN.md §0.1` | 从"⚠️ 未验证，阻塞项"改为"✅ 实测通过"，写入采用方案与三条被否定的路 |
| `DESIGN.md §0 技术栈` | **不变**，维持 Go + Wails v2，无需上 v3 alpha |
| `DESIGN.md §7` #3 | 面板实现要点改为引用本报告 |
| `DESIGN.md §17 M2` | 增加上述两个遗留问题 |

**M0 门禁关闭，可以开工 M1。**
