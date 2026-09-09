#!/usr/bin/env node
/* 共享给你的那台设备，界面上该少些什么。
 *
 *   tools/portalpreview/serve.sh 8724 &
 *   node tools/portalpreview/shared-device.mjs --base http://127.0.0.1:8724
 *
 * sweep.mjs 量的是「这一屏有没有排坏」，它答不了「这一屏该不该出现」。这支
 * 只问后一个问题，而且只问共享设备这一处 —— 因为这一处的每一条都是权限边界，
 * 排版可以有「可接受」的余地，权限不能。判据全是布尔，退出码不为零就是回归。
 *
 * 钉住的四条（都出过事）：
 *   1. 齿轮不出现。app.js 会给它挂 hidden，可 .chip.icon{display:grid} 的
 *      特指度 (0,2,0) 压得过浏览器默认表里的 [hidden]{display:none} (0,1,0)，
 *      于是齿轮照样画出来 —— 用户点进去看见的是**上一台**设备的设置。
 *      修法是 app.css 顶上那句 [hidden]{display:none!important}。
 *   2. 模式胶囊是死的。审批归设备主人，中继也会 403。
 *   3. 只读横幅在，指纹填上了。两者都来自设备清单，清单没到就都是空的
 *      （issue #48 说的就是这个形状）。
 *   4. #device/<共享设备>/settings 不给出设置屏。openDevice 对共享设备提前
 *      返回、根本没加载设置，那一屏于是留着上一台设备的名字、别名和卡片。
 *
 * 两条进入路径都走一遍：地址栏直接打开（哈希在文档加载时就在 URL 里），
 * 以及从设备清单点进去。前者是 issue #48 唯一能重现的入口。
 */
import { spawn } from 'node:child_process';
import { rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const argv = process.argv.slice(2);
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const BASE = arg('--base', 'http://127.0.0.1:8724');
const PORT = Number(arg('--port', '9805'));
const CHROME = arg('--chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome');
const SHARED = arg('--device', 'slate');       // fixtures.js 里唯一那台共享设备
const OWNED = arg('--owned', 'bench-02');      // 用来先把设置屏灌上别人的数据
const TRIALS = Number(arg('--trials', '8'));
// 把 /api/devices 拖慢，好让「设备页先画完、清单后到」那一类竞态真的有机会发生。
const SLOW = arg('--slow', '350');

const dir = join(tmpdir(), 'portal-shared-chrome-' + PORT);
rmSync(dir, { recursive: true, force: true });
const chrome = spawn(CHROME, ['--headless=new', `--remote-debugging-port=${PORT}`,
  `--user-data-dir=${dir}`, '--no-first-run', '--no-default-browser-check',
  '--disable-extensions', '--hide-scrollbars', '--force-device-scale-factor=1', 'about:blank'],
  { stdio: 'ignore' });
process.on('exit', () => { try { chrome.kill(); } catch {} });
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => { try { chrome.kill(); } catch {} process.exit(1); });

async function connect(port) {
  const t0 = Date.now(); let tabs, host;
  while (Date.now() - t0 < 30000) {
    for (const h of ['127.0.0.1', 'localhost']) {
      try {
        const r = await fetch(`http://${h}:${port}/json`, { signal: AbortSignal.timeout(1500) });
        if (r.ok) { const j = await r.json(); if (j.length) { tabs = j; host = h; } }
      } catch { /* not up */ }
    }
    if (tabs) break;
    await sleep(400);
  }
  if (!tabs) throw new Error(`CDP port ${port} never came up`);
  const page = tabs.find((t) => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl.replace('localhost', host));
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pending = new Map();
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
  const send = (method, params = {}) => new Promise((res, rej) => {
    const i = ++id;
    const timer = setTimeout(() => { pending.delete(i); rej(new Error(method + ': timeout')); }, 30000);
    pending.set(i, (m) => { clearTimeout(timer); m.error ? rej(new Error(method + ': ' + m.error.message)) : res(m.result); });
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  await send('Runtime.enable'); await send('Page.enable');
  return { send, ws, async eval(expr) {
    const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, returnByValue: true, awaitPromise: true });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || 'eval failed');
    return r.result.value;
  } };
}

