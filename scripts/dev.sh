#!/bin/zsh
# PawClip 本地开发入口 —— 带热重载跑起来，用于改代码时看效果。
#
# 为什么需要这个包装脚本，而不能直接 `wails dev`：
#
#   和 scripts/build.sh 是同一个理由，而且这里更容易踩——`wails dev` 同样需要
#   `-tags sqlite_fts5`。不带标签时 store.Open 会**优雅降级**：打一行 WARN、
#   退回 LIKE 检索、进程照常起来。于是"全文检索在开发时其实是坏的"这件事
#   只有在你去翻启动日志时才会发现；而开发时最容易得出的错误结论就是
#   "搜索怎么感觉不准" → 去改检索代码，而真正的问题是构建标签。
#
#   第二个理由：`wails dev` 默认读**真实数据目录**
#   （~/Library/Application Support/PawClip）。开发时反复重启，会一边写你的
#   真实剪贴板历史、一边把 debug 级日志刷满真实剪贴板内容。所以本脚本默认给
#   一个隔离的运行目录（仓库内 .workbuddy/tmp/pawclip-dev，已被 .gitignore 覆盖）。
#   想对着真实数据调试时：PAWCLIP_DEV_REAL=1 scripts/dev.sh
#
# 用法：
#   scripts/dev.sh                  # 热重载启动（隔离数据目录）
#   scripts/dev.sh -browser         # 额外参数透传给 wails dev
#   PAWCLIP_DEV_REAL=1 scripts/dev.sh
#
# 起来之后：
#   · 前端改动（frontend/src/**）→ Vite HMR，改完立刻生效，不用重启。
#   · Go 改动（**/*.go）        → Wails 重新编译并重启 App（默认监听 .go）。
#   · http://localhost:5173     → Vite 的站点（只有前端资源）。
#   · http://localhost:34115    → Wails dev server：在浏览器里也能调绑定的
#                                 Go 方法，适合调 UI 交互。
#   · 原生部分（NSPanel、⌘⇧V 热键、托盘）只有 App 进程里才有，浏览器里没有。
#
# ⚠️ 启动日志里会出现一行
#       flag provided but not defined: -config
#       Usage of dev: -assetdir … -loglevel …
#   这是 Wails 自己的 dev flagset 在解析 os.Args 时的有意行为（它的源码注释
#   写的是 "Parse args but ignore errors in case -appargs was used to pass in
#   args for the app"），**不是我们传错了参数**。它发生在你的 -config 已经
#   生效之后，不影响运行。留意"日志里真有 db=<隔离路径>"即可确认。

set -euo pipefail

cd "$(dirname "$0")/.."

WAILS="${WAILS:-$HOME/go/bin/wails}"
if ! command -v "$WAILS" >/dev/null 2>&1; then
  echo "找不到 wails：$WAILS" >&2
  echo "安装：go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2
  exit 1
fi

WAILS_ARGS=(-tags sqlite_fts5)

if [ "${PAWCLIP_DEV_REAL:-0}" = "1" ]; then
  echo "⚠️  PAWCLIP_DEV_REAL=1：这次会读写你的**真实数据目录**"
  echo "    （~/Library/Application Support/PawClip）——调试完记得别把测试数据留在里面。"
else
  WORK="$PWD/.workbuddy/tmp/pawclip-dev"
  CFG="$WORK/config.toml"
  mkdir -p "$WORK"

  # 只在缺失时生成，不覆盖：这个文件是拿来手动调参数的（比如临时开 trace 级日志、
  # 换语言），每次启动都重写会让那些调整莫名其妙地丢。
  if [ ! -f "$CFG" ]; then
    cat > "$CFG" <<TOML
# scripts/dev.sh 自动生成的**开发用**配置（只生成一次，之后随你改）。
# 数据全在仓库内的 .workbuddy/tmp/pawclip-dev/，与真实数据目录无关。
[database]
path = "$WORK/pawclip.db"
[ui]
language = "zh-CN"
[log]
# debug 级会打「启动基线」与第 5/25 秒的「资源快照」，
# 调内存/CPU 时用得上；嫌吵改成 info。
level = "debug"
TOML
    echo "已生成开发配置：$CFG"
  fi

  # wails dev 按**空格**切分 -appargs，所以路径里不能有空格。
  case "$CFG" in
    *" "*)
      echo "仓库路径含空格，无法用 -appargs 传递配置路径：$CFG" >&2
      echo "请把仓库移到无空格的路径下，或改用 PAWCLIP_DEV_REAL=1。" >&2
      exit 1 ;;
  esac
  WAILS_ARGS+=(-appargs "-config $CFG")
  echo "开发数据目录：$WORK"
fi

exec "$WAILS" dev "${WAILS_ARGS[@]}" "$@"
