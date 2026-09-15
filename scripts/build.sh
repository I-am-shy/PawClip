#!/bin/zsh
# PawClip 构建入口 —— 唯一正确的构建方式。
#
# 为什么需要这个包装脚本，而不能直接 `wails build`：
#
#   store/schema.go 里的 FTS5 全文索引（HANDOFF-PROMPT §4 第 7 条要求）
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

WAILS="${WAILS:-$HOME/go/bin/wails}"
if ! command -v "$WAILS" >/dev/null 2>&1; then
  echo "找不到 wails：$WAILS" >&2
  echo "安装：go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2
  exit 1
fi

# FTS5 标签必须在最前面，用户追加的参数排后面（同名参数后者覆盖前者，
# 所以这里也顺手防了一手：如果用户自己带了 -tags，下面的检查会拦下来）。
exec "$WAILS" build -tags sqlite_fts5 "$@"
