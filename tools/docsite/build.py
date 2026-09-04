# /// script
# requires-python = ">=3.9"
# dependencies = ["markdown==3.7"]
# ///
"""Render the repository's markdown into the static docs site at wc.z10.dev/docs.

The markdown in `docs/` is the only source. Nothing here forks an article's
text; if a body has to change to render well, the change goes into the markdown
and every other consumer (the portal, GitHub) gets it too.

Two sets go in:

  * `docs/portal/*.md` — six user guides written in Chinese, grouped and
    ordered by `docs/portal/manifest.json`. Their bodies are
    deployment-neutral: they say `relay.example.com` / `portal.example.com`,
    and this script substitutes the real origins the same way
    `scripts/sync-portal-docs.py` does. A body that still contains a
    placeholder afterwards aborts the build, so a half-configured run cannot
    publish `example.com` to a live site.
  * `docs/*.md` — seven technical documents written in English. Their titles
    come from their own H1, which is then removed from the body: the template
    renders the title, so leaving it in would print it twice.

Every article is a **pair**. Next to each source file sits its translation,
same stem with a language suffix before `.md`:

    docs/portal/quickstart__enroll-device.md      (zh, the source)
    docs/portal/quickstart__enroll-device.en.md   (en, the translation)
    docs/architecture.md                          (en, the source)
    docs/architecture.zh.md                       (zh, the translation)

A translation always opens with an H1; that H1 is its title in that language
and is stripped from the body, exactly as a technical document's own H1 is.
The source file stays canonical — `scripts/sync-portal-docs.py` reads
`manifest.json`, which names only the source files, so the translations are
invisible to the portal sync.

Out comes `site/docs/index.html` plus `site/docs/<slug>/index.html` per
article, which is what lets nginx serve `/docs/architecture/` without a
redirect. The output directory is build output — it is in `.gitignore` and
never committed.

Each article page carries **both** bodies, one `<article>` per language, the
inactive one hidden. `site/assets/docs.js` toggles them together with the
chrome, the on-page contents, the `<title>`, the breadcrumb, the sidebar and
the pager, so a page has no residue of the other language in either mode. URLs
do not move: one page, two bodies. Heading anchors are derived per language, so
each language's contents lands in its own body; where the two languages
slugify to the same id, the translation's copy is suffixed and the source
language keeps the canonical anchor.

A missing translation is not fatal: the source body is rendered in both modes
with a one-line note in the other language. Nothing relies on that path today —
all thirteen articles are translated.

**The parity check** is what keeps a pair from drifting. Before rendering, the
two bodies are compared and any mismatch fails the build:

  * the same fenced code blocks, in the same order, with byte-identical info
    strings and contents — commands, paths, flags, env vars and sample output
    are never translated;
  * the same heading levels in the same order;
  * the same link targets in the same order (a bare same-page `#anchor` is
    language-derived, so only its position is compared);
  * the same table shapes — same tables, same rows, same columns.

Editing an article therefore means editing both halves. That is the point: a
translation that silently drops a command is worse than no translation.

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

# Both languages, in the order they are laid into every page. English first
# because English is what an unconfigured reader gets: the markup ships with
# the English half visible and the Chinese half hidden, and docs.js only has
# to move if the reader has asked for Chinese.
LANGS = ("en", "zh")
OTHER = {"en": "zh", "zh": "en"}
HTML_LANG = {"en": "en", "zh": "zh-CN"}

# The suffix the site puts after a title, per language.
SUFFIX = {"en": " — wanctl docs", "zh": " — wanctl 文档"}

# Shown at the top of a body that exists in one language only.
ONLY_NOTE = {
    "en": "This page has not been translated yet. It is shown in Chinese.",
    "zh": "这一页还没有翻译，下面是英文原文。",
}

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


def read_body(src, origins):
    """One markdown file, with the deployment placeholders already filled in."""
    body = substitute(src.read_text(encoding="utf-8"), origins)
    if "example.com" in body:
        sys.exit(
            "%s still contains example.com after substitution — refusing to "
            "publish placeholder text" % src.relative_to(ROOT)
        )
    return body


def take_h1(src, text):
    """Split `# Title` off the front. The template renders the title itself."""
    m = re.match(r"#\s+(.+?)\s*\n", text)
    if not m:
        sys.exit("%s has no H1 to take a title from" % src.relative_to(ROOT))
    return m.group(1), text[m.end() :].lstrip("\n")


# ── parity ────────────────────────────────────────────────────────────────
#
# The two halves of a pair are the same document in two languages. Everything
# that is not prose has to survive the translation unchanged, and the cheapest
# way to be sure of that is to refuse to build when it did not.

FENCE = re.compile(r"^(```+|~~~+)([^\n]*)\n(.*?)^\1[ \t]*$\n?", re.M | re.S)


def code_blocks(text):
    """(info string, contents) per fenced block, in order."""
    return [(m.group(2).strip(), m.group(3)) for m in FENCE.finditer(text)]


def outside_code(text):
    """The same text with fenced blocks removed, so `#` in a shell comment is
    not counted as a heading and a URL in a sample command is not a link."""
    return FENCE.sub("\n", text)


def heading_levels(text):
    return [len(m.group(1)) for m in re.finditer(r"^(#{1,6})\s", outside_code(text), re.M)]


def link_targets(text):
    out = []
    for href in re.findall(r"\]\(([^)\s]+)", outside_code(text)):
        # A bare same-page anchor is derived from a heading, so it is a
        # different string in each language by construction. Its position in
        # the sequence still has to match.
        out.append("#" if href.startswith("#") and not href.startswith("#docs/") else href)
    return out


def table_shapes(text):
    """One list of per-row cell-boundary counts per table, in order."""
    shapes, cur = [], None
    for line in outside_code(text).splitlines():
        line = line.strip()
        if line.startswith("|"):
            cur = cur if cur is not None else []
            cur.append(line.count("|"))
        elif cur is not None:
            shapes.append(cur)
            cur = None
    if cur is not None:
        shapes.append(cur)
    return shapes


def check_parity(src, src_body, tr, tr_body, errors):
    where = "%s vs %s" % (src.relative_to(ROOT), tr.relative_to(ROOT))

    a, b = code_blocks(src_body), code_blocks(tr_body)
    if len(a) != len(b):
        errors.append("%s: %d code blocks vs %d" % (where, len(a), len(b)))
    else:
        for i, (x, y) in enumerate(zip(a, b)):
            if x != y:
                errors.append(
                    "%s: code block %d differs — a code block is copied, never "
                    "translated" % (where, i + 1)
                )

    for name, fn in (
        ("heading levels", heading_levels),
        ("link targets", link_targets),
        ("table shapes", table_shapes),
    ):
        x, y = fn(src_body), fn(tr_body)
        if x != y:
            errors.append("%s: %s differ — %r vs %r" % (where, name, x, y))


# ── articles ──────────────────────────────────────────────────────────────


def pair(src, src_lang, src_title, src_body, origins, errors):
    """Fill in the other language for one article.

    Returns (titles, bodies, body_langs, only) — `only` is the one language a
    body exists in when the translation is missing, otherwise None.
    """
    tr = src.with_name("%s.%s%s" % (src.stem, OTHER[src_lang], src.suffix))
    if not tr.exists():
        return (
            {l: src_title for l in LANGS},
            {l: src_body for l in LANGS},
            {l: src_lang for l in LANGS},
            src_lang,
        )
    tr_title, tr_body = take_h1(tr, read_body(tr, origins))
    check_parity(src, src_body, tr, tr_body, errors)
    other = OTHER[src_lang]
    return (
        {src_lang: src_title, other: tr_title},
        {src_lang: src_body, other: tr_body},
        {l: l for l in LANGS},
        None,
    )


def load(relay_origin, portal_origin, errors):
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

    def assemble(slug, src, src_lang, src_title, src_body):
        titles, bodies, body_langs, only = pair(
            src, src_lang, src_title, src_body, origins, errors
        )
        return {
            "slug": slug,
            "src": src,
            "src_lang": src_lang,
            "titles": titles,
            "bodies": bodies,
            "body_langs": body_langs,
            "only": only,
        }

    # The portal guides carry no H1 of their own — their titles live in the
    # manifest, which is the portal's contract. Their English halves do carry
    # one, because that is where an English title can come from at all.
    for a in sorted(manifest["articles"], key=lambda a: (a["group"], a["position"])):
        src = PORTAL / a["file"]
        by_slug[a["group"]]["articles"].append(
            assemble(a["slug"], src, "zh", a["title"], read_body(src, origins))
        )

    for g in TECH_GROUPS:
        group = {"id": g["id"], "en": g["en"], "zh": g["zh"], "articles": []}
        groups.append(group)
        for name in g["files"]:
            src = DOCS / name
            title, body = take_h1(src, read_body(src, origins))
            group["articles"].append(assemble(src.stem, src, "en", title, body))

    return groups


# ── render ────────────────────────────────────────────────────────────────


def render_one(text):
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
    body = md.convert(text)
    # A wide table must scroll inside itself; the page never scrolls sideways.
    body = body.replace("<table>", '<div class="tw"><table>').replace(
        "</table>", "</table></div>"
    )
    return body, md.toc_tokens


def retoc(tokens, rename):
    for t in tokens:
        t["id"] = rename.get(t["id"], t["id"])
        retoc(t["children"], rename)


def render(article):
    """Both bodies, with each language's heading anchors kept distinct.

    Two bodies live in one document, so two headings that slugify to the same
    id would collide. The source language keeps the canonical anchor — that is
    the one already published and linked to from outside — and the
    translation's duplicate gets a language suffix.
    """
    article["html"] = {}
    article["toc"] = {}
    article["ids"] = {}
    for lang in LANGS:
        body, toc = render_one(article["bodies"][lang])
        article["html"][lang] = body
        article["toc"][lang] = toc
        article["ids"][lang] = set(re.findall(r'\bid="([^"]+)"', body))

    src, other = article["src_lang"], OTHER[article["src_lang"]]
    clash = article["ids"][src] & article["ids"][other]
    if clash:
        rename = {i: "%s-%s" % (i, other) for i in clash}
        body = article["html"][other]
        for old, new in rename.items():
            body = body.replace('id="%s"' % old, 'id="%s"' % new)
            body = body.replace('href="#%s"' % old, 'href="#%s"' % new)
        article["html"][other] = body
        article["ids"][other] = {rename.get(i, i) for i in article["ids"][other]}
        retoc(article["toc"][other], rename)


MD_LINK = re.compile(r'href="([^"]*)"')


def resolve_links(article, lang, slugs, ids, errors):
    """Point every internal link at its page on this site, and prove it resolves.

    A link that cannot be resolved is a build failure, not a 404 discovered by a
    reader: an article that moved should break the build that moved it.

    Anchors are checked against the same language's ids, on this page and on
    the target page: a Chinese body links to Chinese headings. The page itself
    is the same page in either language, so no URL depends on `lang`.
    """
    here = "%s (%s)" % (article["slug"], lang)

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
            if frag not in article["ids"][lang]:
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
                if frag not in ids.get(stem, {}).get(lang, set()):
                    errors.append("%s: %s has no anchor %r" % (here, stem, frag))
                target += "#" + frag

        elif href.startswith(BASE + "/"):
            path, _, frag = href.partition("#")
            slug = path[len(BASE) + 1 :].strip("/")
            if slug and slug not in slugs:
                errors.append("%s: %s is not an article" % (here, path))
            if frag and frag not in ids.get(slug, {}).get(lang, set()):
                errors.append("%s: %s has no anchor %r" % (here, slug, frag))

        elif href.startswith("/"):
            # A link back to the product site. Only the pages that exist.
            if href.split("#")[0] not in ("/", "/docs/"):
                errors.append("%s: unknown site path %s" % (here, href))

        else:
            errors.append("%s: unresolvable link %s" % (here, href))
            return m.group(0)

        return 'href="%s"' % target

    article["html"][lang] = MD_LINK.sub(one, article["html"][lang])


# ── page ──────────────────────────────────────────────────────────────────


def head(titles, desc, canonical):
    """The document head and top bar. The `<title>` is a `data-en`/`data-zh`
    pair like every other chrome string — `applyLang` walks the whole document,
    head included, so the tab title follows the reader without a special case."""
    return """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-en="%s" data-zh="%s">%s</title>
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
        html.escape(titles["en"], quote=True),
        html.escape(titles["zh"], quote=True),
        html.escape(titles["en"]),
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
  <!-- 右边那句「本站由仓库里的 markdown 生成」已经砍掉：它是说给没人听的，
       想找源码的人去仓库就是了。官网页脚右边那句留着，它讲的是页面上的演示数据是假的
       —— 那句对读者有用。 -->
  <div class="row">
    <span data-en="Open source, Apache-2.0." data-zh="开源，Apache-2.0。">Open source, Apache-2.0.</span>
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
            # No `lang` attribute any more: both titles exist, so the one on
            # screen is always the page language, and it inherits the right
            # font from <html> the way every other chrome string does.
            out.append(
                '      <li><a href="%s/%s/"%s data-en="%s" data-zh="%s">%s</a></li>'
                % (BASE, a["slug"], on,
                   html.escape(a["titles"]["en"], quote=True),
                   html.escape(a["titles"]["zh"], quote=True),
                   html.escape(a["titles"]["en"]))
            )
        out.append("    </ul>")
    out.append("  </div>")
    out.append("</details>")
    return "\n".join(out)


