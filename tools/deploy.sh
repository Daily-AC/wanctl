#!/bin/sh
# 把 site/ 发布到 ls 的 /srv/www/wc.z10.dev（hk 只反代，见 fleet-deploy）。
#
# 为什么要在这里改文件而不是直接 rsync：
#   Cloudflare 会用它自己的 Browser Cache TTL（免费版默认 4h）改写浏览器看到的
#   Cache-Control，源站写 no-cache 也拦不住。所以样式表和脚本的 URL 必须带内容指纹
#   ——换了内容就是换了 URL，旧副本再怎么被缓存也影响不到新访客。
#   指纹只加在发布副本上，本地开发照旧访问 assets/app.css，不用跑构建。
#
# 用法：tools/deploy.sh [--dry-run]
set -e
HOST=ls
DEST=/srv/www/wc.z10.dev
ROOT=$(cd "$(dirname "$0")/.." && pwd)
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

cp -R "$ROOT/site/." "$STAGE/"

# 内容指纹：md5 前 10 位，够用且短。
# mark.svg 也在里面：2026-09-04 换标记那次它被漏下了 —— 源站换了新文件，
# 而 Cloudflare 拿着一份 age=13048 的 HIT 在发旧的（nginx 给图片的
# Cache-Control 是 public, max-age=86400），标签页里整整一天还是旧标记。
for f in app.css app.js mark.svg; do
  h=$(md5 -q "$STAGE/assets/$f" 2>/dev/null || md5sum "$STAGE/assets/$f" | cut -c1-32)
  h=$(echo "$h" | cut -c1-10)
  # BSD 和 GNU sed 的 -i 用法不同，走临时文件绕开
  sed "s|assets/$f\"|assets/$f?v=$h\"|g" "$STAGE/index.html" > "$STAGE/index.html.new"
  mv "$STAGE/index.html.new" "$STAGE/index.html"
  echo "  $f -> ?v=$h"
done
for f in app.css app.js mark.svg; do
  grep -q "assets/$f?v=[0-9a-f]" "$STAGE/index.html" || {
    echo "$f 的指纹没写进 index.html —— 别发这一版" >&2; exit 1; }
done
grep -o 'assets/[a-z.]*?v=[0-9a-f]*' "$STAGE/index.html"

if [ "$1" = "--dry-run" ]; then echo "dry run，未上传"; exit 0; fi

# macOS 的 tar 默认会带 ._* 影子文件（扩展属性），那些会被公开服务出去
COPYFILE_DISABLE=1 tar -C "$STAGE" --no-xattrs -cf - . 2>/dev/null \
  | ssh "$HOST" "sudo rm -rf $DEST && sudo mkdir -p $DEST && sudo chown \$(whoami) $DEST \
      && tar -C $DEST -xf - && find $DEST -name '._*' -delete \
      && printf '已发布 %s 个文件，' \"\$(find $DEST -type f | wc -l)\" && du -sh $DEST | cut -f1 \
      && sudo chown -R www-data:www-data $DEST"

echo "--- 线上核验 ---"
ssh "$HOST" 'for u in https://wc.z10.dev/ https://wc.lab.z10.dev/; do
  printf "%-26s %s\n" "$u" "$(curl -sS -o /dev/null -w "http=%{http_code} t=%{time_total}s" --max-time 25 "$u")"; done'
