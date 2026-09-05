#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pillow", "numpy", "scipy"]
# ///
"""
tools/frames.py — 真机外壳：量出屏在哪，把壳裁好缩好。

首屏那两台机器（site/index.html 的 .laptop / .phone）用的是真机渲染图：带透明通道的 PNG，
屏幕那块是透明的洞。活界面垫在壳底下，壳压在上面（pointer-events:none），
所以要精确知道那个洞在壳里的位置 —— 这个脚本就是量那个的。

    uv run tools/frames.py <frame.png> --out site/assets/iphone.webp --width 900

它做三件事：
  1. 找到壳（不透明像素的外接框）和屏（壳中心所在的那个透明连通块）。
  2. 算出「屏那一层」该有的矩形和圆角：矩形比洞每边多出 --expand 像素（洞边上有
     一两像素的抗锯齿灰边，要压在屏底下而不是压在页面白底上）；圆角取一个区间的中点 ——
     区间下界是「角不戳出壳外」，上界是「角不露出洞的圆角」，两头都是按像素验的。
  3. 把壳裁到外接框、按 --width 缩放（RGBa 预乘之后再缩，透明边缘不发黑），
     --out 是 .webp 就存有损 q95 + 无损 alpha 的 WebP，否则存 PNG。

打印出来的是给 CSS 用的百分比（相对壳的宽 / 高），圆角是壳宽的百分比（配合 cqw）。
"""
import argparse
import pathlib
import sys

import numpy as np
from PIL import Image
from scipy import ndimage


