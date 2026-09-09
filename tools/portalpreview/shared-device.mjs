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
const SHARED = arg('--device', 'slate');       // 共享给你、但没给管理权的那台
const MANAGED = arg('--managed', 'quarry');    // 共享给你、授权带着 manage 的那台
const OWNER = arg('--owner', 'rowan');         // 两台共享设备的主人，用来比对横幅
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
  const vis = (el) => !!el && getComputedStyle(el).display !== 'none' && el.getBoundingClientRect().width > 0;
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
    bannerText: (document.querySelector('#dShared').textContent || '').trim(),
    fingerprint: (document.querySelector('#dFp').textContent || '').trim(),
    deviceName: (document.querySelector('#dName').textContent || '').trim(),
    // 审批与配对上那几枚真按钮。共享只读时它们该换成一句灰字，
    // 拿到管理权时它们该真的在。
    approveButtons: document.querySelectorAll('#dAsks .ask .acts .btn').length,
    roNotice: document.querySelectorAll('#dAsks .ask .acts .dim').length,
    // 属主的东西：通知卡、飞书卡、解绑。拿到管理权也够不着。
    notifyCard: vis(document.querySelector('#dsNotify')),
    larkCard: vis(document.querySelector('#dsLark')),
    removeButton: vis(document.querySelector('#dsRemove')),
    // 三个页签。活动跟着共享走，另外两个跟着 manage 走。
    tabAsks: vis(document.querySelector('.tab[data-tab="asks"]')),
    tabTrust: vis(document.querySelector('.tab[data-tab="trust"]')),
    tabLog: vis(document.querySelector('.tab[data-tab="log"]')),
    tabOn: (document.querySelector('.tab.on') || {}).dataset ? document.querySelector('.tab.on').dataset.tab : null,
    logRows: document.querySelectorAll('#devlog tr').length,
    logEmpty: !!document.querySelector('#devlog .tempty'),
  };`;

const cdp = await connect(PORT);
const fails = [];
const note = (where, msg) => { fails.push(`${where}: ${msg}`); };

/* 共享但没给管理权：今天这一屏一个字都不该变。 */
function checkReadOnly(where, r) {
  if (r.gearDisplay !== 'none') note(where, `齿轮可见（display:${r.gearDisplay}，宽 ${r.gearWidth}px），共享设备不该有设备设置入口`);
  if (!r.modeDisabled) note(where, '模式胶囊可交互，而这份授权没给管理权');
  if (!r.banner) note(where, '横幅没出现，看不出这是谁的机器');
  if (!r.fingerprint) note(where, '指纹是空的（设备清单还没到就把这一屏画出来了）');
  if (r.approveButtons) note(where, `待审批上有 ${r.approveButtons} 枚可点的按钮，而这份授权没给管理权`);
  // 活动是使用权的一部分：CLI 上的 wanctl logs 每个被授权方本来就有。
  if (!r.tabLog) note(where, '活动页签不见了，而看这台机器做过什么是使用权的一部分');
  if (r.tabAsks) note(where, '待审批页签还摆着，可它整页都点不动');
  if (r.tabTrust) note(where, '信任与规则页签还摆着，可它整页都点不动');
  if (r.tabOn !== 'log') note(where, `第一眼停在 ${r.tabOn}，只有使用权时唯一有内容的是活动`);
  if (!r.logRows || r.logEmpty) note(where, '活动是空的 —— 页签开了但日志没拉到（后端把它当管理权拦了？）');
  checkOwnerOnlyHidden(where, r);
}

/* 共享而且给了管理权：控制面整屏都在，属主的东西一样都不在。 */
function checkManaged(where, r) {
  if (r.gearDisplay !== 'none') note(where, `齿轮可见（display:${r.gearDisplay}），齿轮后面整页都是属主的东西，manage 够不着`);
  if (r.modeDisabled) note(where, '模式胶囊是死的，而这份授权给了管理权');
  if (!r.banner) note(where, '横幅没出现 —— 这一屏和自己的设备长得一样，不说就不知道在管谁的机器');
  if (r.banner && !r.bannerText.includes(OWNER)) note(where, `横幅没点出设备主人（"${r.bannerText.slice(0, 40)}…"）`);
  if (!r.fingerprint) note(where, '指纹是空的');
  if (!r.approveButtons) note(where, '待审批上没有可点的按钮，而这份授权给了管理权');
  if (r.roNotice) note(where, '待审批上还留着「只有设备主人能回答」那句灰字');
  if (!r.tabAsks || !r.tabTrust || !r.tabLog) note(where, '三个页签没齐，拿到管理权的这一屏和自己的设备应该一样');
  checkOwnerOnlyHidden(where, r);
}

/* 两种共享设备都适用：属主的东西一律不出现。 */
function checkOwnerOnlyHidden(where, r) {
  if (r.notifyCard) note(where, '通知卡可见 —— 那是设备主人的联系方式，不是这台机器的状态');
  if (r.larkCard) note(where, '飞书卡可见 —— 同上，属于设备主人');
  if (r.removeButton) note(where, '「解除设备」可见，解绑只归设备主人');
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
    checkReadOnly(`${w}px 直接打开 #${i}`, await cdp.eval(READ));
  }

  // B. 先进一台自己的设备的设置屏，把那一屏灌满别人的数据，再走到共享设备，
  //    然后请求它的设置地址 —— 「点齿轮跳到另一台设备的设置」就是这条路。
  await cdp.send('Page.navigate', { url: `${BASE}/?lang=en&slow=${SLOW}&_o=${w}&view=device/${encodeURIComponent(OWNED)}/settings` });
  await sleep(2000 + Number(SLOW));
  const owned = await cdp.eval(READ);
  if (owned.view !== 'devsettings') note(`${w}px 自有设备`, `设置屏没打开（view=${owned.view}），这一步是工装自己的前提`);

  await cdp.eval(`location.hash = '#device/${SHARED}'; return 1`);
  await sleep(1200);
  checkReadOnly(`${w}px 从自有设备走过来`, await cdp.eval(READ));

  await cdp.eval(`location.hash = '#device/${SHARED}/settings'; return 1`);
  await sleep(1200);
  const after = await cdp.eval(READ);
  if (after.view === 'devsettings') {
    note(`${w}px 共享设备的设置地址`, '给出了设备设置屏；共享设备没有这一屏，它上面留着的是上一台设备的名字与别名');
  }
}

