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
# 截图工装（tools/shot.sh 和 visual-acceptance 的 shot-mobile.sh）会在 site/ 里
# 临时放一个 500px 壳页。跑崩了会留下来，那种东西不该被公开服务出去。
rm -f "$STAGE"/_shell-*.html

# 文档站从 docs/*.md 生成，不进仓库（site/docs/ 在 .gitignore 里）。
# 生成到发布副本里，所以本地那份是新是旧都不影响发出去的东西。
rm -rf "$STAGE/docs"
uv run "$ROOT/tools/docsite/build.py" --out "$STAGE/docs"

# 内容指纹：md5 前 10 位，够用且短。
# mark.svg 也在里面：2026-09-04 换标记那次它被漏下了 —— 源站换了新文件，
# 而 Cloudflare 拿着一份 age=13048 的 HIT 在发旧的（nginx 给图片的
# Cache-Control 是 public, max-age=86400），标签页里整整一天还是旧标记。
#
# 指纹要写进每一个页面，不只是首页：文档站有 14 个 .html，漏掉任何一个
# 就等于那一页永远在拿 Cloudflare 缓存里的旧样式表。
PAGES=$(find "$STAGE" -name '*.html')
for f in app.css app.js mark.svg docs.css docs.js; do
  h=$(md5 -q "$STAGE/assets/$f" 2>/dev/null || md5sum "$STAGE/assets/$f" | cut -c1-32)
  h=$(echo "$h" | cut -c1-10)
  for p in $PAGES; do
    # BSD 和 GNU sed 的 -i 用法不同，走临时文件绕开
    sed "s|assets/$f\"|assets/$f?v=$h\"|g" "$p" > "$p.new"
    mv "$p.new" "$p"
  done
  echo "  $f -> ?v=$h"
done

# 每一页都必须只剩带指纹的引用。href/src 之外的 assets/ 出现（og:image 那些
# 绝对 URL）不算 —— 浏览器不拿它们当样式或脚本。
missed=$(grep -oE '(href|src)="/?assets/[A-Za-z0-9._-]+"' $PAGES || true)
if [ -n "$missed" ]; then
  echo "还有没打上指纹的引用 —— 别发这一版：" >&2
  grep -nE '(href|src)="/?assets/[A-Za-z0-9._-]+"' $PAGES >&2
  exit 1
fi
for p in $PAGES; do
  grep -q '?v=[0-9a-f]' "$p" || {
    echo "${p#$STAGE} 一个指纹都没有 —— 别发这一版" >&2; exit 1; }
done
echo "  $(echo "$PAGES" | wc -l | tr -d ' ') 个页面，引用全部带指纹："
grep -ohE '(href|src)="/?assets/[A-Za-z0-9._-]+\?v=[0-9a-f]+"' $PAGES \
  | sed 's|^[a-z]*="||; s|"$||; s|^|    |' | sort -u

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