/* 量的是计算值，不是属性：属性挂上了而 CSS 把它压掉，正是这里出过的那次事故。 */
const READ = `
  const gear = document.querySelector('#dGear');
  const mode = document.querySelector('#dMode');
  const shown = document.querySelector('.view.show');
  return {
    view: shown ? shown.dataset.view : null,
    hash: location.hash,
    gearDisplay: getComputedStyle(gear).display,
    gearWidth: Math.round(gear.getBoundingClientRect().width),
    modeDisabled: !!mode.disabled,
    banner: !document.querySelector('#dShared').hidden,
    fingerprint: (document.querySelector('#dFp').textContent || '').trim(),
    deviceName: (document.querySelector('#dName').textContent || '').trim(),
  };`;

const cdp = await connect(PORT);
const fails = [];
const note = (where, msg) => { fails.push(`${where}: ${msg}`); };

function checkDevicePage(where, r) {
  if (r.gearDisplay !== 'none') note(where, `齿轮可见（display:${r.gearDisplay}，宽 ${r.gearWidth}px），共享设备不该有设备设置入口`);
  if (!r.modeDisabled) note(where, '模式胶囊可交互，审批只归设备主人');
  if (!r.banner) note(where, '只读横幅没出现');
  if (!r.fingerprint) note(where, '指纹是空的（设备清单还没到就把这一屏画出来了）');
}

for (const w of [1200, 390]) {
  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: w, height: 900, deviceScaleFactor: 1, mobile: w <= 768, screenWidth: w, screenHeight: 900 });

  // A. 地址栏直接打开，重复若干次 —— 这是竞态唯一露头的入口（issue #48）。
  for (let i = 0; i < TRIALS; i++) {
    await cdp.send('Page.navigate', { url: 'about:blank' });
    await sleep(100);
    await cdp.send('Page.navigate', { url: `${BASE}/?lang=en&slow=${SLOW}&_d=${w}-${i}#device/${encodeURIComponent(SHARED)}` });
    await sleep(1600 + Number(SLOW));
    checkDevicePage(`${w}px 直接打开 #${i}`, await cdp.eval(READ));
  }

  // B. 先进一台自己的设备的设置屏，把那一屏灌满别人的数据，再走到共享设备，
  //    然后请求它的设置地址 —— 「点齿轮跳到另一台设备的设置」就是这条路。
  await cdp.send('Page.navigate', { url: `${BASE}/?lang=en&slow=${SLOW}&_o=${w}&view=device/${encodeURIComponent(OWNED)}/settings` });
  await sleep(2000 + Number(SLOW));
  const owned = await cdp.eval(READ);
  if (owned.view !== 'devsettings') note(`${w}px 自有设备`, `设置屏没打开（view=${owned.view}），这一步是工装自己的前提`);

  await cdp.eval(`location.hash = '#device/${SHARED}'; return 1`);
  await sleep(1200);
  checkDevicePage(`${w}px 从自有设备走过来`, await cdp.eval(READ));

  await cdp.eval(`location.hash = '#device/${SHARED}/settings'; return 1`);
  await sleep(1200);
  const after = await cdp.eval(READ);
  if (after.view === 'devsettings') {
    note(`${w}px 共享设备的设置地址`, '给出了设备设置屏；共享设备没有这一屏，它上面留着的是上一台设备的名字与别名');
  }
}

const runs = (TRIALS + 2) * 2;
if (fails.length) {
  console.log(`共享设备门禁：${runs} 次检查，${fails.length} 条不合格\n`);
  for (const f of fails) console.log('  ' + f);
  process.exitCode = 1;
} else {
  console.log(`共享设备门禁：${runs} 次检查全部合格`);
  console.log('  齿轮不可见 · 模式胶囊不可交互 · 只读横幅在 · 指纹已填 · 设置地址退回设备页');
}
cdp.ws.close();
chrome.kill();