def toc_list(tokens, lang, body_lang):
    """One language's list of h2/h3 links, hidden unless that language is on."""
    out = ['  <ul data-lang="%s"%s lang="%s">'
           % (lang, "" if lang == "en" else " hidden", HTML_LANG[body_lang])]
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
    out.append("  </ul>")
    return "\n".join(out)


def toc_html(article):
    """The right column: the label once, then one list per language.

    Parity guarantees both bodies have the same heading levels, so either both
    languages have a contents or neither does.
    """
    if not any(article["toc"][l] for l in LANGS):
        return ""
    out = [
        '<aside class="dtoc"><nav aria-label="On this page">',
        "  %s" % bi("On this page", "本页目录", tag="p", cls="t"),
    ]
    for l in LANGS:
        out.append(toc_list(article["toc"][l], l, article["body_langs"][l]))
    out.append("</nav></aside>")
    return "\n".join(out)


def crumbs(group, article):
    """Docs / group / this page. The last crumb is a pill, not a link."""
    # 分隔的斜杠由 CSS 的 ::after 画：它是笔画不是内容，而且窄屏上面包屑一换行，
    # 一个孤零零吊在行尾的斜杠比没有它更难看 —— 那时候直接关掉最后一道。
    return (
        '<nav class="crumbs" aria-label="Breadcrumb">'
        '<a href="%s/" data-en="Docs" data-zh="文档">Docs</a>'
        '<span data-en="%s" data-zh="%s">%s</span>'
        '<span class="pill" data-en="%s" data-zh="%s">%s</span>'
        "</nav>"
    ) % (
        BASE,
        html.escape(group["en"], quote=True),
        html.escape(group["zh"], quote=True),
        html.escape(group["en"]),
        html.escape(article["titles"]["en"], quote=True),
        html.escape(article["titles"]["zh"], quote=True),
        html.escape(article["titles"]["en"]),
    )


