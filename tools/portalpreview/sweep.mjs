#!/usr/bin/env node
/* 门户的响应式普查。
 *
 * 甲方一条一条地在手机上撞见布局缺陷，说「不要让我发现一个改一个」。这个脚本
 * 就是那句话的答案：把门户的每一个状态 × 每一个视口 × 两种语言全部走一遍，
 * 在真 Chrome 里量出来，而不是靠眼睛在几个屏幕上抽查。
 *
 *   tools/portalpreview/serve.sh 8724 &          # 先起预览
 *   node tools/portalpreview/sweep.mjs --out /tmp/sweep
 *
 * 量的是六件事，全部是可判真假的数字，没有一条是形容词：
 *   1. 横向溢出   documentElement.scrollWidth > innerWidth
 *   2. 越界元素   getBoundingClientRect().right > innerWidth+1 或 left < -1（只报最深的那一个）
 *   3. 浮层出界   .ov.show 里的 .modal 盒子、以及它的按钮，是否整个在视口内
 *   4. 文本截断   overflow:hidden / text-overflow:ellipsis 且 scrollWidth > clientWidth
 *   5. 手指靶子   手机视口下可点元素小于 40×40
 *   6. 控制台错误 exceptionThrown / console.error
 *
 * 依赖：Node 22 的全局 WebSocket 和一个本机 Chrome。不装任何东西。
 * CDP 的那几个坑（端点双栈、Page.enable 抢跑、输入事件不能 await）来自
 * ~/.agents/skills/visual-acceptance/scripts/cdp-drive.mjs，这里只留用得上的部分。
 */

import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ── 参数 ─────────────────────────────────────────────────────────── */
const argv = process.argv.slice(2);
const arg = (k, d) => {
  const i = argv.indexOf(k);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : d;
};
const BASE = arg('--base', 'http://127.0.0.1:8724');
const OUT = arg('--out', join(tmpdir(), 'portal-sweep'));
const ONLY = arg('--only', '');
const SHOT_WIDTHS = arg('--shots', '412,1200').split(',').map(Number);
const LANGS = arg('--langs', 'en,zh').split(',');
const PORT = Number(arg('--port', '9779'));
const CHROME = arg('--chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome');
// 把工装里的「现在」钉住，好让两次普查的截图逐像素可比（fixtures.js 读 ?now）。
const NOW = arg('--now', '');
// 附加在每个 SPA 地址上的查询串，例如 --query avatar=off：
// 想把某一处变化按住不动、只看别处差异时用它。
const QUERY = arg('--query', '');

const VIEWPORTS = arg('--viewports', '360x780,390x844,412x915,430x932,768x1024,1200x820,1440x900')
  .split(',').map((s) => { const [w, h] = s.split('x').map(Number); return { w, h }; });

const PHONE_MAX = 480;   // 手指靶子只在这个宽度以下量

/* ── 状态表 ───────────────────────────────────────────────────────────
   每一行是门户的一个可达状态。`open` 是进入页面之后要执行的一段脚本 ——
   一律走真正的点击处理器（`.click()` / dispatchEvent），不直接改 DOM：
   工装不许替真代码渲染一遍。 */
const DEV_LONG = 'DESKTOP-RQFV0SH-workstation-long';
const enc = encodeURIComponent;

