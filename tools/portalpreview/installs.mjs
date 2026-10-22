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
const OUT = arg('--out', join(tmpdir(), 'wanctl-install-shots'));
const PORT = Number(arg('--port', '9826'));
const dir = join(tmpdir(), 'wanctl-version-chrome-' + PORT);
mkdirSync(OUT, {recursive:true});
const chrome = spawn(arg('--chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'),
 [`--remote-debugging-port=${PORT}`, `--user-data-dir=${dir}`,
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
 const m=JSON.parse(event.data);
 if(m.method==='Runtime.exceptionThrown') errors.push(m.params.exceptionDetails);
 if(m.method==='Runtime.consoleAPICalled' && m.params.type==='error') errors.push(m.params.args);
});
const device='DESKTOP-RQFV0SH-workstation-long';
const cases=[['behind','install-behind',device],['installed','noask',device],['not-installed','install-empty',device],['beta','install-behind',device],['downloading','install-downloading',device],['installing','install-installing',device],['done','install-done',device],['opencu-done','install-opencu-done',device],['failure','install-failed',device],['missing','install-missing',device],['unreachable','install-unreachable',device],['old-agent','install-old',device],['shared','noask','slate'],['shared-manage','noask','quarry'],['offline','install-offline',device]];
const results=[];
async function until(expr){for(let i=0;i<120;i++){if(await cdp.eval('return '+expr+';'))return;await sleep(50);}throw Error('not ready: '+expr);}
async function click(selector){
 const r=await cdp.eval(`const e=document.querySelector(${JSON.stringify(selector)});const r=e.getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2,hit:e.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2))};`);
 assert.equal(r.hit,true,'click target occluded');
 await cdp.send('Input.dispatchMouseEvent',{type:'mousePressed',x:r.x,y:r.y,button:'left',clickCount:1});
 await cdp.send('Input.dispatchMouseEvent',{type:'mouseReleased',x:r.x,y:r.y,button:'left',clickCount:1});
}
try{
 for(const [width,height] of [[1200,900],[412,915],[390,844]]){
  await cdp.send('Emulation.setDeviceMetricsOverride',{width,height,deviceScaleFactor:1,mobile:width<500});
  await cdp.send('Emulation.setFocusEmulationEnabled',{enabled:true});
  for(const lang of ['en','zh'])for(const [name,scene,target]of cases){
   await cdp.send('Page.navigate',{url:'about:blank'});
   await cdp.send('Page.navigate',{url:`${BASE}/?lang=${lang}&avatar=off&scene=${scene}&slow=100&now=1792555200000#device/${target}`});
   await until("document.querySelectorAll('#grid .dev').length===8 && document.querySelector('#dFp').textContent!==''");
   const shared=name.startsWith('shared');
   if(!shared)await until("!document.querySelector('#dUpgrade').hidden");
   if(['downloading','installing','done','opencu-done','failure','missing','unreachable','old-agent'].includes(name))await until("!document.querySelector('#dInstallProgress').hidden");
   if(name==='not-installed')await until("document.querySelector('#dOpenCUVersion').textContent==='"+(lang==='en'?'not installed':'未安装')+"'");
   if(name==='beta'){
    await click('#dUpgrade');await until("document.querySelector('#form').classList.contains('show')");
    assert.equal(await cdp.eval("return document.querySelector('#formF input').value;"),'v0.30.0');
    await cdp.send('Input.dispatchKeyEvent',{type:'rawKeyDown',key:'a',code:'KeyA',modifiers:4});
    await cdp.send('Input.dispatchKeyEvent',{type:'keyUp',key:'a',code:'KeyA',modifiers:4});
    await cdp.eval("document.querySelector('#formF input').select();");
    await cdp.send('Input.insertText',{text:'v0.31.0-beta.1'});
   }
   if(name==='installed'){
    await click('#dUpgrade');assert.equal(await cdp.eval("return document.querySelector('#formF input').value;"),'');
    await click('#formNo');
   }
   const metrics=await cdp.eval(`const shown=e=>e.getBoundingClientRect().height>0&&getComputedStyle(e).display!=='none';const buttons=['#dUpgrade','#dOpenCUInstall'].map(s=>document.querySelector(s));return {width:innerWidth,scroll:document.documentElement.scrollWidth,buttons:buttons.map(e=>({shown:shown(e),disabled:e.disabled,w:e.getBoundingClientRect().width,h:e.getBoundingClientRect().height})),progress:document.querySelector('#dInstallProgress').textContent,clipped:[...document.querySelectorAll('.device-versions dd,.install-status')].filter(shown).some(e=>e.scrollWidth>e.clientWidth+1)};`);
   assert.equal(metrics.width,width);assert.ok(metrics.scroll<=width,JSON.stringify(metrics));assert.equal(metrics.clipped,false);
   assert.equal(metrics.buttons[0].shown,!shared);assert.equal(metrics.buttons[1].shown,!shared);
   if(width<500)for(const b of metrics.buttons.filter(b=>b.shown)){assert.ok(b.w>=40&&b.h>=40,JSON.stringify(b));}
   if(name==='offline')assert.ok(metrics.buttons.every(b=>b.disabled));
   if(['failure','missing','unreachable','old-agent'].includes(name)) assert.ok(metrics.progress.includes(lang==='zh'?'失败：':'Failed: '),metrics.progress);
   if(name==='old-agent') assert.equal(metrics.progress,lang==='zh'?'wanctl · 失败：这台设备的 wanctl 太旧，不能从网页升级。请在设备上运行 wanctl update；开着自动更新的话，它也会自己更新。':'wanctl · Failed: This device\'s wanctl is too old to be upgraded from the web. Run `wanctl update` on the device; if auto-update is on, it will also update itself.');
   if(name==='opencu-done') assert.equal(metrics.progress,lang==='zh'?'OpenCU · 已完成 0.4.1':'OpenCU · Done 0.4.1');
   if(name==='missing') assert.equal(metrics.progress,lang==='zh'?'wanctl · 失败：找不到 v0.29.0-beta.9 这个版本':'wanctl · Failed: version v0.29.0-beta.9 was not found in the official releases');
   if(name==='unreachable') assert.equal(metrics.progress,lang==='zh'?'wanctl · 失败：设备连不上 github.com':'wanctl · Failed: the device could not reach github.com');
   const shot=await cdp.send('Page.captureScreenshot',{format:'png'});
   writeFileSync(join(OUT,`${name}-${lang}-${width}x${height}.png`),Buffer.from(shot.data,'base64'));
   if(name==='beta'){
    await click('#formYes');await until("document.querySelector('#dInstallProgress').textContent.includes('wanctl')");
    const result=await cdp.eval("return await (await fetch('/api/devices/install?device="+device+"')).json();");
    assert.equal(result.package,'wanctl');assert.equal(result.version,'v0.31.0-beta.1');
   }
   if(name==='not-installed'){
    await click('#dOpenCUInstall');
    await until("document.querySelector('#dInstallProgress').textContent.startsWith('OpenCU · ')");
    const result=await cdp.eval("return await (await fetch('/api/devices/install?device="+device+"')).json();");
    assert.equal(result.package,'opencu');assert.equal(result.version,'latest');
   }
   results.push({name,lang,width,height,...metrics});
  }
 }
 assert.deepEqual(errors,[],'browser errors');
 writeFileSync(join(OUT,'install-checks.json'),JSON.stringify(results,null,2));
 console.log(`PASS: ${results.length} cases, owner/share gates, real clicks, beta request, OpenCU latest request, no overflow, 40px targets.`);
}finally{cdp.ws.close();chrome.kill();await new Promise(r=>chrome.once('exit',r));rmSync(dir,{recursive:true,force:true});}
