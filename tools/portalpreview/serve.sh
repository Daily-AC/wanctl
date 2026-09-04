#!/bin/sh
# 门户的离线预览：把 internal/portal/web/ 拷进一个临时目录，注入 fixtures.js，
# 用 python3 起个静态服务。改样式不需要 relay、Postgres 或登录。
#
#   tools/portalpreview/serve.sh          # 前台，Ctrl-C 停
#   tools/portalpreview/serve.sh 8724     # 换端口
#
# 页面上 ?view=device/bench-02 之类可以直接跳到某个状态，见 fixtures.js 末尾。
set -e
PORT=${1:-8724}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SRC="$ROOT/internal/portal/web"
OUT="${TMPDIR:-/tmp}/wanctl-portal-preview"

rm -rf "$OUT"
mkdir -p "$OUT"
# macOS 的 cp 会带 ._* 影子文件（扩展属性），静态服务会把它们照发出去。
COPYFILE_DISABLE=1 cp -R "$SRC/." "$OUT/"
find "$OUT" -name '._*' -delete
cp "$ROOT/tools/portalpreview/fixtures.js" "$OUT/fixtures.js"

# 版本占位符在真实运行时由 assets.go 填；预览里给个固定值即可。
# fixtures.js 必须排在 app.js 前面 —— 它要在 app.js 发第一个请求之前接管 fetch。
sed -e 's/__V__/dev/g' \
    -e 's#<script src="/assets/app.js?v=dev"></script>#<script src="/fixtures.js"></script>\
<script src="/assets/app.js?v=dev"></script>#' \
    "$SRC/index.html" > "$OUT/index.html"

# 三张认证页（登录 / 等待邀请 / 设备授权）由 Go 渲染，模板就是同一批文件。
# 这里用真实形状的假值把占位符填上 —— 形状不对就是工装在替真代码撒谎：
#   授权码  9 位、中间一道横杠、字母表去掉了 I O 0 1（internal/relay/enroll.go）
#   指纹    SHA256: 加 44 个 base64 字符（internal/transport/identity.go）
#   有效期  5 分钟（enrollCodeTTL），不是随手写的 10
for page in login pending enroll; do
  sed -e 's/{{\.V}}/dev/g' \
      -e 's#{{\.Host}}#wanctl.example.dev#g' \
      -e 's#{{\.Start}}#/auth/github?next=%2F#g' \
      -e 's/{{\.Login}}/octocat/g' \
      -e 's/{{\.NS}}/octocat/g' \
      -e 's/{{\.Code}}/K7RM-2QXP/g' \
      -e 's/{{\.Mins}}/5/g' \
      -e 's#{{\.FP}}#SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw=#g' \
      "$SRC/$page.html" > "$OUT/$page.html"
  if grep -q '{{' "$OUT/$page.html"; then
    echo "$page.html 里还有没填的占位符，补一条 sed" >&2
    exit 1
  fi
done

# app.css / app.js / fonts 在页面里是 /assets/... 的绝对路径。
# 这里用软链而不是拷贝：改一行 CSS 就要重启服务才看得见，那个来回不值当。
# index.html 仍是拷贝（要注入 fixtures.js），它改得少，改了重启一次即可。
ln -s "$SRC" "$OUT/assets"

grep -q fixtures.js "$OUT/index.html" || { echo "注入 fixtures.js 失败，检查 index.html 里的 script 标签" >&2; exit 1; }

# 先占端口再报喜：否则端口冲突时你会先看到一行「预览：」再看到一段 traceback。
if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用：$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | awk 'NR==2{print $1}')。换一个：$0 8725" >&2
  exit 1
fi

# 绑全网卡而不是 127.0.0.1：手机要能走局域网 IP 打开。
# 数据全是虚构的，也没有任何凭据，暴露在局域网里没有代价。
LAN=$(ipconfig getifaddr en0 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')
echo "预览： http://127.0.0.1:$PORT/"
echo "认证： /login.html · /pending.html · /enroll.html（静态渲染，兑换与退出登录点了没反应）"
[ -n "$LAN" ] && echo "手机： http://$LAN:$PORT/"
echo "目录： $OUT"
cd "$OUT" && exec python3 -m http.server "$PORT"
