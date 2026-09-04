# /// script
# requires-python = ">=3.9"
# dependencies = ["markdown==3.7"]
# ///
"""Render the repository's markdown into the static docs site at wc.z10.dev/docs.

The markdown in `docs/` is the only source. Nothing here forks an article's
text; if a body has to change to render well, the change goes into the markdown
and every other consumer (the portal, GitHub) gets it too.

Two sets go in:

  * `docs/portal/*.md` — six user guides in Chinese, grouped and ordered by
    `docs/portal/manifest.json`. Their bodies are deployment-neutral: they say
    `relay.example.com` / `portal.example.com`, and this script substitutes the
    real origins the same way `scripts/sync-portal-docs.py` does. A body that
    still contains a placeholder afterwards aborts the build, so a
    half-configured run cannot publish `example.com` to a live site.
  * `docs/*.md` — seven technical documents in English. Their titles come from
    their own H1, which is then removed from the body: the template renders the
    title, so leaving it in would print it twice.

Out comes `site/docs/index.html` plus `site/docs/<slug>/index.html` per article,
which is what lets nginx serve `/docs/architecture/` without a redirect. The
output directory is build output — it is in `.gitignore` and never committed.

Article bodies stay in the language they were written in this round. Only the
chrome (header, group names, breadcrumb, the on-page table of contents, the
pager, the footer) is bilingual, through the same `data-en` / `data-zh` pairs
the product site uses. Per-article translation can be added later by giving an
article a second body without moving any URL.

    uv run tools/docsite/build.py [--out DIR]
        [--relay-origin https://…] [--portal-origin https://…]
"""

import argparse
import html
import json
import pathlib
import re
import shutil
import sys
import urllib.parse

import markdown
from markdown.extensions.toc import slugify_unicode

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
DOCS = ROOT / "docs"
PORTAL = DOCS / "portal"

DEFAULT_RELAY = "https://wanctl-relay.z10.dev"
DEFAULT_PORTAL = "https://wanctl.z10.dev"

# The English half of the portal manifest's group names. The manifest is the
# portal's contract and stays as it is; the docs site's chrome is bilingual, so
# the second half lives here.
PORTAL_GROUPS_EN = {
    "quickstart": "Quickstart",
    "control": "Control devices",
    "ai": "AI access",
    "advanced": "Approvals & advanced",
}

# The technical documents, in reading order, split into two groups that follow
# the six user guides.
TECH_GROUPS = [
    {
        "id": "internals",
        "en": "Self-hosting & architecture",
        "zh": "自部署与架构",
        "files": [
            "architecture.md",
            "self-hosting.md",
            "environment.md",
            "release-signing.md",
            "android.md",
        ],
    },
    {
        "id": "security",
        "en": "Security",
        "zh": "安全",
        "files": [
            "security-audit-2026-08-28.md",
            "security-audit-2026-07-23.md",
        ],
    },
]

SITE = "https://wc.z10.dev"
BASE = "/docs"

MARK = (
    '<svg class="mk" viewBox="0 0 32 32" aria-hidden="true">'
    '<path d="M5.6 12.4 10.2 21.8 15.2 14.6 19.6 21.8 26.4 7.6" fill="none" '
    'stroke="currentColor" stroke-width="3.1" stroke-linecap="round" '
    'stroke-linejoin="round"/></svg>'
)


def bi(en, zh, tag="span", cls=None):
    """One bilingual chrome string, in the site's own `data-en` / `data-zh` shape."""
    c = ' class="%s"' % cls if cls else ""
    return '<%s%s data-en="%s" data-zh="%s">%s</%s>' % (
        tag,
        c,
        html.escape(en, quote=True),
        html.escape(zh, quote=True),
        en,
        tag,
    )


# ── source ────────────────────────────────────────────────────────────────


