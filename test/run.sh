#!/bin/zsh
# PawClip 测试总入口 —— 本机一条命令，跑完所有"能自动判对错"的层。
#
# 设计原则：**与 CI 同口径**。下面每一条命令都和在 GitHub Actions 上跑的
# 一模一样（同一个构建标签、同一个 -p 1、同一个 -count=1）。本机绿了 CI 就该绿，
# 反过来也一样——"我本地明明是好的"这类分歧，多半来自两边参数不一致。
#
# 分层而不 set -e：一次看到全部问题，比"修一个再跑一遍"快得多。
# 但最后一定给总表，并以非零码退出。
#
# 为什么单测必须留在各包目录里（而不是像某些语言那样集中到一个 tests/ 目录）：
# Go 工具链要求 `*_test.go` 与被测包**同目录**，连 `package foo_test` 这种
# 外部测试包也必须在同目录才能编译。本仓库的测试大量访问包内未导出符号
# （listSQL / chooseFTSPlan / newApp …），搬走即全红。所以能集中到 test/ 的
# 只有"独立可执行的测试资产"——本文件、accept.sh、demo-m1.sh、check-i18n.mjs。

set -u

cd "$(dirname "$0")/.."

usage() {
  cat <<'EOF'
用法：test/run.sh [选项]

默认跑这四层（都不需要先构建）：
  ① 格式      gofmt -l .
  ② 静态检查  go vet -tags sqlite_fts5 ./...
  ③ 单元测试  go test -tags sqlite_fts5 -p 1 -count=1 ./...
  ④ 前端检查  npm run check:i18n + check:md + check:types

选项：
  --short     跳过规模测试（10 万条检索延迟、5 MB 图片落库）
  --accept    追加一层：对**已构建的产物**做真机端到端验收（test/accept.sh）
  --build     在 --accept 之前先 scripts/build.sh 重新出包（发布前的完整口径）
  --demo      跑捕获链路演示（test/demo-m1.sh），只给人看，不判对错
  -h, --help  显示本说明

注意：
  · --accept 会**覆盖你当前的系统剪贴板**，耗时约 40 秒，其中 26 秒是静置采样
    ——那段时间别动剪贴板，否则空闲 CPU 会量成"我们正在折腾它"。
  · --accept 不做构建。产物过期时它测的是旧的 .app，所以发布前请用
    `test/run.sh --build --accept`。
EOF
}

SHORT=0
WANT_ACCEPT=0
WANT_BUILD=0
WANT_DEMO=0

while [ $# -gt 0 ]; do
  case "$1" in
    --short)  SHORT=1 ;;
    --accept) WANT_ACCEPT=1 ;;
    --build)  WANT_BUILD=1 ;;
    --demo)   WANT_DEMO=1 ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "未知参数：$1" >&2
      echo "（test/run.sh --help 看用法）" >&2
      exit 2 ;;
  esac
  shift
done

PASSED=()
FAILED=()
LAYER_NO=0

# run_layer <名字> <命令...>
# 无论成败都继续下一层，把结论记进数组，最后统一汇报。
run_layer() {
  local name="$1"; shift
  LAYER_NO=$((LAYER_NO + 1))
  echo ""
  echo "────────────────────────────────────────────────────────"
  printf ' %s %s\n' "$(printf '%s' "$LAYER_NO" | sed 's/^/第/;s/$/层/')" "$name"
  echo "────────────────────────────────────────────────────────"
  if "$@"; then
    PASSED+=("$name")
  else
    local rc=$?
    FAILED+=("$name（退出码 $rc）")
  fi
  return 0
}

# ① 格式。gofmt -l 只列不改，所以这一步是只读的。
layer_fmt() {
  local bad
  bad=$(gofmt -l . 2>/dev/null)
  if [ -n "$bad" ]; then
    echo "$bad" | sed 's/^/  /'
    echo ""
    echo "  上面这些文件没走 gofmt。修：gofmt -w <文件>"
    return 1
  fi
  echo "  全部 Go 文件已格式化"
}

# ③ 单测。参数顺序有讲究：构建标签必须写在包路径**之前**，
# 写成 `go test ./... -tags x` 是另一回事（见 ci.yml 里 compile-check 的长注释）。
layer_gotest() {
  local args=(-tags sqlite_fts5 -p 1 -count=1)
  if [ "$SHORT" = 1 ]; then
    args+=(-short)
  fi
  echo "  go test ${args[*]} ./..."
  go test "${args[@]}" ./...
}

# ⑤ 产物验收。这里不构建——构建是 --build 的事，两件事混在一起会让人
# 分不清"测的是新包还是旧包"。
layer_accept() {
  if [ ! -x build/bin/pawclip.app/Contents/MacOS/PawClip ]; then
    echo "  找不到 build/bin/pawclip.app —— 先跑 scripts/build.sh，或加 --build"
    return 1
  fi
  ./test/accept.sh
}

# ④ 前端检查。三项都挂在 frontend/package.json 上，各是一个可单独跑的入口
# （这样 test/ 不需要重复写路径，改目录结构时只动 package.json 一处）。
# 层内是 fail-fast：i18n 键对不上时先报缺哪个键，不让 TS 的连锁报错盖过去。
layer_frontend() {
  npm --prefix frontend run --silent check:i18n || return 1
  npm --prefix frontend run --silent check:md || return 1
  npm --prefix frontend run --silent check:types
}

run_layer "格式（gofmt）"                layer_fmt
run_layer "静态检查（go vet）"           go vet -tags sqlite_fts5 ./...
run_layer "单元测试（go test）"          layer_gotest
run_layer "前端检查（i18n + 类型）"      layer_frontend

if [ "$WANT_BUILD" = 1 ]; then
  run_layer "构建产物（scripts/build.sh）" scripts/build.sh -platform darwin/universal
fi

if [ "$WANT_ACCEPT" = 1 ]; then
  run_layer "产物验收（test/accept.sh）"   layer_accept
fi

if [ "$WANT_DEMO" = 1 ]; then
  run_layer "捕获链路演示（test/demo-m1.sh）" ./test/demo-m1.sh
fi

# ── 总表 ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════════════"
if [ ${#PASSED[@]} -gt 0 ]; then
  for n in "${PASSED[@]}"; do echo "  ✅ $n"; done
fi
if [ ${#FAILED[@]} -gt 0 ]; then
  for n in "${FAILED[@]}"; do echo "  ❌ $n"; done
fi
echo "════════════════════════════════════════════════════════════"
if [ ${#FAILED[@]} -eq 0 ]; then
  echo " 全部 ${#PASSED[@]} 层通过"
  exit 0
fi
echo " 通过 ${#PASSED[@]} 层，失败 ${#FAILED[@]} 层"
exit 1