const STATES = [
  // A. 路由
  { id: 'devices', q: '' },
  { id: 'devices-noask', q: '&scene=noask' },
  { id: 'devices-empty', q: '&scene=empty' },
  { id: 'devices-down', q: '&scene=down' },
  { id: 'dev-asks', q: '&view=device/bench-02' },
  { id: 'dev-asks-long', q: `&view=device/${enc(DEV_LONG)}` },
  { id: 'dev-asks-blank', q: '&view=device/kestrel' },
  { id: 'dev-trust', q: '&view=device/bench-02', open: `document.querySelector('.tab[data-tab="trust"]').click()` },
  { id: 'dev-trust-long', q: `&view=device/${enc(DEV_LONG)}`, open: `document.querySelector('.tab[data-tab="trust"]').click()` },
  { id: 'dev-log', q: '&view=device/bench-02', open: `document.querySelector('.tab[data-tab="log"]').click()` },
  { id: 'dev-shared', q: '&view=device/slate' },
  { id: 'dev-offline', q: '&view=device/orchard' },
  { id: 'dev-bypass', q: '&view=device/kestrel' },
  { id: 'devset', q: '&view=device/bench-02/settings' },
  { id: 'devset-long', q: `&view=device/${enc(DEV_LONG)}/settings` },
  { id: 'settings', q: '&view=settings' },
  { id: 'set-tokens', q: '&view=settings/tokens' },
  { id: 'set-notify', q: '&view=settings/notify' },
  { id: 'set-invites', q: '&view=settings/invites' },
  { id: 'set-friends', q: '&view=settings/friends' },
  { id: 'set-acl', q: '&view=settings/acl' },
  { id: 'set-downloads', q: '&view=settings/downloads', settle: 1400 },
  { id: 'set-downloads-open', q: '&view=settings/downloads', settle: 1400,
    open: `document.querySelector('#dl details').open = true` },
  { id: 'set-audit', q: '&view=settings/audit' },

  // B. 浮层
  { id: 'ask-bypass', q: '&view=device/bench-02',
    open: `(()=>{const s=document.querySelector('#dMode');s.value='bypass';s.dispatchEvent(new Event('change'))})()` },
  { id: 'ask-remove', q: '&view=device/bench-02/settings', open: `document.querySelector('#dsRemove').click()` },
  { id: 'ask-remove-long', q: `&view=device/${enc(DEV_LONG)}/settings`, open: `document.querySelector('#dsRemove').click()` },
  { id: 'ask-revoke-trust', q: '&view=device/bench-02',
    open: `document.querySelector('.tab[data-tab="trust"]').click();await new Promise(r=>setTimeout(r,300));document.querySelector('#trusted .act').click()` },
  { id: 'ask-clear-notify', q: '&view=settings/notify', open: `document.querySelector('#nDelete').click()` },
  { id: 'ask-revoke-invite', q: '&view=settings/invites', open: `document.querySelector('#invites .act').click()` },
  { id: 'ask-remove-friend', q: '&view=settings/friends',
    open: `[...document.querySelectorAll('#friends .act')].filter(b=>b.classList.contains('danger'))[0].click()` },
  { id: 'form-rule', q: '&view=device/bench-02',
    open: `document.querySelector('.tab[data-tab="trust"]').click();await new Promise(r=>setTimeout(r,300));document.querySelector('#rAdd').click()` },
  { id: 'form-token', q: '&view=settings/tokens', open: `document.querySelector('#tAdd').click()` },
  { id: 'form-invite', q: '&view=settings/invites', open: `document.querySelector('#iAdd').click()` },
  { id: 'form-friend', q: '&view=settings/friends', open: `document.querySelector('#fAdd').click()` },
  { id: 'form-share', q: '&view=settings/acl', open: `document.querySelector('#aAdd').click()` },
  { id: 'sheet-changelog', q: '', open: `document.querySelector('#ver').click()`, settle: 1200 },
  { id: 'pair', q: `#pair?device=atlas&fp=SHA256:2lvJifeK%2B%2F%2FVCxE0zMA9YVdw76clBF0bGk6n7zbyoaz%3D&name=kestrel&label=claude-code%20on%20kestrel`, hash: true },

  // C. 一次性凭据与 toast
  { id: 'slip-token', q: '&view=settings/tokens',
    open: `document.querySelector('#tAdd').click();await new Promise(r=>setTimeout(r,350));document.querySelector('#formF [data-k]').value='studio';document.querySelector('#formYes').click()`,
    settle: 900 },
  { id: 'slip-invite', q: '&view=settings/invites',
    open: `document.querySelector('#iAdd').click();await new Promise(r=>setTimeout(r,350));document.querySelector('#formYes').click()`,
    settle: 900 },
  { id: 'toast-bad', q: '&view=device/slate',
    open: `document.querySelector('.tab[data-tab="asks"]').click();window.__t=1;(function(){const s=document.querySelector('#dMode');s.disabled=false;s.value='bypass';s.dispatchEvent(new Event('change'))})()`,
    settle: 600 },

  // 顶栏那枚身份章的两种样子。头像挂在外壳上，所以它其实出现在上面每一个
  // 状态里 —— 这两条是把「图加载出来」和「退回字母」两条路各钉死一个状态，
  // 好让它们也各自被量到每一个视口。
  { id: 'avatar-img', q: '&avatar=on' },
  { id: 'avatar-letter', q: '&avatar=broken' },

  // D. 认证页（Go 渲染，预览里是静态的）
  { id: 'login', page: 'login.html' },
  { id: 'pending', page: 'pending.html' },
  { id: 'enroll', page: 'enroll.html' },
];

