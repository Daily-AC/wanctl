/* wanctl product site — the character field behind the hero.
 *
 * 首屏背景：一片看不见的密文，鼠标划过才显形。
 *   relay 看得见的只有密文（DESIGN.md §6、首页第四屏），所以桌面底下铺着的字符就是
 *   那种东西：base64 的字节，跟页面上那两条指纹同一套字母表。
 *   静止时什么都没有 —— 首屏就是原来那块白。鼠标划过：经过的地方解析出一团字节，
 *   边缘是「·」，中心是字，同一格会闪着换字，然后半秒内散掉。指针停住不动，字也跟着
 *   散干净（甲方 09-05 的要求：鼠标不动的时候看不到字符）。
 *   点一下：一圈蓝色的波从点的地方推出去 —— 蓝在这套系统里是「活的 / 已核对」，
 *   这一下是「你点了头」。手机屏里按「允许一次」也会推出一圈：那正是那个动作。
 *   波和透镜露出来的形状不是均匀的圆盘：底下有一层慢慢演化的噪声在决定哪几格是字、
 *   哪几格是点、哪几格是空 —— 像手电筒照在一片本来就在那里的东西上。
 *
 * 底纹（同一天甲方追加的第二层，静止时就在）：
 *   一团光 —— 蓝色的径向渐变，落在首屏正中。svg，静的。
 *   几圈线 —— 同心圆，很淡，静止。每圈一到三段高光弧沿着圈流：头亮、尾巴渐隐成一条
 *   彗尾，相邻两圈反向、相位错开。这就是甲方要的「线条渐变 + 流动」，第五轮他说明白了：
 *   「是高光弧形沿着圆圈流动」。第三轮做的正是这个，但圈本身太亮、光太淡，看不出来；
 *   第四轮误会成圈自己往外漫。画在这块 canvas 上，30fps。
 *   圆心是首屏的几何中心。第三轮追着两块屏之间那个点（relay 站的位置）跑，
 *   页面居中的标题和 CTA 衬得它偏右，甲方说「不居中」—— 意象让位给版式。
 *
 * 字符和圈线都直接从标题底下过。曾经给标题、导语、CTA 挖过一块池子（那里不画字、
 * 遮掉圈线），甲方看了两轮都说「标题有一层背景色」—— 周围有纹理、中间是白，
 * 眼睛就把那块白读成一块底色。删了。**别再挖。**
 *
 * 纪律（DESIGN.md §7.5 记了为什么）：
 *   · 灰只用 --ink-3，蓝只用 --blue。没有第三种颜色。
 *     色值从 :root 读，不抄 —— tools/contrast.py 验的就是那份，抄一份就会漂。
 *   · 静止时不画一个字符，所以「密排噪点场」那条硬禁碰不到它。
 *   · 静止时只画那几圈线和线上的光，30fps；有人碰才跟着 rAF 走。
 *   · prefers-reduced-motion：那团光照常，圈和高光弧画一帧不再动，不装监听、不画字。
 *     无 JS：白底。
 *   · 画布 pointer-events:none，事件挂在 section 上；手机屏里那三个按钮照常能按。
 *   · 首屏滚出视口、标签页藏起来：停。
 *
 * 验收接口 window.__field：stats() / move(x,y) / tap(x,y) / rebuild()，坐标是相对首屏的 CSS px
 */