// 能管的那台，两条进入路径都走一遍。
for (const w of [1200, 390]) {
  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: w, height: 900, deviceScaleFactor: 1, mobile: w <= 768, screenWidth: w, screenHeight: 900 });

  for (let i = 0; i < TRIALS; i++) {
    await cdp.send('Page.navigate', { url: 'about:blank' });
    await sleep(100);
    await cdp.send('Page.navigate', { url: `${BASE}/?lang=en&slow=${SLOW}&_m=${w}-${i}#device/${encodeURIComponent(MANAGED)}` });
    await sleep(1600 + Number(SLOW));
    checkManaged(`${w}px 能管的共享设备，直接打开 #${i}`, await cdp.eval(READ));
  }

  // 从只读那台切到能管那台：同一份 DOM 换一台设备，门禁要跟着换。
  await cdp.send('Page.navigate', { url: `${BASE}/?lang=en&slow=${SLOW}&_s2=${w}&view=device/${encodeURIComponent(SHARED)}` });
  await sleep(1800 + Number(SLOW));
  checkReadOnly(`${w}px 只读那台（切换前）`, await cdp.eval(READ));
  await cdp.eval(`location.hash = '#device/${MANAGED}'; return 1`);
  await sleep(1400);
  checkManaged(`${w}px 从只读那台切过来`, await cdp.eval(READ));
  await cdp.eval(`location.hash = '#device/${SHARED}'; return 1`);
  await sleep(1400);
  checkReadOnly(`${w}px 又切回只读那台`, await cdp.eval(READ));
}

const runs = (TRIALS + 2) * 2 + (TRIALS + 3) * 2;
if (fails.length) {
  console.log(`共享设备门禁：${runs} 次检查，${fails.length} 条不合格\n`);
  for (const f of fails) console.log('  ' + f);
  process.exitCode = 1;
} else {
  console.log(`共享设备门禁：${runs} 次检查全部合格`);
  console.log('  只读共享：齿轮不可见 · 模式胶囊不可交互 · 横幅在 · 指纹已填 · 无审批按钮 · 只剩活动一页且有内容 · 设置地址退回设备页');
  console.log('  可管共享：控制面在（审批按钮、模式胶囊）· 横幅点出设备主人 · 通知/飞书/解绑一律不可见');
}
cdp.ws.close();
chrome.kill();