/* ── 页内量具 ─────────────────────────────────────────────────────────
   全部在页面里跑，回来的是数字。选择器路径要短到能贴进缺陷表里。 */
/* REF 是**请求的**视口宽度，不是 innerWidth。
   这条区别就是甲方那张截图的全部原因：手机 Chrome 在内容横向溢出时会把
   **布局视口**撑宽到内容宽度（实测 412 请求 → innerWidth 487），而屏幕上
   看得见的仍然是 412。于是 position:fixed 的浮层按 487 居中、按 412 显示，
   看起来就是「被推到右边、右缘切掉」。用 innerWidth 当基准的量具会说
   scrollWidth === innerWidth、一切正常 —— 它量的是已经被撑坏的那个视口。 */
const AUDIT = (REF) => String.raw`(() => {
  const REF = ` + REF + String.raw`;
  const vw = REF, vh = innerHeight;
  const sel = (el) => {
    if (!el || el.nodeType !== 1) return '?';
    if (el.id) return '#' + el.id;
    const t = el.tagName.toLowerCase();
    const c = (el.className && typeof el.className === 'string')
      ? '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.') : '';
    const p = el.parentElement;
    if (!p || p === document.body) return t + c;
    const pid = p.id ? '#' + p.id : p.tagName.toLowerCase() +
      ((p.className && typeof p.className === 'string') ? '.' + p.className.trim().split(/\s+/)[0] : '');
    return pid + ' > ' + t + c;
  };
  const vis = (el, cs, r) =>
    cs.visibility !== 'hidden' && cs.display !== 'none' && cs.opacity !== '0' &&
    (r.width > 0 || r.height > 0);

  const over = [], trunc = [], small = [], unbreak = [], scroll = [];
  let maxRight = 0;
  const all = [...document.querySelectorAll('body *')];
  const overSet = new Set();

  for (const el of all) {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    if (!vis(el, cs, r)) continue;

    if (r.right > maxRight) maxRight = r.right;
    if (r.right > vw + 1 || r.left < -1) { over.push(el); overSet.add(el); }

    // 截断：装得下才算数，装不下且是有意义的内容（名字/指纹/命令/路径）才上报
    const clipped = cs.overflowX === 'hidden' || cs.overflowX === 'clip' || cs.textOverflow === 'ellipsis';
    if (clipped && el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0) {
      const txt = (el.textContent || '').trim();
      trunc.push({ sel: sel(el), by: el.scrollWidth - el.clientWidth, text: txt.slice(0, 70),
                   ellipsis: cs.textOverflow === 'ellipsis' });
    }

    // 自己能横向滚的容器：页面不溢出，但读者看不到全部内容。命令、路径、
    // 指纹被藏在右边和被省略号切掉是同一件事，只是形状不同。
    if ((cs.overflowX === 'auto' || cs.overflowX === 'scroll') && el.scrollWidth > el.clientWidth + 1) {
      scroll.push({ sel: sel(el), by: el.scrollWidth - el.clientWidth,
                    text: (el.textContent || '').trim().slice(0, 70) });
    }

    // 断不开的长串顶破容器：自己是文本叶子、比父窄的空间还宽
    if (!el.children.length) {
      const txt = (el.textContent || '').trim();
      if (/^[^\s]{28,}$/.test(txt) && el.scrollWidth > el.clientWidth + 1) {
        unbreak.push({ sel: sel(el), text: txt.slice(0, 60), w: Math.round(el.scrollWidth) });
      }
    }

    // 手指靶子。量的是**真正接受点击的那个盒子**：一个 13px 的复选框包在
    // <label> 里时，指头点的是那张标签，量那个 13 才是量错了对象。
    if (vw <= ` + PHONE_MAX + String.raw`) {
      const tappable = el.matches('button,a[href],select,input,textarea,[role="button"]');
      if (tappable && !el.disabled) {
        const lab = el.closest('label');
        const hit = lab ? lab.getBoundingClientRect() : r;
        if (hit.width < 40 || hit.height < 40) {
          small.push({ sel: sel(el), w: Math.round(hit.width), h: Math.round(hit.height),
                       text: (el.textContent || el.getAttribute('aria-label') || '').trim().slice(0, 30) });
        }
      }
    }
  }

  // 越界只报最深的那一个：祖先跟着越界是同一个缺陷的回声
  const overLeaf = over.filter((el) => ![...overSet].some((o) => o !== el && el.contains(o)))
    .map((el) => {
      const r = el.getBoundingClientRect();
      return { sel: sel(el), left: Math.round(r.left), right: Math.round(r.right),
               w: Math.round(r.width), text: (el.textContent || '').trim().slice(0, 60) };
    });

  // 浮层
  let modal = null;
  const ov = [...document.querySelectorAll('.ov')].find((o) => o.classList.contains('show'));
  if (ov) {
    const m = ov.querySelector('.modal');
    const r = m.getBoundingClientRect();
    const btns = [...m.querySelectorAll('.btns .btn')].map((b) => {
      const q = b.getBoundingClientRect();
      return { text: b.textContent.trim().slice(0, 20), left: Math.round(q.left), right: Math.round(q.right),
               top: Math.round(q.top), bottom: Math.round(q.bottom),
               inside: q.left >= -1 && q.right <= vw + 1 && q.top >= -1 && q.bottom <= vh + 1 };
    });
    const ovr = ov.getBoundingClientRect();
    modal = {
      id: ov.id,
      ovWidth: Math.round(ovr.width), ovLeft: Math.round(ovr.left),
      left: Math.round(r.left), right: Math.round(r.right),
      top: Math.round(r.top), bottom: Math.round(r.bottom), w: Math.round(r.width), h: Math.round(r.height),
      inside: r.left >= -1 && r.right <= vw + 1 && r.top >= -1 && r.bottom <= vh + 1,
      // 居中判据：左右余量差不超过 2px
      centered: Math.abs(r.left - (vw - r.right)) <= 2,
      btns,
    };
  }

  return {
    vw, vh, innerWidth,
    scrollWidth: document.documentElement.scrollWidth,
    // 三个信号取最大，绝不少报：最右的那条墨迹、文档的滚动宽度、以及被撑宽的
    // 布局视口。前一个是纯几何、稳定；后两个受 Chrome 移动端模拟的影响，
    // 同一个页面排在不同状态后面跑，答案会变。
    overflowX: Math.max(0, Math.round(maxRight) - REF,
                        document.documentElement.scrollWidth - REF, innerWidth - REF),
    maxRight: Math.round(maxRight),
    over: overLeaf, trunc, small, unbreak, scroll, modal,
  };
})()`;