def pager(prev, nxt):
    if not prev and not nxt:
        return ""
    def name(a):
        return '<span class="n" data-en="%s" data-zh="%s">%s</span>' % (
            html.escape(a["titles"]["en"], quote=True),
            html.escape(a["titles"]["zh"], quote=True),
            html.escape(a["titles"]["en"]),
        )

    out = ['<nav class="pager">']
    if prev:
        out.append(
            '  <a class="p" href="%s/%s/"><span class="k" data-en="Previous" data-zh="上一篇">Previous</span>'
            "%s</a>" % (BASE, prev["slug"], name(prev))
        )
    if nxt:
        out.append(
            '  <a class="n-side" href="%s/%s/"><span class="k" data-en="Next" data-zh="下一篇">Next</span>'
            "%s</a>" % (BASE, nxt["slug"], name(nxt))
        )
    out.append("</nav>")
    return "\n".join(out)


def summarize(article, lang):
    text = re.sub(r"<[^>]+>", "", article["html"][lang])
    text = html.unescape(re.sub(r"\s+", " ", text)).strip()
    return text[:157] + ("…" if len(text) > 157 else "")


def article_page(groups, group, article, prev, nxt):
    toc = toc_html(article)
    body = [
        head(
            {l: article["titles"][l] + SUFFIX[l] for l in LANGS},
            summarize(article, "en"),
            "%s%s/%s/" % (SITE, BASE, article["slug"]),
        ),
        '<div class="dpage%s">' % ("" if toc else " no-toc"),
        group_nav(groups, article["slug"]),
        '<main class="dbody">',
        crumbs(group, article),
    ]
    # Both bodies ship in the markup; docs.js shows one. The `lang` attribute
    # follows the text, not the reader, so a body left untranslated still gets
    # its own typography while the chrome around it switches.
    for l in LANGS:
        body.append(
            '<article lang="%s" data-lang="%s"%s>'
            % (HTML_LANG[article["body_langs"][l]], l, "" if l == "en" else " hidden")
        )
        body.append("<h1>%s</h1>" % html.escape(article["titles"][l]))
        if article["only"] and article["only"] != l:
            body.append(
                '<p class="only" lang="%s">%s</p>' % (HTML_LANG[l], html.escape(ONLY_NOTE[l]))
            )
        body.append(article["html"][l])
        body.append("</article>")
    body += [
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
            {"en": "wanctl docs", "zh": "wanctl 文档"},
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
                '<li><a href="%s/%s/" data-en="%s" data-zh="%s">%s</a></li>'
                % (BASE, a["slug"],
                   html.escape(a["titles"]["en"], quote=True),
                   html.escape(a["titles"]["zh"], quote=True),
                   html.escape(a["titles"]["en"]))
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

    parity = []
    groups = load(args.relay_origin, args.portal_origin, parity)
    if parity:
        print("a translation has drifted from its source:", file=sys.stderr)
        for e in parity:
            print("  " + e, file=sys.stderr)
        sys.exit(1)

    articles = [a for g in groups for a in g["articles"]]
    slugs = {a["slug"] for a in articles}
    if len(slugs) != len(articles):
        sys.exit("two articles want the same slug")

    for a in articles:
        render(a)
    ids = {a["slug"]: a["ids"] for a in articles}

    errors = []
    for a in articles:
        for l in LANGS:
            resolve_links(a, l, slugs, ids, errors)
    if errors:
        print("broken links:", file=sys.stderr)
        for e in errors:
            print("  " + e, file=sys.stderr)
        sys.exit(1)

    untranslated = [a["slug"] for a in articles if a["only"]]

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

    print("%s: index + %d articles × 2 bodies, parity ok, %d links checked, 0 broken"
          % (out.relative_to(ROOT) if out.is_relative_to(ROOT) else out, n,
             sum(len(MD_LINK.findall(a["html"][l])) for a in articles for l in LANGS)))
    if untranslated:
        print("  not translated yet, shown in one language: %s" % ", ".join(untranslated))


if __name__ == "__main__":
    main()
