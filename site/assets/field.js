/* field.js — 密文场
 *
 * 首屏右侧那片空白在结构上就是 relay 的位置（控制端 → relay → 设备板）。
 * 这里画的不是背景花纹，是过境的密文：一片极稀疏的细胞栅格，
 * 指针经过时被点亮，然后按生命游戏的规则演化几代再衰减掉。
 * 命令过境时（burst）沿传输方向涌一道，让「relay 只看得到密文」这句话有个身体。
 *
 * 实现取向对齐 openai.com/codex 的做法（2026-09-03 扒的真实现）：
 *   单个 2D canvas、零依赖、pointer-events:none、2×DPR、
 *   标记极低不透明度（那边是白底上 alpha 16–80，只填 0.3% 面积）。
 * 不用 WebGL：为一个静态站背 three.js 不划算，全幅 shader 也会变成装饰性铺底。
 *
 * 浅色主题下反过来：白底上的深色标记。
 */
(function () {
  'use strict';

  var host = document.querySelector('.hero');
  if (!host) return;
  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  var cv = document.createElement('canvas');
  cv.id = 'field';
  cv.setAttribute('aria-hidden', 'true');
  host.insertBefore(cv, host.firstChild);
  var ctx = cv.getContext('2d');

  var CELL = 15;          /* 栅格步长 px */
  var MARK = 4;           /* 标记边长 px */
  var STEP = 130;         /* 演化一代的间隔 ms */
  var FADE = 0.055;       /* 每帧衰减 */
  var MAXA = 0.17;        /* 最亮的细胞也只有这个 alpha —— 它是耳语不是元素 */
  var SEED = 0.34;        /* 指针半径内每代播种概率 */
  var RAD  = 62;          /* 指针影响半径 px */

  var cols = 0, rows = 0, dpr = 1;
  var alive = null, age = null, next = null;
  var mx = -1e4, my = -1e4, lastStep = 0, raf = 0;

  function resize() {
    var r = host.getBoundingClientRect();
    if (!r.width || !r.height) return;
    dpr = Math.min(2, window.devicePixelRatio || 1);
    cv.width  = Math.round(r.width  * dpr);
    cv.height = Math.round(r.height * dpr);
    cv.style.width  = r.width + 'px';
    cv.style.height = r.height + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    cols = Math.ceil(r.width  / CELL);
    rows = Math.ceil(r.height / CELL);
    var n = cols * rows;
    alive = new Uint8Array(n);
    age   = new Float32Array(n);
    next  = new Uint8Array(n);
  }

  function neighbours(x, y) {
    var n = 0;
    for (var dy = -1; dy <= 1; dy++) {
      for (var dx = -1; dx <= 1; dx++) {
        if (!dx && !dy) continue;
        var nx = x + dx, ny = y + dy;
        if (nx < 0 || ny < 0 || nx >= cols || ny >= rows) continue;
        n += alive[ny * cols + nx];
      }
    }
    return n;
  }

  /* 生命游戏 B3/S23。稀疏场里它会自己熄灭，正是我们要的：
     指针不动，场就安静下去。 */
  function evolve() {
    for (var y = 0; y < rows; y++) {
      for (var x = 0; x < cols; x++) {
        var i = y * cols + x, n = neighbours(x, y);
        next[i] = alive[i] ? (n === 2 || n === 3 ? 1 : 0) : (n === 3 ? 1 : 0);
      }
    }
    alive.set(next);
  }

  /* 环境播种：指针没动过时场也不该是死白的。极低概率，只够维持一点呼吸。 */
  function ambient() {
    var n = cols * rows;
    var k = Math.max(2, Math.round(n * 0.0042));
    for (var i = 0; i < k; i++) alive[(Math.random() * n) | 0] = 1;
  }

  function seed() {
    if (mx < -1000) return;
    var gx = mx / CELL, gy = my / CELL, gr = RAD / CELL;
    var x0 = Math.max(0, Math.floor(gx - gr)), x1 = Math.min(cols - 1, Math.ceil(gx + gr));
    var y0 = Math.max(0, Math.floor(gy - gr)), y1 = Math.min(rows - 1, Math.ceil(gy + gr));
    for (var y = y0; y <= y1; y++) {
      for (var x = x0; x <= x1; x++) {
        var d = Math.hypot(x - gx, y - gy);
        if (d > gr) continue;
        if (Math.random() < SEED * (1 - d / gr)) { alive[y * cols + x] = 1; }
      }
    }
  }

  /* 命令过境：从左（控制端）向右推一道波，或反过来（输出回流）。 */
  function burst(dir) {
    if (!cols) return;
    var lane = Math.floor(rows * (0.34 + Math.random() * 0.32));
    var t0 = 0;
    var push = function (col) {
      if (col < 0 || col >= cols) return;
      for (var d = -2; d <= 2; d++) {
        var y = lane + d;
        if (y < 0 || y >= rows) continue;
        if (Math.random() < 0.5 - Math.abs(d) * 0.13) alive[y * cols + col] = 1;
      }
    };
    var step = 0, total = cols;
    var iv = setInterval(function () {
      var col = dir > 0 ? step : (cols - 1 - step);
      push(col); push(col + (dir > 0 ? -1 : 1));
      if (++step >= total) clearInterval(iv);
    }, 9);
  }

  function draw() {
    ctx.clearRect(0, 0, cv.width, cv.height);
    ctx.fillStyle = '#111314';
    var off = (CELL - MARK) / 2;
    for (var y = 0; y < rows; y++) {
      for (var x = 0; x < cols; x++) {
        var i = y * cols + x;
        if (alive[i]) age[i] = Math.min(1, age[i] + 0.34);
        else age[i] = Math.max(0, age[i] - FADE);
        if (age[i] <= 0.01) continue;
        ctx.globalAlpha = age[i] * MAXA;
        ctx.fillRect(x * CELL + off, y * CELL + off, MARK, MARK);
      }
    }
    ctx.globalAlpha = 1;
  }

  function frame(now) {
    if (now - lastStep >= STEP) {
      /* 顺序要紧：先演化（已有图形推进一代），再播种。
         反过来的话新播的孤立细胞会被 B3/S23 当场杀掉，一个都活不到绘制。 */
      evolve(); ambient(); seed();
      lastStep = now;
    }
    draw();
    raf = requestAnimationFrame(frame);
  }

  host.addEventListener('pointermove', function (e) {
    var r = host.getBoundingClientRect();
    mx = e.clientX - r.left; my = e.clientY - r.top;
  });
  host.addEventListener('pointerleave', function () { mx = my = -1e4; });

  var ro = window.ResizeObserver ? new ResizeObserver(resize) : null;
  if (ro) ro.observe(host); else window.addEventListener('resize', resize);

  resize();
  if (reduce) {
    /* 降级：一帧静态的稀疏场，流程和观感都还在，只是不动。 */
    for (var k = 0; k < cols * rows; k++) if (Math.random() < 0.012) { alive[k] = 1; age[k] = 1; }
    draw();
  } else {
    for (var q = 0; q < 5; q++) { evolve(); ambient(); }
    raf = requestAnimationFrame(frame);
  }

  window.__field = { burst: burst, stop: function () { cancelAnimationFrame(raf); } };
})();
