import math

def oklch_to_srgb(L, C, h):
    hr = math.radians(h); a = C*math.cos(hr); b = C*math.sin(hr)
    l_ = L + 0.3963377774*a + 0.2158037573*b
    m_ = L - 0.1055613458*a - 0.0638541728*b
    s_ = L - 0.0894841775*a - 1.2914855480*b
    l, m, s = l_**3, m_**3, s_**3
    r =  4.0767416621*l - 3.3077115913*m + 0.2309699292*s
    g = -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
    bb=-0.0041960863*l - 0.7034186147*m + 1.7076147010*s
    def enc(x):
        x = max(0.0, min(1.0, x))
        return 12.92*x if x <= 0.0031308 else 1.055*(x**(1/2.4)) - 0.055
    return tuple(enc(v) for v in (r, g, bb))

def lum(srgb):
    def lin(c):
        return c/12.92 if c <= 0.04045 else ((c+0.055)/1.055)**2.4
    r,g,b = (lin(c) for c in srgb)
    return 0.2126*r + 0.7152*g + 0.0722*b

def ratio(c1, c2):
    a, b = lum(c1), lum(c2)
    hi, lo = max(a,b), min(a,b)
    return (hi+0.05)/(lo+0.05)

def hexs(srgb):
    return '#' + ''.join('%02X' % round(max(0,min(1,c))*255) for c in srgb)

T = {
 'ground':  (0.972, 0.002,  250),
 'surface': (0.988, 0.0015, 250),
 'ink':     (0.185, 0.004,  250),
 'ink-2':   (0.485, 0.006,  250),
 'ink-3':   (0.640, 0.005,  250),
 'line':    (0.885, 0.004,  250),
 'line-2':  (0.815, 0.005,  250),
 'tag':     (0.874, 0.184,  92),
 'vermil':  (0.552, 0.196,  30),
 'live':    (0.605, 0.155,  150),
}
rgb = {k: oklch_to_srgb(*v) for k,v in T.items()}
for k in T: print('%-8s %s' % (k, hexs(rgb[k])))

print('\n%-34s %6s  %s' % ('pair', 'ratio', 'verdict (body>=4.5, large>=3)'))
pairs = [
 ('ink   on ground',  'ink','ground', 4.5),
 ('ink-2 on ground',  'ink-2','ground', 4.5),
 ('ink-3 on ground',  'ink-3','ground', 4.5),
 ('ink-2 on surface', 'ink-2','surface', 4.5),
 ('ink-3 on surface', 'ink-3','surface', 4.5),
 ('ink   on tag',     'ink','tag', 4.5),
 ('vermil on ground', 'vermil','ground', 4.5),
 ('vermil on surface','vermil','surface', 4.5),
 ('live  on ground',  'live','ground', 3.0),
]
for name,a,b,need in pairs:
    r = ratio(rgb[a], rgb[b])
    print('%-34s %6.2f  %s' % (name, r, 'PASS' if r >= need else 'FAIL (need %.1f)' % need))