(function () {
  'use strict';

  var hero = document.querySelector('.tile.hero');
  var canvas = document.getElementById('field');
  if (!hero || !canvas || !canvas.getContext) return;
  var ctx = canvas.getContext('2d');
  if (!ctx) return;

  var REDUCED = !!(window.matchMedia &&
                   window.matchMedia('(prefers-reduced-motion: reduce)').matches);

  /* ── 颜色和字体都从 app.css 的 :root 读 ─────────────────────────────── */
  var css = getComputedStyle(document.documentElement);
  function token(name, fallback) {
    var v = css.getPropertyValue(name);
    return v && v.trim() ? v.trim() : fallback;
  }
  var GREY = token('--ink-3', '#86868b');
  var BLUE = token('--blue', '#0066cc');
  var MONO = token('--mono', "'JetBrains Mono',ui-monospace,Menlo,monospace");

  /* base64 的字母表。索引 64 是那个点。 */
  var BYTES = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
  var DOT = '·';
  var NB = BYTES.length;
  var GLYPHS = BYTES + DOT;
  var NG = GLYPHS.length;

  /* ── 旋钮 ──────────────────────────────────────────────────────────────
     全在这一处。长度单位是 CSS px，时间单位是秒。 */
  var P = {
    /* 格子。行高 1.75 跟 .term 一样；窄屏字号降一档，格子跟着收 */
    cellW: 13, cellH: 21, font: 12,
    cellWNarrow: 12, cellHNarrow: 20, fontNarrow: 11,
    /* 底下那层噪声：采样尺度（单位：格）、演化速度；露出来的格子乘上 [texLo, 1] */
    nsx: 1 / 9, nsy: 1 / 5.5, evolve: 0.12, texLo: 0.45,
    /* 能量到画法：低于 floor 不画；floor..byteAt 画点；再往上画字节 */
    floor: 0.06, byteAt: 0.2,
    /* 指针：透镜半径，划得快半径会再长一点（封顶 lensGrow 倍）；注入的峰值；尾巴的时间常数 */
    lensR: 120, lensGrow: 1.5, lensPeak: 1.2, trail: 0.55,
    /* 涟漪：波速、环宽、峰值、幅度的时间常数、并存上限；蓝色退去的时间常数 */
    waveV: 640, waveW: 56, wavePeak: 1.15, waveLife: 0.8, waveMax: 8, blueFade: 0.5,
    /* 换字：每格每秒的概率 —— 底数，加上被激活时的 */
    flickIdle: 0.4, flickHot: 22,
    /* 光：半径（按首屏长边算）和峰值透明度 */
    washR: 0.55, washA: 0.06,
    /* 圈：起始半径、间距、线宽、透明度（远处再淡） */
    ringR0: 120, ringStep: 120, ringW: 1, ringA: 0.14,
    /* 高光弧：弧长（小圈上封顶为周长的六成）、沿线速度、头部透明度、线宽、
       柔边的模糊半径（甲方要的「调低透明度、带点模糊」）、每多少周长再加一段（相邻两圈反向） */
    arcLen: 340, arcV: 90, arcA: 0.5, arcW: 1.5, arcBlur: 4, arcEvery: 3000,
    /* 没人碰的时候只画圈和光，这个帧率就够 */
    idleFps: 30
  };

  /* ── 3D simplex 噪声（Gustavson 的公版实现），固定种子 ─────────────────
     种子固定，同一个位置在同一时刻露出来的形状每次加载都一样：截图才可比。 */
  var grad3 = [[1,1,0],[-1,1,0],[1,-1,0],[-1,-1,0],[1,0,1],[-1,0,1],
               [1,0,-1],[-1,0,-1],[0,1,1],[0,-1,1],[0,1,-1],[0,-1,-1]];
  var perm = new Uint8Array(512), perm12 = new Uint8Array(512);
  (function () {
    var p = [], i, j, tmp, s = 1337;
    for (i = 0; i < 256; i++) p[i] = i;
    for (i = 255; i > 0; i--) {
      s = (s * 16807) % 2147483647; j = s % (i + 1);
      tmp = p[i]; p[i] = p[j]; p[j] = tmp;
    }
    for (i = 0; i < 512; i++) { perm[i] = p[i & 255]; perm12[i] = perm[i] % 12; }
  })();
  var F3 = 1 / 3, G3 = 1 / 6;
  function noise3(xin, yin, zin) {
    var s = (xin + yin + zin) * F3;
    var i = Math.floor(xin + s), j = Math.floor(yin + s), k = Math.floor(zin + s);
    var t = (i + j + k) * G3;
    var x0 = xin - (i - t), y0 = yin - (j - t), z0 = zin - (k - t);
    var i1, j1, k1, i2, j2, k2;
    if (x0 >= y0) {
      if (y0 >= z0)      { i1 = 1; j1 = 0; k1 = 0; i2 = 1; j2 = 1; k2 = 0; }
      else if (x0 >= z0) { i1 = 1; j1 = 0; k1 = 0; i2 = 1; j2 = 0; k2 = 1; }
      else               { i1 = 0; j1 = 0; k1 = 1; i2 = 1; j2 = 0; k2 = 1; }
    } else {
      if (y0 < z0)       { i1 = 0; j1 = 0; k1 = 1; i2 = 0; j2 = 1; k2 = 1; }
      else if (x0 < z0)  { i1 = 0; j1 = 1; k1 = 0; i2 = 0; j2 = 1; k2 = 1; }
      else               { i1 = 0; j1 = 1; k1 = 0; i2 = 1; j2 = 1; k2 = 0; }
    }
    var x1 = x0 - i1 + G3, y1 = y0 - j1 + G3, z1 = z0 - k1 + G3;
    var x2 = x0 - i2 + 2 * G3, y2 = y0 - j2 + 2 * G3, z2 = z0 - k2 + 2 * G3;
    var x3 = x0 - 1 + 3 * G3, y3 = y0 - 1 + 3 * G3, z3 = z0 - 1 + 3 * G3;
    var ii = i & 255, jj = j & 255, kk = k & 255, g, n = 0;
    var t0 = 0.6 - x0 * x0 - y0 * y0 - z0 * z0;
    if (t0 > 0) { g = grad3[perm12[ii + perm[jj + perm[kk]]]]; t0 *= t0; n += t0 * t0 * (g[0] * x0 + g[1] * y0 + g[2] * z0); }
    var t1 = 0.6 - x1 * x1 - y1 * y1 - z1 * z1;
    if (t1 > 0) { g = grad3[perm12[ii + i1 + perm[jj + j1 + perm[kk + k1]]]]; t1 *= t1; n += t1 * t1 * (g[0] * x1 + g[1] * y1 + g[2] * z1); }
    var t2 = 0.6 - x2 * x2 - y2 * y2 - z2 * z2;
    if (t2 > 0) { g = grad3[perm12[ii + i2 + perm[jj + j2 + perm[kk + k2]]]]; t2 *= t2; n += t2 * t2 * (g[0] * x2 + g[1] * y2 + g[2] * z2); }
    var t3 = 0.6 - x3 * x3 - y3 * y3 - z3 * z3;
    if (t3 > 0) { g = grad3[perm12[ii + 1 + perm[jj + 1 + perm[kk + 1]]]]; t3 *= t3; n += t3 * t3 * (g[0] * x3 + g[1] * y3 + g[2] * z3); }
    return 32 * n;   /* [-1, 1] */
  }
  function smooth(a, b, x) {
    x = (x - a) / (b - a);
    if (x <= 0) return 0;
    if (x >= 1) return 1;
    return x * x * (3 - 2 * x);
  }
  /* 底下那层：两个八度，[texLo, 1]。它只在有东西照到的时候才被看见。 */
  function texture(col, row, t) {
    var x = col * P.nsx, y = row * P.nsy, z = t * P.evolve;
    var n = noise3(x, y, z) * 0.68 + noise3(x * 2.3 + 7.1, y * 2.3 + 3.7, z * 1.6 + 11) * 0.32;
    return P.texLo + (1 - P.texLo) * smooth(0.3, 0.72, n * 0.5 + 0.5);
  }

  /* ── 网格 ──────────────────────────────────────────────────────────────
     全程用设备像素：格子尺寸取整，字形落在整像素上才不发虚。 */
  var dpr = 1, W = 0, H = 0, cw = 0, ch = 0, fontPx = 0, diag = 0;
  var cols = 0, rows = 0, ox = 0, oy = 0, N = 0;
  var chars = null, act = null, blu = null;
  var atlas = null, ACOLS = 16;
  var stats = { drawn: 0, dots: 0, rings: 0, ms: 0 };

  function measure() {
    var narrow = window.innerWidth < 560;
    return narrow ? { w: P.cellWNarrow, h: P.cellHNarrow, f: P.fontNarrow }
                  : { w: P.cellW, h: P.cellH, f: P.font };
  }

  function build() {
    var m = measure(), r = hero.getBoundingClientRect(), i;
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    W = Math.max(1, Math.round(r.width * dpr));
    H = Math.max(1, Math.round(r.height * dpr));
    diag = Math.sqrt(W * W + H * H);
    if (canvas.width !== W || canvas.height !== H) { canvas.width = W; canvas.height = H; }
    cw = Math.round(m.w * dpr); ch = Math.round(m.h * dpr); fontPx = Math.round(m.f * dpr);
    cols = Math.floor(W / cw); rows = Math.floor(H / ch);
    ox = Math.floor((W - cols * cw) / 2); oy = Math.floor((H - rows * ch) / 2);
    N = cols * rows;
    chars = new Uint8Array(N); act = new Float32Array(N); blu = new Float32Array(N);
    for (i = 0; i < N; i++) chars[i] = (Math.random() * NB) | 0;
    atlasBuild();
    ctx.clearRect(0, 0, W, H);
    stats.drawn = 0; stats.dots = 0;
  }

  /* 字形图集：65 个字形 × 两种颜色，每格一张。逐格 drawImage 比 fillText 便宜一个量级。 */
  function atlasBuild() {
    atlas = document.createElement('canvas');
    atlas.width = ACOLS * cw;
    atlas.height = Math.ceil(NG * 2 / ACOLS) * ch;
    var a = atlas.getContext('2d'), k, i, t;
    a.font = fontPx + 'px ' + MONO;
    if (a.font.indexOf('JetBrains') < 0) a.font = fontPx + 'px "JetBrains Mono",ui-monospace,Menlo,monospace';
    a.textAlign = 'center'; a.textBaseline = 'middle';
    for (k = 0; k < 2; k++) {
      a.fillStyle = k ? BLUE : GREY;
      for (i = 0; i < NG; i++) {
        t = k * NG + i;
        a.fillText(GLYPHS.charAt(i), (t % ACOLS) * cw + cw / 2, Math.floor(t / ACOLS) * ch + ch / 2 + 0.5);
      }
    }
  }

  /* ── 光 ────────────────────────────────────────────────────────────────
     一块 svg，只有那团径向渐变，圆心在首屏正中。viewBox 跟首屏同尺寸。 */
  var SVG = 'http://www.w3.org/2000/svg';
  var backdrop = null;
  function node(name, attrs, parent) {
    var n = document.createElementNS(SVG, name), k;
    for (k in attrs) n.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(n);
    return n;
  }
  function backdropBuild() {
    var hr = hero.getBoundingClientRect(), w = Math.round(hr.width), h = Math.round(hr.height);
    if (!backdrop) {
      backdrop = node('svg', { 'class': 'backdrop', 'aria-hidden': 'true' });
      hero.insertBefore(backdrop, hero.firstChild);
    }
    while (backdrop.firstChild) backdrop.removeChild(backdrop.firstChild);
    backdrop.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    var defs = node('defs', {}, backdrop);
    var wash = node('radialGradient', { id: 'bd-wash', gradientUnits: 'userSpaceOnUse',
                                        cx: w / 2, cy: h / 2, r: Math.max(w, h) * P.washR }, defs);
    node('stop', { offset: '0', 'stop-color': BLUE, 'stop-opacity': P.washA }, wash);
    node('stop', { offset: '1', 'stop-color': BLUE, 'stop-opacity': '0' }, wash);
    node('rect', { width: w, height: h, fill: 'url(#bd-wash)' }, backdrop);
  }

  /* ── 圈和圈上的高光 ──────────────────────────────────────────────────
     圆心在首屏正中。圈是静止的淡蓝细线；每圈按周长分一到三段高光弧，沿线以恒定的
     弧长速度流（大圈小圈上的光走得一样快），相邻两圈反向、相位错开。
     高光是一条彗尾：头最亮，往后渐隐 —— 用 conic 渐变沿着弧画。先把坐标系转到头所在的
     角度，渐变和弧都在这个坐标系里定义，头永远在角度 0，尾巴拖在它前面或后面。
     没有 createConicGradient 的老浏览器（Safari 16.2 之前）退回叠几段等透明度的弧。 */
  var blueRGB = (function () {
    var m = /^#([0-9a-f]{6})$/i.exec(BLUE);
    if (!m) return '0,102,204';
    var v = parseInt(m[1], 16);
    return (v >> 16 & 255) + ',' + (v >> 8 & 255) + ',' + (v & 255);
  })();
  var CONIC = typeof ctx.createConicGradient === 'function';
  function rgba(a) { return 'rgba(' + blueRGB + ',' + a.toFixed(3) + ')'; }
  function comet(cx, cy, r, head, len, dir, alpha) {
    var f = len / (2 * Math.PI), g, k, n = 6, from, to, s0, s1;
    ctx.save();
    ctx.translate(cx, cy);
    ctx.rotate(head);
    /* 柔边：同色的阴影不偏移，就是一圈高斯模糊的光晕；描边本身透明，晕跟着渐变一起淡 */
    if (P.arcBlur > 0) { ctx.shadowColor = rgba(alpha); ctx.shadowBlur = P.arcBlur * dpr; }
    /* dir > 0 顺时针跑：尾巴在 [-len, 0]；dir < 0 逆时针跑：尾巴在 [0, len] */
    from = dir > 0 ? -len : 0;
    to = dir > 0 ? 0 : len;
    if (CONIC) {
      g = ctx.createConicGradient(from, 0, 0);
      if (dir > 0) {
        g.addColorStop(0, rgba(0));
        g.addColorStop(f * 0.5, rgba(alpha * 0.22));
        g.addColorStop(f * 0.9, rgba(alpha * 0.75));
        g.addColorStop(f, rgba(alpha));
        g.addColorStop(Math.min(1, f + 0.002), rgba(0));
      } else {
        g.addColorStop(0, rgba(alpha));
        g.addColorStop(f * 0.1, rgba(alpha * 0.75));
        g.addColorStop(f * 0.5, rgba(alpha * 0.22));
        g.addColorStop(f, rgba(0));
      }
      g.addColorStop(1, rgba(0));
      ctx.strokeStyle = g;
      ctx.beginPath();
      ctx.arc(0, 0, r, from, to);
      ctx.stroke();
    } else {
      ctx.strokeStyle = BLUE;
      for (k = 0; k < n; k++) {
        /* 六段，越靠头越亮：每段从头往尾各占 len 的一份，透明度按头部往下阶梯 */
        s0 = dir > 0 ? -len * (k + 1) / n : len * k / n;
        s1 = dir > 0 ? -len * k / n : len * (k + 1) / n;
        ctx.globalAlpha = alpha * (1 - k / n) * (1 - k / n);
        ctx.beginPath();
        ctx.arc(0, 0, r, s0, s1);
        ctx.stroke();
      }
      ctx.globalAlpha = 1;
    }
    ctx.restore();
  }
  function ringsDraw(t) {
    var cx = W / 2, cy = H / 2, far = Math.sqrt(cx * cx + cy * cy);
    var r0 = P.ringR0 * dpr, step = P.ringStep * dpr, n = 0, i, r, circ, m, k, dir, len, head, fade;
    ctx.lineCap = 'butt';
    ctx.lineWidth = P.ringW * dpr;
    ctx.strokeStyle = BLUE;
    for (r = r0; r < far; r += step) {
      ctx.globalAlpha = P.ringA * (1 - 0.45 * r / far);
      ctx.beginPath();
      ctx.arc(cx, cy, r, 0, 2 * Math.PI);
      ctx.stroke();
      n++;
    }
    ctx.globalAlpha = 1;
    ctx.lineWidth = P.arcW * dpr;
    for (r = r0, i = 0; r < far; r += step, i++) {
      circ = 2 * Math.PI * r;
      len = Math.min(P.arcLen * dpr, circ * 0.6) / r;          /* 弧长 → 弧度 */
      m = 1 + Math.floor(circ / (P.arcEvery * dpr));
      dir = i % 2 ? -1 : 1;
      fade = P.arcA * (1 - 0.35 * r / far);
      for (k = 0; k < m; k++) {
        head = i * 2.4 + k * 2 * Math.PI / m + dir * t * P.arcV * dpr / r;
        comet(cx, cy, r, head, len, dir, fade);
      }
    }
    stats.rings = n;
  }

  /* ── 指针与涟漪 ────────────────────────────────────────────────────────
     指针只记位置，注入放在帧里做：两帧之间扫过的那一段整段点亮，快速划过不留缝。
     只有动了才注入 —— 指针停在原地不算，尾巴照常散掉。 */
  var ptr = { x: 0, y: 0, lx: 0, ly: 0, has: false, fresh: false };
  var waves = [];
  function local(e) {
    var r = canvas.getBoundingClientRect();
    return [(e.clientX - r.left) * dpr, (e.clientY - r.top) * dpr];
  }
  function moveTo(x, y) {
    if (!ptr.has) { ptr.lx = x; ptr.ly = y; ptr.has = true; }
    ptr.x = x; ptr.y = y; ptr.fresh = true;
  }
  function tap(x, y) {
    if (waves.length >= P.waveMax) waves.shift();
    waves.push({ x: x, y: y, t: 0 });
  }

  function inject(dt) {
    var i, row, col, cx, cy, c0, c1, r0, r1, dx, dy, v;
    if (ptr.fresh) {
      var x0 = ptr.lx, y0 = ptr.ly, sx = ptr.x - x0, sy = ptr.y - y0, sl = sx * sx + sy * sy;
      /* 划得越快，透镜越大：速度按这一帧走过的距离估 */
      var speed = Math.sqrt(sl) / dpr / Math.max(dt, 0.008);
      var R = P.lensR * dpr * Math.min(P.lensGrow, 1 + speed / 2400), R2 = R * R;
      c0 = Math.max(0, Math.floor((Math.min(x0, ptr.x) - R - ox) / cw));
      c1 = Math.min(cols - 1, Math.ceil((Math.max(x0, ptr.x) + R - ox) / cw));
      r0 = Math.max(0, Math.floor((Math.min(y0, ptr.y) - R - oy) / ch));
      r1 = Math.min(rows - 1, Math.ceil((Math.max(y0, ptr.y) + R - oy) / ch));
      for (row = r0; row <= r1; row++) {
        cy = oy + row * ch + ch / 2;
        for (col = c0; col <= c1; col++) {
          cx = ox + col * cw + cw / 2;
          var u = sl > 0 ? ((cx - x0) * sx + (cy - y0) * sy) / sl : 0;
          if (u < 0) u = 0; else if (u > 1) u = 1;
          dx = cx - (x0 + u * sx); dy = cy - (y0 + u * sy);
          var d2 = dx * dx + dy * dy;
          if (d2 > R2) continue;
          v = P.lensPeak * Math.exp(-d2 / R2 * 3.2);
          i = row * cols + col;
          if (v > act[i]) act[i] = v;
        }
      }
      ptr.lx = ptr.x; ptr.ly = ptr.y; ptr.fresh = false;
    }
    var V = P.waveV * dpr, Wd = P.waveW * dpr, reach = 2.5 * Wd;
    for (var w = waves.length - 1; w >= 0; w--) {
      var wv = waves[w];
      wv.t += dt;
      var A = P.wavePeak * Math.exp(-wv.t / P.waveLife), rad = V * wv.t;
      if (A < 0.03 || rad - reach > diag) { waves.splice(w, 1); continue; }
      var hi = rad + reach;
      c0 = Math.max(0, Math.floor((wv.x - hi - ox) / cw));
      c1 = Math.min(cols - 1, Math.ceil((wv.x + hi - ox) / cw));
      r0 = Math.max(0, Math.floor((wv.y - hi - oy) / ch));
      r1 = Math.min(rows - 1, Math.ceil((wv.y + hi - oy) / ch));
      for (row = r0; row <= r1; row++) {
        cy = oy + row * ch + ch / 2 - wv.y;
        for (col = c0; col <= c1; col++) {
          cx = ox + col * cw + cw / 2 - wv.x;
          var d = Math.sqrt(cx * cx + cy * cy) - rad;
          if (d < -reach || d > reach) continue;
          v = A * Math.exp(-d * d / (Wd * Wd) * 2);
          i = row * cols + col;
          if (v > act[i]) act[i] = v;
          if (v > blu[i]) blu[i] = v;
        }
      }
    }
  }

  /* ── 画 ────────────────────────────────────────────────────────────────── */
  function draw(t, dt) {
    ctx.clearRect(0, 0, W, H);
    ringsDraw(t);
    var pIdle = 1 - Math.exp(-P.flickIdle * dt), hot = P.flickHot * dt;
    var drawn = 0, dots = 0, i = 0, row, col, x, y, a, e, g, alpha, b, gb;
    for (row = 0; row < rows; row++) {
      y = oy + row * ch;
      for (col = 0; col < cols; col++, i++) {
        a = act[i];
        if (a < P.floor) continue;             /* 大多数格子在这一行就出去了 */
        e = a * texture(col, row, t);
        if (e < P.floor) continue;
        if (e > 1) e = 1;
        if (e < P.byteAt) {
          g = NB; dots++;
          alpha = 0.18 + 0.5 * (e / P.byteAt);
        } else {
          if (Math.random() < pIdle + a * hot) chars[i] = (Math.random() * NB) | 0;
          g = chars[i];
          alpha = 0.35 + 0.65 * ((e - P.byteAt) / (1 - P.byteAt));
        }
        x = ox + col * cw;
        ctx.globalAlpha = alpha;
        ctx.drawImage(atlas, (g % ACOLS) * cw, Math.floor(g / ACOLS) * ch, cw, ch, x, y, cw, ch);
        b = blu[i];
        if (b > 0.02) {
          gb = g + NG;
          ctx.globalAlpha = alpha * (b > 0.7 ? 1 : b * 1.4);
          ctx.drawImage(atlas, (gb % ACOLS) * cw, Math.floor(gb / ACOLS) * ch, cw, ch, x, y, cw, ch);
        }
        drawn++;
      }
    }
    ctx.globalAlpha = 1;
    stats.drawn = drawn; stats.dots = dots;
  }

  /* ── 帧循环 ──────────────────────────────────────────────────────────────
     首屏在视口里就一直转：没人碰的时候 idleFps，只画那几圈线；指针刚动过、有波在推、
     或者还有尾巴没散，就跟着 rAF 走。首屏不在视口里、标签页藏起来：停。 */
  var raf = 0, t0 = 0, last = 0, lastDraw = 0, maxAct = 0;
  var inView = true, hidden = !!document.hidden;

  function step(now) {
    var dt = last ? Math.min(0.05, (now - last) / 1000) : 0.016;
    last = now;
    var kA = Math.exp(-dt / P.trail), kB = Math.exp(-dt / P.blueFade), i, mx = 0;
    for (i = 0; i < N; i++) {
      act[i] *= kA; blu[i] *= kB;
      if (act[i] > mx) mx = act[i];
    }
    maxAct = mx;
    inject(dt);
    draw((now - t0) / 1000, dt);
  }
  function loop(now) {
    raf = 0;
    if (!inView || hidden) return;
    var active = ptr.fresh || waves.length > 0 || maxAct >= P.floor || stats.drawn > 0;
    if (active || now - lastDraw >= 1000 / P.idleFps - 1) {
      var t1 = performance.now();
      step(now);
      stats.ms = performance.now() - t1;
      lastDraw = now;
    }
    raf = requestAnimationFrame(loop);
  }
  function wake() {
    if (REDUCED || !started || raf || !inView || hidden) return;
    last = 0;
    raf = requestAnimationFrame(loop);
  }

  /* ── 启动 ────────────────────────────────────────────────────────────────
     reduced 下：光照常，圈画一帧就停，不装监听、不画字。 */
  var started = false;
  function start() {
    if (started) return;
    started = true;
    t0 = performance.now();
    backdropBuild();
    build();
    if (REDUCED) { draw(0, 0); return; }
    hero.addEventListener('pointermove', function (e) {
      var p = local(e);
      moveTo(p[0], p[1]);
      wake();
    }, { passive: true });
    var off = function () { ptr.has = false; ptr.fresh = false; };
    hero.addEventListener('pointerleave', off, { passive: true });
    hero.addEventListener('pointercancel', off, { passive: true });
    hero.addEventListener('pointerdown', function (e) {
      var p = local(e);
      tap(p[0], p[1]);
      wake();
    }, { passive: true });
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (es) {
        inView = es[es.length - 1].isIntersecting;
        wake();
      }, { threshold: 0 }).observe(hero);
    }
    document.addEventListener('visibilitychange', function () {
      hidden = !!document.hidden;
      wake();
    });
    wake();
  }

  window.__field = {
    P: P,
    stats: function () {
      return { cols: cols, rows: rows, cells: N, drawn: stats.drawn, dots: stats.dots,
               bytes: stats.drawn - stats.dots, rings: stats.rings,
               ms: +stats.ms.toFixed(2), waves: waves.length, running: !!raf,
               reduced: REDUCED, started: started };
    },
    move: function (x, y) { moveTo(x * dpr, y * dpr); wake(); },
    leave: function () { ptr.has = false; ptr.fresh = false; },
    tap: function (x, y) { tap(x * dpr, y * dpr); wake(); },
    rebuild: function () { if (!started) return; backdropBuild(); build(); if (REDUCED) draw(0, 0); }
  };

  /* 图集在等宽字体到之前画会拿回退字体的字形，字一到就整片换脸。
     等 fonts.load，最多等 1.5s；万一超时先建，字体到了再把图集重画一遍。 */
  if (document.fonts && document.fonts.load) {
    document.fonts.load(measure().f + 'px ' + MONO, GLYPHS).then(start, start);
    setTimeout(start, 1500);
    if (document.fonts.ready) document.fonts.ready.then(function () { if (started) atlasBuild(); });
  } else start();

  /* ── 尺寸变了 ────────────────────────────────────────────────────────────
     首屏变了尺寸（转屏、缩放、换语言之后高度变了）→ 光和网格整个重来。 */
  var pending = 0;
  function relayout() {
    if (pending || !started) return;
    pending = requestAnimationFrame(function () {
      pending = 0;
      var r = hero.getBoundingClientRect(), d = Math.min(window.devicePixelRatio || 1, 2);
      if (Math.round(r.width * d) === W && Math.round(r.height * d) === H && d === dpr) return;
      backdropBuild();
      build();
      if (REDUCED) draw(0, 0);
    });
  }
  if (window.ResizeObserver) new ResizeObserver(relayout).observe(hero);
  window.addEventListener('resize', relayout);
})();
