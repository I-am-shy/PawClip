# poc/ —— M0 技术门禁验证

回答一个问题：**Wails v2 能不能做出"免抢焦点面板"？**（macOS 上既要能打字、又不夺走前台 App）

答案是**能**，但必须走一条特定路径。这里是验证工程与完整报告。

| 文件 | 内容 |
|---|---|
| **`POC-RESULT.md`** | 完整报告：五组对照的原始数据、三条被否定路线的根因、复现步骤 |
| `wails-panel/` | 最小验证工程（Wails v2.16.0 + Go + cgo + Objective-C） |

**结论摘要**：真正创建 `NSPanel` 并接管 contentView + App 运行在 Accessory 激活策略下，**两个条件缺一不可**。实测面板持有键盘焦点 6/6 次，系统前台被抢走 0/6 次（三次独立复现一致）。详见 `POC-RESULT.md`。

---

## 这个工程对 M1/M2 的价值

不是一次性用完就丢的草稿，里面有两部分值得直接复用：

**1. `panel_darwin.m`（314 行）—— 面板实现，M2 直接复用**
包含调对顺序的完整创建流程（`orderFrontRegardless` → `makeKeyWindow`，顺序反了会得到"可见但收不到键盘"的面板）、`canBecomeKeyWindow` 覆写、以及三条错误路线的规避注释。

**2. `probe.go`（241 行）—— 探测方法，M2 验收时复用**
用 `NSWorkspace.frontmostApplication` 而非 `NSApp.isActive` 判断有没有抢焦点（判据写错会把正确答案误判成失败）。M2 做完面板后可以用同一套逻辑验收。

平台无关部分（`clipboard/`、`store/`）不在本工程内——那是 M1 的交付物。本工程只验证窗口层。

---

## 构建与复现

```bash
cd poc/wails-panel
export PATH="$HOME/go/bin:$PATH"        # wails CLI（go install 装在这里）

wails build -platform darwin/arm64      # 首次会重新生成 build/ 模板目录

# 关键一组（期望 PASS）
PAWCLIP_PROBE_OUT=/tmp/probe.json \
  ./build/bin/wails-panel.app/Contents/MacOS/wails-panel -mode=adopt -accessory

# 对照组：去掉 -accessory 应当立刻失败（证明两个条件缺一不可）
PAWCLIP_PROBE_OUT=/tmp/probe2.json \
  ./build/bin/wails-panel.app/Contents/MacOS/wails-panel -mode=adopt
```

其余模式：`-mode=baseline | mask | panel | panelmin`。
运行时会自动跑完探测流程、在窗口里打印结论、约 12 秒后自动退出（期间会切一次 Finder）。

> `frontend/` 是纯静态前端（无 npm 步骤，`wails.json` 里 install/build 均为空），因此**构建不需要 Node**。