/* 这一屏画完了吗。判据是「地址要求的那个视图正在显示」，设备页还要多问一句
   指纹填上了没有 —— 它是 route() 里最后落下的那一笔。 */
const READY = String.raw`return (() => {
  const g = document.querySelector('#grid');
  if (!g) return false;
  const listed = g.children.length || document.querySelector('#onboard').children.length || g.querySelector('.blank');
  if (!listed) return false;
  const shown = document.querySelector('.view.show');
  if (!shown) return false;
  const h = location.hash;
  const want = h.indexOf('#device/') === 0 ? (/\/settings$/.test(h) ? 'devsettings' : 'device')
             : h.indexOf('#settings') === 0 ? 'settings' : 'devices';
  if (shown.dataset.view !== want) return false;
  if (want === 'device') return !!document.querySelector('#dFp').textContent;
  if (want === 'devsettings') return !!document.querySelector('#dsName').textContent.replace('—','');
  return true;
})()`;

/* 冻结活的东西，好让截图可比：秒表、相对时间、以及正在滑出的 toast。 */
const FREEZE = String.raw`(() => {
  document.querySelectorAll('.ask .wait[data-since]').forEach((el) => {
    el.removeAttribute('data-since'); el.textContent = '0:12';
  });
  return true;
})()`;

