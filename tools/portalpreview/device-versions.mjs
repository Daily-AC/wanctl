#!/usr/bin/env node
// Run against tools/portalpreview/serve.sh. No npm dependencies.
// Checks the real SPA with the device-list endpoint shape, in both languages.
import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import assert from 'node:assert/strict';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const argv = process.argv.slice(2);
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i+1] : d; };
const BASE = arg('--base', 'http://127.0.0.1:8724');
const OUT = arg('--out', join(tmpdir(), 'wanctl-version-shots'));
const PORT = Number(arg('--port', '9819'));
const dir = join(tmpdir(), 'wanctl-version-chrome-' + PORT);
mkdirSync(OUT, {recursive:true});
const chrome = spawn(arg('--chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'),
 ['--headless=new', `--remote-debugging-port=${PORT}`, `--user-data-dir=${dir}`,
 '--no-first-run', '--no-default-browser-check', '--disable-extensions',
 '--disable-features=CalculateNativeWinOcclusion', '--disable-backgrounding-occluded-windows',
 '--disable-renderer-backgrounding', '--disable-background-timer-throttling', 'about:blank'], {stdio:'ignore'});
process.on('exit', () => { try { chrome.kill(); } catch {} });
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


const cdp = await connect(PORT);
const errors = [];
cdp.ws.addEventListener('message', event => {
 const m = JSON.parse(event.data);
 if (m.method === 'Runtime.exceptionThrown') errors.push(m.params.exceptionDetails);
 if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') errors.push(m.params.args);
});
let checks = 0;
const text = id => cdp.eval(`return document.getElementById(${JSON.stringify(id)}).textContent;`);
async function navigate(lang, device = '') {
 await cdp.send('Page.navigate', {url:'about:blank'});
 await cdp.send('Page.navigate', {url: `${BASE}/?lang=${lang}&avatar=off&scene=noask&slow=100&trial=${checks}-${Date.now()}${device ? '#device/'+encodeURIComponent(device) : ''}`});
 for (let i=0; i<100; i++) {
  const ready = await cdp.eval(`return document.querySelectorAll('#grid .dev').length === 8 && ${device ? "document.querySelector('#dWanctlVersion').textContent !== '' && document.querySelector('#dFp').textContent !== ''" : 'true'};`);
  if (ready) return;
  await sleep(50);
 }
 throw Error('device list did not render');
}
try {
 for (const width of [1200,390]) {
  await cdp.send('Emulation.setDeviceMetricsOverride', {width,height:900,deviceScaleFactor:1,mobile:width<500});
  for (const lang of ['en','zh']) {
   const unknown = lang==='en' ? 'unknown' : '未知';
   const latest = lang==='en' ? 'latest' : '最新';
   const update = lang==='en' ? 'update available: v0.30.0' : '可升级到 v0.30.0';
   await navigate(lang);
   const cards = await cdp.eval(`return [...document.querySelectorAll('#grid .dev')].map(e=>({id:e.dataset.dev,text:e.textContent}));`);
   assert.equal(cards.filter(c=>c.text.includes(update)).length,2);
   assert.deepEqual(cards.filter(c=>c.text.includes(update)).map(c=>c.id), ['bench-02','slate']);
   assert.equal(cards.filter(c=>/v0\.(29|31)|0\.3\.99/.test(c.text)).length,0);
   const shape = await cdp.eval(`return (await (await fetch('/api/devices')).json());`);
   assert.equal(shape.latest_wanctl_version, 'v0.30.0');
   for (const d of shape.devices) {
    const keys=['name','alias','fingerprint','online','last_seen','owner','shared','display_name','legacy_name','wanctl_version','opencu_version','versions_reported_at','wanctl_status'];
    if(d.shared) keys.push('manage','perms');
    assert.deepEqual(Object.keys(d).sort(),keys.sort());
   }
   let shot = await cdp.send('Page.captureScreenshot', {format:'png'});
   writeFileSync(join(OUT,`list-${lang}-${width}.png`), Buffer.from(shot.data,'base64'));
   for(const [device,wanctl,opencu] of [
    ['bench-02','v0.29.0 · '+update,unknown],
    ['DESKTOP-RQFV0SH-workstation-long','v0.30.0 · '+latest,'0.3.99'],
    ['atlas','v0.31.0-beta.1',unknown],
    ['mill-01',unknown,unknown],
    ['slate','v0.29.0 · '+update,'0.3.99'],
    ['quarry','v0.30.0 · '+latest,unknown],
   ]) {
    await navigate(lang,device);
    assert.equal(await text('dWanctlVersion'),wanctl,device);
    assert.equal(await text('dOpenCUVersion'),opencu,device);
    assert.equal(await cdp.eval('return document.documentElement.scrollWidth > innerWidth;'),false);
    const visible=await cdp.eval(`const e=document.querySelector('.device-versions');return e.getBoundingClientRect().height>0 && getComputedStyle(e).display!=='none';`);
    assert.equal(visible,true);
    if (device==='slate') {
     shot = await cdp.send('Page.captureScreenshot',{format:'png'});
     writeFileSync(join(OUT,`shared-${lang}-${width}.png`),Buffer.from(shot.data,'base64'));
     await cdp.eval(`document.querySelector('#lang').click();`);
     assert.equal(await text('dWanctlVersion'),'v0.29.0 · '+(lang==='en'?'可升级到 v0.30.0':'update available: v0.30.0'));
    }
    checks++;
   }
  }
 }
 assert.deepEqual(errors, [], 'browser errors');
 console.log(`PASS: ${checks} detail cases; both languages, desktop/mobile, read-only/managed shares, delayed list, language switch, endpoint shape and list markers.`);
} finally {
 cdp.ws.close(); chrome.kill();
 await new Promise(resolve=>chrome.once('exit',resolve));
 rmSync(dir,{recursive:true,force:true});
}
