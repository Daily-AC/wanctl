"""对比度是算的，不是估的。

真源是 site/assets/app.css 的 :root —— 这里只读它，不再抄一份色值，
省得像 LOTO 那版一样：调色板换了，校验工具还在验早就下线的颜色。

  python3 tools/contrast.py
"""
import re
import sys
import pathlib

CSS = pathlib.Path(__file__).resolve().parent.parent / 'site' / 'assets' / 'app.css'


def tokens(css: str) -> dict:
    root = re.search(r':root\s*\{(.*?)\}', css, re.S)
    if not root:
        sys.exit('no :root block in %s' % CSS)
    out = {}
    for name, value in re.findall(r'--([\w-]+)\s*:\s*(#[0-9a-fA-F]{3,8})\s*;', root.group(1)):
        out[name] = value
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
PAIRS = [
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
EXPECTED_FAIL = {'ink-3'}

t = tokens(CSS.read_text(encoding='utf-8'))
print('tokens read from %s\n' % CSS.relative_to(CSS.parents[2]))
for k in sorted(t):
    print('  --%-10s %s' % (k, t[k]))

print('\n%-38s %6s  %s' % ('pair', 'ratio', 'verdict'))
bad = 0
for fg, bg, need, note in PAIRS:
    if fg not in t or bg not in t:
        sys.exit('unknown token in pair: %s on %s' % (fg, bg))
    r = ratio(t[fg], t[bg])
    ok = r >= need
    if ok:
        verdict = 'PASS  (%s)' % note
    elif fg in EXPECTED_FAIL:
        verdict = 'fails by design  (%s)' % note
    else:
        verdict = 'FAIL need %.1f  (%s)' % (need, note)
        bad += 1
    print('%-38s %6.2f  %s' % ('%s on %s' % (fg, bg), r, verdict))

print('\n%d unexpected failure(s)' % bad)
sys.exit(1 if bad else 0)