def rounded_mask(h, w, r, corners):
    """h×w 的布尔掩膜：矩形，四角按 corners=(tl,tr,bl,br) 决定要不要磨成半径 r 的圆角。"""
    m = np.ones((h, w), dtype=bool)
    if r <= 0:
        return m
    yy, xx = np.mgrid[0:r, 0:r]
    # 左上角那个 r×r 的窗：圆心在 (r-0.5, r-0.5)，窗外一律保留
    d2 = (xx + 0.5 - r) ** 2 + (yy + 0.5 - r) ** 2
    cut = d2 > r * r
    tl, tr, bl, br = corners
    if tl: m[:r, :r] &= ~cut
    if tr: m[:r, w - r:] &= ~cut[:, ::-1]
    if bl: m[h - r:, :r] &= ~cut[::-1, :]
    if br: m[h - r:, w - r:] &= ~cut[::-1, ::-1]
    return m


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("frame")
    ap.add_argument("--out", required=True)
    ap.add_argument("--width", type=int, required=True, help="导出的壳宽（像素）")
    ap.add_argument("--expand", type=int, default=4, help="屏那一层比洞每边多出几个源像素")
    ap.add_argument("--safety", type=int, default=3, help="离壳外缘至少留几个源像素")
    args = ap.parse_args()

    im = Image.open(args.frame).convert("RGBA")
    a = np.asarray(im)[:, :, 3]
    H, W = a.shape

    ys, xs = np.where(a > 0)
    bx0, bx1, by0, by1 = xs.min(), xs.max() + 1, ys.min(), ys.max() + 1
    bw, bh = bx1 - bx0, by1 - by0

    lab, _ = ndimage.label(a == 0)
    outer = lab == lab[0, 0]
    cy, cx = (by0 + by1) // 2, (bx0 + bx1) // 2
    if lab[cy, cx] == 0 or lab[cy, cx] == lab[0, 0]:
        sys.exit("壳的中心不是一个封闭的透明洞 —— 这张图不是我们要的那种外壳")
    cut = lab == lab[cy, cx]
    ys, xs = np.where(cut)
    sx0, sx1, sy0, sy1 = xs.min(), xs.max() + 1, ys.min(), ys.max() + 1
    sw, sh = sx1 - sx0, sy1 - sy0

    # 洞里有没有不透明的东西（灵动岛 / 刘海）：只看洞上部正中那一块 ——
    # 洞的外接框四个角落着的是圆角处的边框像素，不能算
    qx0, qx1, qy1 = sx0 + sw // 4, sx1 - sw // 4, sy0 + sh // 4
    inside = (a > 0)[sy0:qy1, qx0:qx1]
    island = None
    if inside.any():
        iy, ix = np.where(inside)
        island = (ix.min() + qx0, iy.min() + sy0, ix.max() + 1 + qx0, iy.max() + 1 + sy0)

    # 洞的四个角是圆的还是方的：角上那个像素在洞里就是方的
    corners = (not cut[sy0, sx0], not cut[sy0, sx1 - 1], not cut[sy1 - 1, sx0], not cut[sy1 - 1, sx1 - 1])

    e = args.expand
    lx0, ly0, lx1, ly1 = sx0 - e, sy0 - e, sx1 + e, sy1 + e
    lw, lh = lx1 - lx0, ly1 - ly0
    forbidden = ndimage.binary_dilation(outer, iterations=args.safety)

    win_cut = cut[ly0:ly1, lx0:lx1]
    win_forb = forbidden[ly0:ly1, lx0:lx1]

    def covers(r):
        return not np.any(win_cut & ~rounded_mask(lh, lw, r, corners))

    def contained(r):
        return not np.any(win_forb & rounded_mask(lh, lw, r, corners))

    rmax_lim = min(lw, lh) // 2 - 1
    if not covers(0):
        sys.exit("连直角矩形都盖不住洞 —— expand 给小了？")
    # covers 随 r 单调递减，contained 随 r 单调递增：各二分一次
    lo, hi = 0, rmax_lim
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if covers(mid): lo = mid
        else: hi = mid - 1
    r_cover = lo
    lo, hi = 0, rmax_lim
    while lo < hi:
        mid = (lo + hi) // 2
        if contained(mid): hi = mid
        else: lo = mid + 1
    r_contain = lo if contained(lo) else None
    if r_contain is None or r_contain > r_cover:
        sys.exit(f"没有可行的圆角：盖住洞要 r<={r_cover}，不戳出壳要 r>={r_contain}")
    r = (r_cover + r_contain) // 2

    scale = args.width / bw
    pct = lambda v, base: f"{100.0 * v / base:.3f}%"
    print(f"{args.frame}: {W}x{H}")
    print(f"  壳   x {bx0}..{bx1}  y {by0}..{by1}   {bw}x{bh}  (宽高比 {bw/bh:.5f})")
    print(f"  洞   x {sx0}..{sx1}  y {sy0}..{sy1}   {sw}x{sh}  四角圆? tl/tr/bl/br={corners}")
    if island:
        ix0, iy0, ix1, iy1 = island
        print(f"  岛   x {ix0}..{ix1}  y {iy0}..{iy1}   {ix1-ix0}x{iy1-iy0}"
              f"   相对壳宽: left {pct(ix0-bx0, bw)} right {pct(bx1-ix1, bw)} "
              f"top {pct(iy0-by0, bw)} h {pct(iy1-iy0, bw)}  中线 {pct((iy0+iy1)/2-by0, bw)}")
    print(f"  屏层 (洞外扩 {e}px)  圆角可行区间 [{r_contain}, {r_cover}] -> 取 {r}")
    print(f"    CSS (相对壳):  left {pct(lx0-bx0, bw)}  top {pct(ly0-by0, bh)}"
          f"  width {pct(lw, bw)}  height {pct(lh, bh)}")
    print(f"    圆角: {100.0*r/bw:.3f}cqw (壳宽)  = {r*scale:.1f}px @ 壳宽 {args.width}"
          f"   屏层宽高比 {lw/lh:.5f}")
    if island:
        print(f"    屏层内 (壳宽): 岛 left {pct(island[0]-lx0, bw)} right {pct(lx1-island[2], bw)}"
              f" w {pct(island[2]-island[0], bw)} 中线 {pct((island[1]+island[3])/2-ly0, bw)}"
              f" 底 {pct(island[3]-ly0, bw)}")

    out = im.crop((bx0, by0, bx1, by1)).convert("RGBa")
    out = out.resize((args.width, round(bh * scale)), Image.LANCZOS).convert("RGBA")
    if args.out.lower().endswith(".webp"):
        # 有损 + 无损 alpha：壳是大片平色加一圈金属边，q95 在显示尺寸上看不出差别，
        # 体积是 PNG 的四分之一。alpha 必须无损 —— 洞的边缘就是屏的边缘。
        out.save(args.out, "WEBP", quality=95, method=6, alpha_quality=100)
    else:
        out.save(args.out, optimize=True)
    print(f"  -> {args.out}  {out.size[0]}x{out.size[1]}  {pathlib.Path(args.out).stat().st_size // 1024}K")


if __name__ == "__main__":
    main()