/* ── CDP ──────────────────────────────────────────────────────────── */
async function waitForEndpoint(port, timeoutMs = 30000) {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    for (const host of ['127.0.0.1', 'localhost']) {
      try {
        const r = await fetch(`http://${host}:${port}/json`, { signal: AbortSignal.timeout(1500) });
        if (r.ok) { const tabs = await r.json(); if (tabs.length) return { host, tabs }; }
      } catch { /* not up */ }
    }
    await sleep(400);
  }
  throw new Error(`CDP port ${port} never came up`);
}

async function connect(port) {
  const { host, tabs } = await waitForEndpoint(port);
  const page = tabs.find((t) => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl.replace('localhost', host));
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const pending = new Map();
  let errors = [];
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
    else if (m.method === 'Runtime.exceptionThrown') {
      errors.push(m.params.exceptionDetails?.exception?.description || m.params.exceptionDetails?.text || 'exception');
    } else if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
      errors.push(m.params.args.map((a) => a.value ?? a.description ?? '').join(' ').slice(0, 200));
    }
  };
  const send = (method, params = {}, timeoutMs = 20000) => new Promise((res, rej) => {
    const i = ++id;
    const timer = setTimeout(() => { pending.delete(i); rej(new Error(method + ': timeout')); }, timeoutMs);
    pending.set(i, (m) => { clearTimeout(timer); m.error ? rej(new Error(method + ': ' + m.error.message)) : res(m.result); });
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  await send('Runtime.enable');
  await send('Page.enable');
  return {
    send,
    takeErrors() { const e = errors; errors = []; return e; },
    async eval(expr) {
      const r = await send('Runtime.evaluate', {
        expression: `(async()=>{${expr.trimStart().startsWith('(') ? 'return ' + expr : expr}})()`,
        returnByValue: true, awaitPromise: true,
      });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || 'eval failed');
      return r.result.value;
    },
    async shot(file) {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      writeFileSync(file, Buffer.from(r.data, 'base64'));
    },
    close() { ws.close(); },
  };
}

/* ── 主循环 ───────────────────────────────────────────────────────── */
const chromeDir = join(tmpdir(), 'portal-sweep-chrome-' + PORT);
rmSync(chromeDir, { recursive: true, force: true });
const chrome = spawn(CHROME, [
  '--headless=new', `--remote-debugging-port=${PORT}`, `--user-data-dir=${chromeDir}`,
  '--no-first-run', '--no-default-browser-check', '--disable-extensions',
  '--hide-scrollbars', '--force-device-scale-factor=1', 'about:blank',
], { stdio: 'ignore' });

process.on('exit', () => { try { chrome.kill(); } catch {} });
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => { try { chrome.kill(); } catch {} process.exit(1); });

mkdirSync(OUT, { recursive: true });
mkdirSync(join(OUT, 'shots'), { recursive: true });

const cdp = await connect(PORT);
const rows = [];
const states = ONLY ? STATES.filter((s) => ONLY.split(',').includes(s.id)) : STATES;

