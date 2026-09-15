#!/bin/zsh
# M1 端到端演示 —— 一条命令看懂 M1 干了什么。
#
# 做法：把一个**真实**的 PawClip 跑起来（隔离的临时库，绝不碰你的真实数据目录），
# 用 pbcopy/osascript 模拟"你在系统里复制了几样东西"，然后把落库结果摊开给你看。
#
# 用法：
#   scripts/build.sh        # 先构建（注意必须走这个脚本，否则 FTS5 会静默失效）
#   scripts/demo-m1.sh      # 再跑本演示
#
# 依赖：sqlite3、pbcopy（macOS 自带）。
#
# ⚠️ 两个必须遵守的约束，都是踩过的坑：
#   1. 整条链路必须在**同一个 shell 进程**内跑完。若分成两次调用，后台的 App
#      会在前一条命令结束时被回收，后面再 pbcopy 就什么都捕不到（表现为 0 行）。
#   2. 本脚本不要把输出目录设成自己所在的目录 —— 开头的 `rm -rf` 会把正在被
#      zsh 读取的脚本本身删掉。

set -u

cd "$(dirname "$0")/.."

APP=./build/bin/pawclip.app/Contents/MacOS/PawClip
WORK=$PWD/.workbuddy/tmp/pawclip-demo
DB=$WORK/pawclip.db

if [ ! -x "$APP" ]; then
  echo "找不到 $APP，请先跑 scripts/build.sh" >&2
  exit 1
fi

rm -rf "$WORK"
mkdir -p "$WORK"

cat > "$WORK/config.toml" <<TOML
[database]
path = "$DB"
[ui]
language = "zh-CN"
[log]
level = "debug"
TOML

# 拿仓库里现成的图标当"一张图片"
cp assets/icon/dist/pawclip-1024.png "$WORK/demo.png"

echo "===== ① 启动 App（隔离库，不碰你的真实数据目录）====="
"$APP" -config "$WORK/config.toml" > "$WORK/app.log" 2>&1 &
APP_PID=$!
sleep 4

if ! kill -0 $APP_PID 2>/dev/null; then
  echo "!! App 启动失败，日志："; cat "$WORK/app.log"; exit 1
fi
echo "  已启动 pid=$APP_PID"
echo "  数据目录：$WORK"
echo "  首次建库产物："
ls -1 "$WORK" | sed 's/^/    /'

echo ""
echo "===== ② 模拟你在系统里复制东西 ====="
echo "  [1] 复制文本 A"; printf 'PawClip M1 演示 · 第一条文本' | pbcopy; sleep 2
echo "  [2] 复制文本 B"; printf 'PawClip M1 演示 · 第二条文本' | pbcopy; sleep 2
echo "  [3] 再复制一次文本 A（验去重）"; printf 'PawClip M1 演示 · 第一条文本' | pbcopy; sleep 2
echo "  [4] 复制一张 PNG"; osascript -e "set the clipboard to (read (POSIX file \"$WORK/demo.png\") as «class PNGf»)" >/dev/null 2>&1; sleep 3
echo "  [5] 复制文本 C"; printf 'PawClip M1 演示 · 第三条：English mixed 123' | pbcopy; sleep 2

echo ""
echo "===== ③ 落库结果（M1 的产出）====="
sqlite3 -header -column "$DB" "
SELECT id,
       kind,
       use_count AS 次数,
       byte_size AS 字节,
       substr(coalesce(text_content, preview), 1, 28) AS 内容
FROM items WHERE deleted_at IS NULL ORDER BY id;"

echo ""
echo "  存活条目数 = $(sqlite3 "$DB" 'SELECT count(*) FROM items WHERE deleted_at IS NULL;')"
echo "  去重校验（文本 A 被复制 2 次，应只有一行且 次数=2）："
sqlite3 "$DB" "SELECT '    ' || substr(text_content,1,24) || ' → use_count=' || use_count
               FROM items WHERE text_content LIKE '%第一条%';"

echo ""
echo "===== ④ 图片 blob 是否真落盘 ====="
sqlite3 "$DB" "SELECT '    image_path=' || image_path || char(10) ||
                      '    thumb_path=' || thumb_path
               FROM items WHERE kind='image';"
echo "    实际文件："
find "$WORK/blobs" -type f | sed "s|$WORK/blobs/|      |"

echo ""
echo "===== ⑤ 启动日志（重点看 fts5=true 与有无降级告警）====="
grep -v 'Secure coding' "$WORK/app.log" | sed 's/^/  /'

echo ""
echo "===== ⑥ 关闭 App ====="
kill $APP_PID 2>/dev/null
sleep 2
kill -9 $APP_PID 2>/dev/null
echo "  clean_shutdown 标记：$(cat "$WORK/clean_shutdown" 2>/dev/null || echo '(无 —— 正常：干净退出会把它删掉)')"