def substitute(body, origins):
    """Rewrite <name>.example.com placeholders, as scripts/sync-portal-docs.py does."""
    for name, origin in origins.items():
        # An empty origin substitutes nothing, exactly as the sync script does.
        # Deleting the domain instead would erase the placeholder and let the
        # "still contains example.com" guard pass on a half-configured run.
        if not origin:
            continue
        origin = origin.rstrip("/")
        host = urllib.parse.urlsplit(origin).netloc or origin
        body = body.replace("https://%s.example.com" % name, origin)
        body = body.replace("%s.example.com" % name, host)
    return body


def load(relay_origin, portal_origin):
    """Return the ordered groups, each with its ordered articles."""
    manifest = json.loads((PORTAL / "manifest.json").read_text(encoding="utf-8"))
    origins = {"relay": relay_origin, "portal": portal_origin}

    groups = []
    by_slug = {}

    for g in sorted(manifest["groups"], key=lambda g: g["position"]):
        if g["slug"] not in PORTAL_GROUPS_EN:
            sys.exit(
                "manifest group %r has no English name in PORTAL_GROUPS_EN" % g["slug"]
            )
        group = {
            "id": g["slug"],
            "en": PORTAL_GROUPS_EN[g["slug"]],
            "zh": g["title"],
            "articles": [],
        }
        groups.append(group)
        by_slug[g["slug"]] = group

    for a in sorted(manifest["articles"], key=lambda a: (a["group"], a["position"])):
        src = PORTAL / a["file"]
        body = substitute(src.read_text(encoding="utf-8"), origins)
        if "example.com" in body:
            sys.exit(
                "%s still contains example.com after substitution — refusing to "
                "publish placeholder text" % src.relative_to(ROOT)
            )
        by_slug[a["group"]]["articles"].append(
            {
                "slug": a["slug"],
                "title": a["title"],
                "lang": "zh",
                "body": body,
                "src": src,
            }
        )

    for g in TECH_GROUPS:
        group = {"id": g["id"], "en": g["en"], "zh": g["zh"], "articles": []}
        groups.append(group)
        for name in g["files"]:
            src = DOCS / name
            text = src.read_text(encoding="utf-8")
            m = re.match(r"#\s+(.+?)\s*\n", text)
            if not m:
                sys.exit("%s has no H1 to take a title from" % src.relative_to(ROOT))
            group["articles"].append(
                {
                    "slug": src.stem,
                    "title": m.group(1),
                    "lang": "en",
                    # The template renders the title, so drop the H1 from the body.
                    "body": text[m.end() :].lstrip("\n"),
                    "src": src,
                }
            )

    return groups


# ── render ────────────────────────────────────────────────────────────────


def render(article):
    """Markdown to HTML, plus the h2/h3 tree the on-page contents is built from."""
    md = markdown.Markdown(
        extensions=["tables", "fenced_code", "sane_lists", "toc"],
        extension_configs={
            "toc": {
                "permalink": False,
                "toc_depth": "2-3",
                "slugify": slugify_unicode,
            }
        },
    )
    body = md.convert(article["body"])
    # A wide table must scroll inside itself; the page never scrolls sideways.
    body = body.replace("<table>", '<div class="tw"><table>').replace(
        "</table>", "</table></div>"
    )
    article["html"] = body
    article["toc"] = md.toc_tokens
    article["ids"] = set(re.findall(r'\bid="([^"]+)"', article["html"]))


MD_LINK = re.compile(r'href="([^"]*)"')


