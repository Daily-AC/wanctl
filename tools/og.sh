#!/bin/sh
# 生成分享卡片 site/assets/og.png（1200×630）。
# 卡片本体是 tools/og.html，渲染时临时拷进 site/ 才能吃到同一份 app.css 和字体
# ——卡片和官网必须是同一套样式，不能各画各的。
#
# 用法：site/ 下起 python3 -m http.server <端口>，然后 tools/og.sh [端口=8688]
set -e
PORT=${1:-8688}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
curl -sf -o /dev/null "http://127.0.0.1:$PORT/" || {
  echo "http://127.0.0.1:$PORT/ 没在服务（site/ 下起 python3 -m http.server $PORT）" >&2; exit 1; }
cp "$ROOT/tools/og.html" "$ROOT/site/_og.html"
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --virtual-time-budget=6000 \
  --window-size=1200,630 --screenshot="$ROOT/site/assets/og.png" \
  "http://127.0.0.1:$PORT/_og.html" >/dev/null 2>&1
rm -f "$ROOT/site/_og.html"
sips -g pixelWidth -g pixelHeight "$ROOT/site/assets/og.png" | tail -2
ls -lh "$ROOT/site/assets/og.png" | awk '{print $5}'
