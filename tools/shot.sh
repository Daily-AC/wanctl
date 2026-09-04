#!/bin/sh
# 站点截图：桌面 1440 + 真 390 CSS px 的移动端。
#
# 为什么移动端要绕：本机 headless Chrome 把窗口宽度钳到 >= 500px（--headless=new 和旧版都是），
# 所以 --window-size=390,844 会静默地按 500 排版再裁到 390，看起来像整页横向溢出，其实是量具的锅。
# 绕法：一个 500 宽的壳页面里放 390xH 的 iframe，截壳页再居中裁。
# 两个坑（2026-09-02 实测）：
#   - iframe 必须显式给背景色，否则 headless --screenshot 不绘制它，出来全黑；
#   - sips -c 从中心裁且忽略 --cropOffset，所以 iframe 要在壳里居中（上 50px、左右 auto）。
#
# 用法：tools/shot.sh <输出目录> [端口=8688]
# 前置：site/ 目录下已起 python3 -m http.server <端口>
set -e
OUT=${1:?usage: tools/shot.sh <outdir> [port]}
PORT=${2:-8688}
H=844
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$OUT"

curl -sf -o /dev/null "http://127.0.0.1:$PORT/" || {
  echo "http://127.0.0.1:$PORT/ 没在服务（site/ 下起 python3 -m http.server $PORT）" >&2; exit 1; }

"$CHROME" --headless=new --disable-gpu --hide-scrollbars --virtual-time-budget=6000 \
  --window-size=1440,900 --screenshot="$OUT/desktop.png" "http://127.0.0.1:$PORT/" >/dev/null 2>&1
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --virtual-time-budget=6000 \
  --window-size=1440,2400 --screenshot="$OUT/desktop-full.png" "http://127.0.0.1:$PORT/" >/dev/null 2>&1

SHELL_HTML="$ROOT/site/_shell-$$.html"
cat > "$SHELL_HTML" <<HTML
<!doctype html><meta charset="utf-8">
<style>html,body{margin:0;background:#000}
iframe{display:block;border:0;width:390px;height:${H}px;margin:50px auto 0;background:#F5F6F7}</style>
<iframe src="/"></iframe>
HTML
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --virtual-time-budget=6000 \
  --window-size=500,$((H+100)) --screenshot="$OUT/mobile.png" \
  "http://127.0.0.1:$PORT/$(basename "$SHELL_HTML")" >/dev/null 2>&1
sips -c "$H" 390 "$OUT/mobile.png" --out "$OUT/mobile.png" >/dev/null
rm -f "$SHELL_HTML"

for f in desktop mobile; do
  printf "%-8s %s\n" "$f" "$(sips -g pixelWidth -g pixelHeight "$OUT/$f.png" | awk '/pixel/{printf "%s ", $2}')"
done