def resolve_links(article, slugs, ids, errors):
    """Point every internal link at its page on this site, and prove it resolves.

    A link that cannot be resolved is a build failure, not a 404 discovered by a
    reader: an article that moved should break the build that moved it.
    """
    here = article["slug"]

    def one(m):
        href = m.group(1)
        target = href

        if href.startswith(("http://", "https://", "mailto:")):
            return m.group(0)

        # The portal's own in-app link shape, `#docs/<slug>`. On this site that
        # is a page of its own.
        if href.startswith("#docs/"):
            slug = href[len("#docs/") :]
            if slug not in slugs:
                errors.append("%s: #docs/%s is not an article" % (here, slug))
                return m.group(0)
            target = "%s/%s/" % (BASE, slug)

        elif href.startswith("#"):
            frag = urllib.parse.unquote(href[1:])
            if frag not in article["ids"]:
                errors.append("%s: no heading anchor %r on this page" % (here, href))
            return m.group(0)

        elif ".md" in href:
            path, _, frag = href.partition("#")
            stem = pathlib.PurePosixPath(path).stem
            if stem not in slugs:
                errors.append("%s: link to %s, which is not on the docs site" % (here, path))
                return m.group(0)
            target = "%s/%s/" % (BASE, stem)
            if frag:
                if frag not in ids.get(stem, set()):
                    errors.append("%s: %s has no anchor %r" % (here, stem, frag))
                target += "#" + frag

        elif href.startswith(BASE + "/"):
            path, _, frag = href.partition("#")
            slug = path[len(BASE) + 1 :].strip("/")
            if slug and slug not in slugs:
                errors.append("%s: %s is not an article" % (here, path))
            if frag and frag not in ids.get(slug, set()):
                errors.append("%s: %s has no anchor %r" % (here, slug, frag))

        elif href.startswith("/"):
            # A link back to the product site. Only the pages that exist.
            if href.split("#")[0] not in ("/", "/docs/"):
                errors.append("%s: unknown site path %s" % (here, href))

        else:
            errors.append("%s: unresolvable link %s" % (here, href))
            return m.group(0)

        return 'href="%s"' % target

    article["html"] = MD_LINK.sub(one, article["html"])


# ── page ──────────────────────────────────────────────────────────────────


def head(title, desc, canonical):
    return """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<meta name="description" content="%s">
<link rel="canonical" href="%s">
<meta name="theme-color" content="#ffffff">
<link rel="stylesheet" href="/assets/app.css">
<link rel="stylesheet" href="/assets/docs.css">
<link rel="icon" href="/assets/mark.svg" type="image/svg+xml">
</head>
<body class="docs">

<header class="nav">
  <a class="brand" href="/">%swanctl</a>
  <a class="here" href="/docs/" data-en="Docs" data-zh="文档">Docs</a>
  <nav class="links">
    <a href="/#install" data-en="Install" data-zh="安装">Install</a>
    <a href="https://github.com/Daily-AC/wanctl">GitHub</a>
  </nav>
  <button class="lang" id="lang" type="button" aria-label="Switch language">中文</button>
</header>
""" % (
        html.escape(title),
        html.escape(desc, quote=True),
        canonical,
        MARK,
    )


FOOT = """
<footer class="foot">
  <nav class="flinks">
    <a href="/docs/" data-en="Docs" data-zh="文档">Docs</a>
    <a href="/docs/architecture/" data-en="Architecture" data-zh="架构">Architecture</a>
    <a href="/docs/self-hosting/" data-en="Run your own" data-zh="自己部署一套">Run your own</a>
    <a href="https://github.com/Daily-AC/wanctl/blob/main/SECURITY.md"
       data-en="Security" data-zh="安全">Security</a>
    <a href="https://github.com/Daily-AC/wanctl/releases"
       data-en="Releases" data-zh="版本">Releases</a>
    <a href="https://github.com/Daily-AC/wanctl">GitHub</a>
  </nav>
  <div class="row">
    <span data-en="Open source, Apache-2.0." data-zh="开源，Apache-2.0。">Open source, Apache-2.0.</span>
    <span class="end mono" data-en="Every page here is generated from the markdown in the repository."
          data-zh="这里的每一页都由仓库里的 markdown 生成。">Every page here is generated from the markdown in the repository.</span>
  </div>
</footer>

<script src="/assets/docs.js"></script>
</body>
</html>
"""


