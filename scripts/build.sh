#!/usr/bin/env bash
# PawClip 构建入口 —— 唯一正确的构建方式。
#
# 用 bash 而不是 zsh：脚本内容本来就只有 POSIX/bash 的构造，但 shebang 写 zsh
# 会让它在**任何没有 zsh 的环境**里直接不可执行——Windows runner 就是这种情况
# （2026-09-18 发 v0.1.0 时 Windows 的构建以"步骤报绿、产物不存在"的假绿形态失败）。
# bash 在 macOS / Linux / Windows(Git Bash) 上都存在，是这里唯一安全的选择。
#
# 为什么需要这个包装脚本，而不能直接 `wails build`：
#
#   store/schema.go 里的 FTS5 全文索引（docs/HANDOFF-PROMPT.md §4 第 7 条要求）
#   依赖 mattn/go-sqlite3 的 `sqlite_fts5` 构建标签。不带这个标签编译出来的
#   二进制里没有 fts5 模块，`CREATE VIRTUAL TABLE ... USING fts5` 会失败。
#
#   而 store.Open 的处理是**优雅降级**：打一行 WARN、退回 LIKE 检索、进程照常
#   启动。这是对的（开发时不至于开不起库），但代价是——**你不可能发现**。
#   实测证明：直接 `wails build -platform darwin/arm64` 出来的 App，启动日志是
#
#       level=WARN msg="FTS5 unavailable, search will fall back to LIKE"
#       ... fts5=false
#
#   也就是说全文检索在正式产物里是**静默失效**的。
#
#   更麻烦的是 wails.json 的 schema（config.v2.json）**没有**任何可以持久化
#   Go 构建标签的字段，只有 CLI 的 `-tags`。所以没法在配置里一劳永逸，
#   只能靠一个"唯一的构建入口"把这件事钉死。
#
# 用法：
#   scripts/build.sh                          # 默认 darwin/arm64
#   scripts/build.sh -platform windows/amd64  # 透传任意 wails build 参数
#
set -euo pipefail

cd "$(dirname "$0")/.."

# wails 的位置：先查 PATH（`go install` 的落地目录一般在 PATH 里，
# GitHub Actions 的 runner 也会把 $(go env GOPATH)/bin 放进 PATH），
# 再回退到默认 GOPATH/bin。
#
# 为什么不能只写死 `$HOME/go/bin/wails`：Windows 上 `go install` 的产物
# 是 wails.exe，而 Git Bash 的 `command -v` 对**带路径**的形式不会自动补
# 后缀（只对 PATH 查找补），于是写死的路径在那里恒为"不存在"。
# 显式把两个候选都列出来，三个平台才能用同一份脚本。
WAILS="${WAILS:-}"
if [ -z "$WAILS" ]; then
  WAILS="$(command -v wails 2>/dev/null || true)"
fi
_resolved=""
for cand in "$WAILS" "$HOME/go/bin/wails" "$HOME/go/bin/wails.exe"; do
  if [ -n "$cand" ] && [ -x "$cand" ]; then
    _resolved="$cand"
    break
  fi
done
WAILS="$_resolved"
if [ -z "$WAILS" ]; then
  echo "找不到 wails（已试：PATH、\$HOME/go/bin/wails、\$HOME/go/bin/wails.exe）" >&2
  echo "安装：go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2
  exit 1
fi

# ── 版本号注入 ────────────────────────────────────────────────────
#
# 版本号原先有两份且互不相干：Info.plist 的 CFBundleShortVersionString 来自
# wails.json 的 info.productVersion，而 Go 侧 main.Version（config.go）是另一个
# 手写常量。两份不一致的表现是：启动日志与关于窗口说 0.1.0-m1，系统信息里却是
# 0.1.0 —— 用户报问题时完全无法确认他手上是哪个构建。
#
# 这里把 wails.json 的 productVersion 当**唯一真源**注入进去，允许 VERSION
# 环境变量覆盖（release.yml 用 tag 号覆盖，这样发布包报的是真实版本）。
VERSION="${VERSION:-$(sed -n 's/.*"productVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' wails.json | head -1)}"
if [ -z "$VERSION" ]; then
  echo "无法从 wails.json 读出 info.productVersion" >&2
  exit 1
fi

# FTS5 标签必须在最前面，用户追加的参数排后面（同名参数后者覆盖前者，
# 所以这里也顺手防了一手：如果用户自己带了 -tags，下面的检查会拦下来）。
exec "$WAILS" build -tags sqlite_fts5 -ldflags "-X main.Version=$VERSION" "$@"
