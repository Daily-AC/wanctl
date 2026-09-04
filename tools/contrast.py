"""对比度是算的，不是估的。

真源是每个表面自己的 :root —— 这里只读它们，不再抄一份色值，
省得像 LOTO 那版一样：调色板换了，校验工具还在验早就下线的颜色。

三个表面共用一套 token，但各自用到的组合不同（门户多一个语义红、
一个当前项底色，也没有深色整幅屏），所以配对表分开写。

文档站没有自己的调色板：它先加载官网的 app.css 再加载 docs.css，
docs.css 的 :root 只补一个当前项底色。所以它的 token 是两个文件叠出来的，
一个表面可以给多份样式表，按加载顺序合并——跟浏览器看到的一样。

  python3 tools/contrast.py
"""
import re
import sys
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent


def tokens(*paths: pathlib.Path) -> dict:
    """合并若干张样式表的 :root，后加载的覆盖先加载的。"""
    out = {}
    for path in paths:
        css = path.read_text(encoding='utf-8')
        root = re.search(r':root\s*\{(.*?)\}', css, re.S)
        if not root:
            sys.exit('no :root block in %s' % path)
        out.update(re.findall(r'--([\w-]+)\s*:\s*(#[0-9a-fA-F]{3,8})\s*;', root.group(1)))
    return out


def srgb(h: str):
    h = h.lstrip('#')
    if len(h) == 3:
        h = ''.join(c * 2 for c in h)
    return tuple(int(h[i:i + 2], 16) / 255 for i in (0, 2, 4))


def lum(c):
    def lin(x):
        return x / 12.92 if x <= 0.04045 else ((x + 0.055) / 1.055) ** 2.4
    r, g, b = (lin(v) for v in c)
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def ratio(a, b):
    la, lb = lum(srgb(a)), lum(srgb(b))
    hi, lo = max(la, lb), min(la, lb)
    return (hi + 0.05) / (lo + 0.05)


# (前景, 背景, 门槛, 说明)。门槛 4.5 = 正文，3.0 = 大字与图形。
# 承载文字的每一对都必须在这张表里；只做装饰的那对要写清楚它不承载文字。
SITE = [
    ('ink',       'canvas',   4.5, 'body on white'),
    ('ink',       'canvas-2', 4.5, 'body on grey tile'),
    ('ink-2',     'canvas',   4.5, 'secondary on white'),
    ('ink-2',     'canvas-2', 4.5, 'secondary on grey tile'),
    ('blue',      'canvas',   4.5, 'links on white'),
    ('blue',      'canvas-2', 4.5, 'links on grey tile'),
    ('on-dark',   'dark',     4.5, 'body on dark tile'),
    ('on-dark-2', 'dark',     4.5, 'secondary on dark tile'),
    ('canvas',    'blue',     4.5, 'button label on Action Blue'),
    ('blue',      'canvas',   3.0, 'the wire and the ring, as graphics'),
    ('hairline',  'canvas',   1.0, 'hairline — a rule, not text'),
    ('ink-3',     'canvas',   4.5, 'ink-3 — DECORATION ONLY, expected to fail'),
]

PORTAL = [
    ('ink',       'canvas',    4.5, 'body on white'),
    ('ink',       'canvas-2',  4.5, 'body on the action bar / grey card'),
    ('ink-2',     'canvas',    4.5, 'secondary on white'),
    ('ink-2',     'canvas-2',  4.5, 'secondary on the action bar'),
    ('blue',      'canvas',    4.5, 'links and counts on white'),
    ('blue',      'canvas-2',  4.5, 'links on the action bar'),
    ('blue',      'blue-wash', 4.5, "settings nav — current item's label on its wash"),
    ('canvas',    'blue',      4.5, 'button label on Action Blue'),
    ('canvas',    'ink',       4.5, 'toast label on ink'),
    # 语义红。它不是第二个强调色：只陈述状态（denied / 投递失败 / 危险命令首词），
    # 从不承担「点我」。承载文字，所以按正文门槛验。
    ('red',       'canvas',    4.5, 'denied / danger, as text on white'),
    ('red',       'canvas-2',  4.5, 'denied / danger, as text on a grey card'),
    ('canvas',    'red',       4.5, 'toast label on the failure toast'),
    ('hairline',  'canvas',    1.0, 'hairline — a rule, not text'),
    ('ink-3',     'canvas',    4.5, 'ink-3 — DECORATION ONLY, expected to fail'),
]

# 文档站：官网的 token + docs.css 补的当前项底色。没有深色整幅屏，
# 也没有按钮 —— 它只有正文、链接、代码块底和两处浅蓝。
DOCS = [
    ('ink',       'canvas',    4.5, 'body on white'),
    ('ink',       'canvas-2',  4.5, 'code, inline and in blocks, on its grey'),
    ('ink-2',     'canvas',    4.5, 'group names, breadcrumbs, on-page contents'),
    ('ink-2',     'canvas-2',  4.5, 'the same, hovered onto grey'),
    ('blue',      'canvas',    4.5, 'links on white'),
    ('blue',      'canvas-2',  4.5, 'links hovered onto grey'),
    ('blue',      'blue-wash', 4.5, "current article, and the last breadcrumb"),
    ('hairline',  'canvas',    1.0, 'hairline — the only thing separating the columns'),
    ('ink-3',     'canvas',    4.5, 'ink-3 — DECORATION ONLY, expected to fail'),
]

SURFACES = [
    ('site',   [ROOT / 'site' / 'assets' / 'app.css'],           SITE),
    ('portal', [ROOT / 'internal' / 'portal' / 'web' / 'app.css'], PORTAL),
    ('docs',   [ROOT / 'site' / 'assets' / 'app.css',
                ROOT / 'site' / 'assets' / 'docs.css'],           DOCS),
]

EXPECTED_FAIL = {'ink-3'}

bad = 0
for name, paths, pairs in SURFACES:
    t = tokens(*paths)
    print('\n=== %s — %s' % (name, ' + '.join(str(p.relative_to(ROOT)) for p in paths)))
    print('%-42s %6s  %s' % ('pair', 'ratio', 'verdict'))
    for fg, bg, need, note in pairs:
        if fg not in t or bg not in t:
            sys.exit('unknown token in %s pair: %s on %s' % (name, fg, bg))
        r = ratio(t[fg], t[bg])
        if r >= need:
            verdict = 'PASS  (%s)' % note
        elif fg in EXPECTED_FAIL:
            verdict = 'fails by design  (%s)' % note
        else:
            verdict = 'FAIL need %.1f  (%s)' % (need, note)
            bad += 1
        print('%-42s %6.2f  %s' % ('%s on %s' % (fg, bg), r, verdict))

print('\n%d unexpected failure(s)' % bad)
sys.exit(1 if bad else 0)