def group_nav(groups, current=None):
    """The left column: every group, every article, the current one marked."""
    out = ['<details class="dnav" id="dnav" open>']
    out.append(
        "  <summary>%s</summary>"
        % bi("All documentation", "全部文档", tag="span", cls="dnav-label")
    )
    out.append('  <div class="dnav-in">')
    for g in groups:
        out.append(
            '    <p class="g" data-en="%s" data-zh="%s">%s</p>'
            % (html.escape(g["en"], quote=True), html.escape(g["zh"], quote=True),
               html.escape(g["en"]))
        )
        out.append("    <ul>")
        for a in g["articles"]:
            on = ' class="on" aria-current="page"' if a["slug"] == current else ""
            out.append(
                '      <li><a href="%s/%s/"%s lang="%s">%s</a></li>'
                % (BASE, a["slug"], on, "zh-CN" if a["lang"] == "zh" else "en", html.escape(a["title"]))
            )
        out.append("    </ul>")
    out.append("  </div>")
    out.append("</details>")
    return "\n".join(out)


def toc_html(tokens):
    """The right column, from h2 and h3. Nothing to show means no column."""
    if not tokens:
        return ""
    out = [
        '<aside class="dtoc"><nav aria-label="On this page">',
        "  %s" % bi("On this page", "本页目录", tag="p", cls="t"),
        "  <ul>",
    ]
    for t in tokens:
        out.append(
            '    <li><a href="#%s">%s</a>' % (urllib.parse.quote(t["id"]), html.escape(t["name"]))
        )
        kids = [c for c in t["children"] if c["level"] == 3]
        if kids:
            out.append("      <ul>")
            for c in kids:
                out.append(
                    '        <li><a href="#%s">%s</a></li>'
                    % (urllib.parse.quote(c["id"]), html.escape(c["name"]))
                )
            out.append("      </ul>")
        out.append("    </li>")
    out += ["  </ul>", "</nav></aside>"]
    return "\n".join(out)


def crumbs(group, article):
    """Docs / group / this page. The last crumb is a pill, not a link."""
    # 分隔的斜杠由 CSS 的 ::after 画：它是笔画不是内容，而且窄屏上面包屑一换行，
    # 一个孤零零吊在行尾的斜杠比没有它更难看 —— 那时候直接关掉最后一道。
    return (
        '<nav class="crumbs" aria-label="Breadcrumb">'
        '<a href="%s/" data-en="Docs" data-zh="文档">Docs</a>'
        '<span data-en="%s" data-zh="%s">%s</span>'
        '<span class="pill" lang="%s">%s</span>'
        "</nav>"
    ) % (
        BASE,
        html.escape(group["en"], quote=True),
        html.escape(group["zh"], quote=True),
        html.escape(group["en"]),
        "zh-CN" if article["lang"] == "zh" else "en",
        html.escape(article["title"]),
    )


def pager(prev, nxt):
    if not prev and not nxt:
        return ""
    out = ['<nav class="pager">']
    if prev:
        out.append(
            '  <a class="p" href="%s/%s/"><span class="k" data-en="Previous" data-zh="上一篇">Previous</span>'
            '<span class="n" lang="%s">%s</span></a>'
            % (BASE, prev["slug"], "zh-CN" if prev["lang"] == "zh" else "en", html.escape(prev["title"]))
        )
    if nxt:
        out.append(
            '  <a class="n-side" href="%s/%s/"><span class="k" data-en="Next" data-zh="下一篇">Next</span>'
            '<span class="n" lang="%s">%s</span></a>'
            % (BASE, nxt["slug"], "zh-CN" if nxt["lang"] == "zh" else "en", html.escape(nxt["title"]))
        )
    out.append("</nav>")
    return "\n".join(out)


def summarize(article):
    text = re.sub(r"<[^>]+>", "", article["html"])
    text = html.unescape(re.sub(r"\s+", " ", text)).strip()
    return text[:157] + ("…" if len(text) > 157 else "")