let n = 0;
for (const vp of VIEWPORTS) {
  const mobile = vp.w <= 768;
  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: vp.w, height: vp.h, deviceScaleFactor: 1, mobile,
    screenWidth: vp.w, screenHeight: vp.h,
  });
  // maxTouchPoints 必须在 1..16 —— 传 0 会让这一条整个报错，而报错发生在
  // 第一个宽屏视口上，正好是普查刚跑完手机、开始跑桌面的那一刻。
  await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: mobile, maxTouchPoints: mobile ? 5 : 1 });

  for (const lang of LANGS) {
    for (const st of states) {
      const url = st.page
        ? `${BASE}/${st.page}`
        // `_s` 只为让每次导航的 URL 都不同 —— 同一个地址的 Page.navigate 有时
        // 只是复位滚动，不重新执行脚本，于是上一个状态的浮层会留在页面上。
        : st.hash ? `${BASE}/?lang=${lang}&_s=${st.id}${NOW ? '&now=' + NOW : ''}${QUERY ? '&' + QUERY : ''}`
                  : `${BASE}/?lang=${lang}&_s=${st.id}${NOW ? '&now=' + NOW : ''}${QUERY ? '&' + QUERY : ''}${st.q || ''}`;

      // 45s 而不是默认的 20s：机器上同时跑着别的浏览器时，一次导航偶尔会超时。
      // 超时了就记一行接着往下跑 —— 一次瞬时失败不该把整轮普查烧掉。
      let navErr = null;
      try { await cdp.send('Page.navigate', { url }, 45000); }
      catch (e) { navErr = String(e).slice(0, 120); }
      await sleep(500);
      if (st.page && lang === 'zh') {
        // 三张认证页由 auth.js 自己读 localStorage，导航后切一次即可
        await cdp.eval(`try{localStorage.setItem('wanctl.lang','zh')}catch(e){}; location.reload(); return 1`);
        await sleep(600);
      }
      if (!st.page) {
        // 等到**这个状态那一屏真的画出来**，不是等「有数据了」。
        // 以前的判据是 #grid 里有卡片 —— 而那只说明 loadDevices 回来了。
        // route() 等的是 Promise.all([loadDevices(), whoami])，所以设备页
        // 有可能还没渲染，#dFp 还是空的。空指纹不撑宽页面，于是同一个缺陷
        // 在 dev-asks 上量到、在 ask-bypass 上量不到 —— 一台时灵时不灵的
        // 量具比没有量具更坏。
        for (let i = 0; i < 30; i++) {
          const ready = await cdp.eval(READY).catch(() => false);
          if (ready) break;
          await sleep(200);
        }
        if (st.hash) {
          await cdp.eval(`location.hash = ${JSON.stringify(st.q)}; return 1`);
          await sleep(400);
        }
      }
      await sleep(st.settle ?? 350);
      if (st.open) { await cdp.eval(st.open).catch(() => {}); await sleep(st.settle ?? 450); }
      await cdp.eval(FREEZE).catch(() => {});


      let audit;
      try { audit = await cdp.eval(AUDIT(vp.w)); }
      catch (e) { audit = { error: String(e).slice(0, 200) }; }
      const errors = cdp.takeErrors().filter((e) => !/favicon|ERR_/.test(e));

      const row = { state: st.id, w: vp.w, h: vp.h, lang, ...audit, errors };
      if (navErr) row.navErr = navErr;
      rows.push(row);
      n++;
      // 每一行都落盘。以前只在最后写一次，于是任何一次崩溃都把整轮的量测扔掉。
      writeFileSync(join(OUT, 'sweep.json'), JSON.stringify(rows, null, 1));

      if (SHOT_WIDTHS.includes(vp.w)) {
        const f = join(OUT, 'shots', `${st.id}-${vp.w}-${lang}.png`);
        await cdp.shot(f).catch(() => {});
      }
      process.stdout.write(`\r${n} runs · ${st.id} ${vp.w}×${vp.h} ${lang}            `);
    }
  }
}
process.stdout.write('\n');

writeFileSync(join(OUT, 'sweep.json'), JSON.stringify(rows, null, 1));

/* ── 汇总 ─────────────────────────────────────────────────────────── */
const bad = rows.filter((r) =>
  (r.overflowX > 0) ||
  (r.modal && (!r.modal.inside || !r.modal.centered || r.modal.btns.some((b) => !b.inside))) ||
  (r.trunc && r.trunc.length) || (r.unbreak && r.unbreak.length) || (r.scroll && r.scroll.length) ||
  (r.small && r.small.length) || (r.errors && r.errors.length) || r.error);

const lines = [];
lines.push(`runs: ${rows.length}   flagged: ${bad.length}`);
const kinds = {
  overflow: rows.filter((r) => r.overflowX > 0).length,
  modalOut: rows.filter((r) => r.modal && !r.modal.inside).length,
  modalOff: rows.filter((r) => r.modal && !r.modal.centered).length,
  btnOut: rows.filter((r) => r.modal && r.modal.btns.some((b) => !b.inside)).length,
  truncated: rows.filter((r) => r.trunc.length).length,
  sideScroll: rows.filter((r) => r.scroll.length).length,
  unbreakable: rows.filter((r) => r.unbreak.length).length,
  tinyTaps: rows.filter((r) => r.small.length).length,
  consoleErr: rows.filter((r) => r.errors.length).length,
};
for (const [k, v] of Object.entries(kinds)) lines.push(`${k.padEnd(12)} ${v}`);
const summary = lines.join('\n');
writeFileSync(join(OUT, 'summary.txt'), summary + '\n');
console.log(summary);

cdp.close();
chrome.kill();