def article_page(groups, group, article, prev, nxt):
    toc = toc_html(article["toc"])
    body = [
        head(
            "%s — wanctl docs" % article["title"],
            summarize(article),
            "%s%s/%s/" % (SITE, BASE, article["slug"]),
        ),
        '<div class="dpage%s">' % ("" if toc else " no-toc"),
        group_nav(groups, article["slug"]),
        '<main class="dbody">',
        crumbs(group, article),
        '<article lang="%s">' % ("zh-CN" if article["lang"] == "zh" else "en"),
        "<h1>%s</h1>" % html.escape(article["title"]),
        article["html"],
        "</article>",
        pager(prev, nxt),
        "</main>",
        toc,
        "</div>",
        FOOT,
    ]
    return "\n".join(p for p in body if p)


def index_page(groups):
    out = [
        head(
            "wanctl docs",
            "Every wanctl document in one place: the user guides for the portal "
            "and the technical documents for running it yourself.",
            "%s%s/" % (SITE, BASE),
        ),
        '<main class="dindex">',
        '<h1 data-en="Documentation" data-zh="文档">Documentation</h1>',
        '<p class="lead" data-en="Everything wanctl has written down: the guides for '
        "getting a device enrolled and controlled, and the technical documents for "
        'running the whole thing yourself." data-zh="wanctl 写下来的全部：把设备接入并'
        '控制起来的使用指南，以及自己跑一整套的技术文档。">'
        "Everything wanctl has written down: the guides for getting a device enrolled "
        "and controlled, and the technical documents for running the whole thing "
        "yourself.</p>",
        '<div class="cols">',
    ]
    for g in groups:
        out.append('<section class="g">')
        out.append(
            '<h2 data-en="%s" data-zh="%s">%s</h2>'
            % (html.escape(g["en"], quote=True), html.escape(g["zh"], quote=True),
               html.escape(g["en"]))
        )
        out.append("<ul>")
        for a in g["articles"]:
            out.append(
                '<li><a href="%s/%s/" lang="%s">%s</a></li>'
                % (BASE, a["slug"], "zh-CN" if a["lang"] == "zh" else "en", html.escape(a["title"]))
            )
        out.append("</ul>")
        out.append("</section>")
    out += ["</div>", "</main>", FOOT]
    return "\n".join(out)


# ── main ──────────────────────────────────────────────────────────────────


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", default=str(ROOT / "site" / "docs"),
                    help="output directory (default: site/docs)")
    ap.add_argument("--relay-origin", default=DEFAULT_RELAY,
                    help="substituted for relay.example.com (default: %s)" % DEFAULT_RELAY)
    ap.add_argument("--portal-origin", default=DEFAULT_PORTAL,
                    help="substituted for portal.example.com (default: %s)" % DEFAULT_PORTAL)
    args = ap.parse_args()

    groups = load(args.relay_origin, args.portal_origin)
    articles = [a for g in groups for a in g["articles"]]
    slugs = {a["slug"] for a in articles}
    if len(slugs) != len(articles):
        sys.exit("two articles want the same slug")

    for a in articles:
        render(a)
    ids = {a["slug"]: a["ids"] for a in articles}

    errors = []
    for a in articles:
        resolve_links(a, slugs, ids, errors)
    if errors:
        print("broken links:", file=sys.stderr)
        for e in errors:
            print("  " + e, file=sys.stderr)
        sys.exit(1)

    out = pathlib.Path(args.out)
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    (out / "index.html").write_text(index_page(groups), encoding="utf-8")
    n = 0
    for g in groups:
        for a in g["articles"]:
            i = articles.index(a)
            prev = articles[i - 1] if i else None
            nxt = articles[i + 1] if i + 1 < len(articles) else None
            d = out / a["slug"]
            d.mkdir()
            d.joinpath("index.html").write_text(
                article_page(groups, g, a, prev, nxt), encoding="utf-8"
            )
            n += 1

    print("%s: index + %d articles, %d links checked, 0 broken"
          % (out.relative_to(ROOT) if out.is_relative_to(ROOT) else out,
             n, sum(len(MD_LINK.findall(a["html"])) for a in articles)))


if __name__ == "__main__":
    main()
