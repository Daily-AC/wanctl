#!/usr/bin/env node
/* 首屏密文场（site/assets/field.js）的验收：静止、悬停、点击三种状态各截一张，
 * 桌面 1440×900 和手机 390×844 各一份，顺便把 window.__field.stats() 的数字打出来。
 *
 * 为什么不是 tools/shot.sh：那条路走 --virtual-time-budget，各条动画时钟互相不同步
 * （HANDOFF §4），而这里要看的正是时间里的东西 —— 尾巴会不会散、波推到哪了。
 * 所以按 sweep.mjs 的办法自己起 headless Chrome、走 CDP、按真实毫秒等，
 * 指针事件用 Input.dispatchMouseEvent 打进去（真事件，走的是页面自己的监听）。
 *
 * 用法：site/ 下起 python3 -m http.server <端口>，然后
 *   node tools/field-shot.mjs --base http://127.0.0.1:8688 --out <目录>
 *
 * 除了图，它还查这几件事，任何一件不对就以非零码退出：
 *   · 静止时一个字符都没画（甲方要的：鼠标不动就看不到字符），但圈在画
 *   · 圈真的在动：隔 1.5s 两次读画布，位图必须不同（甲方第四轮：「线圈没动啊」）
 *   · 光的圆心在首屏正中（甲方第四轮：「也不居中」）
 *   · 划过之后有字，移开 2.5s 后字又散干净
 *   · 没有横向溢出（画布铺满首屏不能把页面撑宽）
 *   · 手机屏里「允许一次」那个按钮仍然点得到（画布 pointer-events:none 没被谁盖掉）
 *   · prefers-reduced-motion 下光照常、圈画一帧就停（两次读画布必须相同）、不装监听 */

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync, rmSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const args = process.argv.slice(2);
const flag = (n, d) => { const i = args.indexOf(n); return i >= 0 ? args[i + 1] : d; };
const BASE = flag("--base", "http://127.0.0.1:8688").replace(/\/$/, "");
const OUT = flag("--out", "field-out");
mkdirSync(OUT, { recursive: true });

const CHROME = process.env.CHROME_BIN || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const profile = join(tmpdir(), `wanctl-field-${process.pid}-${Date.now()}`);
const chrome = spawn(CHROME, [
  "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
  "--no-default-browser-check", "--disable-extensions", "--force-device-scale-factor=1",
  `--user-data-dir=${profile}`, "--remote-debugging-port=0", "about:blank",
], { stdio: "ignore" });
process.on("exit", () => { try { chrome.kill(); } catch {} try { rmSync(profile, { recursive: true, force: true }); } catch {} });

async function connect() {
  let port = 0;
  for (let i = 0; i < 50 && !port; i++) {
    try { port = +readFileSync(join(profile, "DevToolsActivePort"), "utf8").split("\n")[0]; } catch { await sleep(200); }
  }
  if (!port) throw new Error("Chrome never wrote DevToolsActivePort");
  let tabs = null;
  for (let i = 0; i < 40 && !tabs; i++) {
    try { const r = await fetch(`http://127.0.0.1:${port}/json`); if (r.ok) tabs = await r.json(); } catch { await sleep(200); }
  }
  const page = tabs.find((t) => t.type === "page");
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pending = new Map();
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
  const send = (method, params = {}) => new Promise((res, rej) => {
    const i = ++id;
    pending.set(i, (m) => m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result));
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  await send("Page.enable"); await send("Runtime.enable");
  return {
    send,
    async js(expr) {
      const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? "eval failed");
      return r.result.value;
    },
    async shot(file) {
      const r = await send("Page.captureScreenshot", { format: "png" });
      writeFileSync(file, Buffer.from(r.data, "base64"));
    },
    async mouse(type, x, y, extra = {}) {
      await send("Input.dispatchMouseEvent", { type, x, y, ...extra });
    },
    close() { ws.close(); },
  };
}

const fails = [];
const check = (ok, what) => { console.log(`  ${ok ? "ok  " : "FAIL"} ${what}`); if (!ok) fails.push(what); };

const cdp = await connect();
for (const vp of [{ w: 1440, h: 900, tag: "desktop" }, { w: 390, h: 844, tag: "mobile" }]) {
  console.log(`\n${vp.tag} ${vp.w}×${vp.h}`);
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: vp.w, height: vp.h, deviceScaleFactor: 1, mobile: vp.w < 560 });
  await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "no-preference" }] });
  await cdp.send("Page.navigate", { url: `${BASE}/?field=${Date.now()}` });
  await sleep(2600);                                   // 字体、开场、环境场都到位

  const s0 = await cdp.js("JSON.stringify(window.__field.stats())");
  console.log("  rest:", s0);
  const st = JSON.parse(s0);
  check(st.started && st.running && st.drawn === 0 && st.rings > 0, `at rest: no characters, ${st.rings} rings drawn`);
  check(st.ms < 4, `idle frame ${st.ms}ms (< 4ms)`);
  const bd = await cdp.js(`(() => { const b = document.querySelector('.tile.hero svg.backdrop'); if (!b) return null;
    const r = b.getBoundingClientRect(); const h = document.querySelector('.tile.hero').getBoundingClientRect();
    const g = b.querySelector('radialGradient');
    return { fits: Math.abs(r.width - h.width) < 1 && Math.abs(r.height - h.height) < 1,
             centred: g && Math.abs(+g.getAttribute('cx') - h.width / 2) < 1 && Math.abs(+g.getAttribute('cy') - h.height / 2) < 1 }; })()`);
  check(bd && bd.fits && bd.centred, `wash fills the hero and is centred: ${JSON.stringify(bd)}`);
  const snap = () => cdp.js("document.getElementById('field').toDataURL('image/png').length + ':' + document.getElementById('field').toDataURL('image/png').slice(-400)");
  const f1 = await snap(); await sleep(1500); const f2 = await snap();
  check(f1 !== f2, "rings move: canvas differs 1.5s apart at rest");
  const over = await cdp.js("document.documentElement.scrollWidth - innerWidth");
  check(over <= 0, `no horizontal overflow (scrollWidth - innerWidth = ${over})`);
  const hit = await cdp.js(`(() => {
    const b = document.querySelector('.ask .yes'); if (!b) return 'no-button';
    const r = b.getBoundingClientRect(); const el = document.elementFromPoint(r.left + r.width/2, r.top + r.height/2);
    return el === b ? 'button' : (el ? el.tagName + '.' + el.className : 'nothing'); })()`);
  check(hit === "button", `phone's Allow button is what the pointer hits (${hit})`);
  await cdp.shot(join(OUT, `${vp.tag}-rest.png`));

  /* 悬停：桌面上沿着笔记本左边的空处划一道 S；手机上沿着手机左边的窄条划下去 */
  const hero = await cdp.js("JSON.stringify(document.querySelector('.tile.hero').getBoundingClientRect())");
  const hr = JSON.parse(hero);
  const pts = [];
  for (let i = 0; i <= 24; i++) {
    const u = i / 24;
    if (vp.w < 560) pts.push([hr.left + hr.width * (0.06 + 0.04 * Math.sin(u * 8)), hr.top + hr.height * (0.45 + 0.45 * u)]);
    else pts.push([hr.left + hr.width * (0.16 + 0.05 * Math.sin(u * 7)), hr.top + hr.height * (0.92 - 0.5 * u)]);
  }
  for (const [x, y] of pts) { await cdp.mouse("mouseMoved", x, y); await sleep(16); }
  await sleep(30);
  const sh = JSON.parse(await cdp.js("JSON.stringify(window.__field.stats())"));
  console.log("  hover:", JSON.stringify(sh));
  check(sh.running && sh.drawn > 0, `hover reveals characters (${sh.drawn} cells, ${sh.ms}ms/frame)`);
  check(sh.ms < 8, `frame ${sh.ms}ms (< 8ms)`);
  await cdp.shot(join(OUT, `${vp.tag}-hover.png`));
  await sleep(700);
  await cdp.shot(join(OUT, `${vp.tag}-hover-fading.png`));

  /* 点击：桌面点在两块屏之间（那正是 relay 站的地方），手机点在手机左边的空处 */
  const cx = vp.w < 560 ? hr.left + hr.width * 0.14 : hr.left + hr.width * 0.5;
  const cy = vp.w < 560 ? hr.top + hr.height * 0.62 : hr.top + hr.height * 0.7;
  await cdp.mouse("mouseMoved", cx, cy);
  await cdp.mouse("mousePressed", cx, cy, { button: "left", clickCount: 1 });
  await cdp.mouse("mouseReleased", cx, cy, { button: "left", clickCount: 1 });
  await sleep(260);
  console.log("  wave:", await cdp.js("JSON.stringify(window.__field.stats())"));
  await cdp.shot(join(OUT, `${vp.tag}-wave-260ms.png`));
  await sleep(500);
  await cdp.shot(join(OUT, `${vp.tag}-wave-760ms.png`));
  await cdp.mouse("mouseMoved", 5, vp.h - 5);         // 移开，尾巴该散
  await sleep(2500);
  const s1 = JSON.parse(await cdp.js("JSON.stringify(window.__field.stats())"));
  check(s1.waves === 0 && s1.drawn === 0, `2.5s after leaving: no characters left (${JSON.stringify(s1)})`);
  await cdp.shot(join(OUT, `${vp.tag}-after.png`));

  /* reduced-motion：底纹照常，不流光、不跑循环 */
  await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "reduce" }] });
  await cdp.send("Page.navigate", { url: `${BASE}/?reduced=${Date.now()}` });
  await sleep(2000);
  const sr = JSON.parse(await cdp.js("JSON.stringify(window.__field.stats())"));
  const g1 = await snap(); await sleep(1200); const g2 = await snap();
  check(sr.reduced && sr.started && !sr.running && sr.drawn === 0 && sr.rings > 0 && g1 === g2,
        `reduced-motion: rings drawn once and still, no loop (${JSON.stringify(sr)})`);
  await cdp.shot(join(OUT, `${vp.tag}-reduced.png`));
}

/* 中文：标题换了字 —— 透镜照常，圈照常 */
await cdp.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "no-preference" }] });
await cdp.send("Page.navigate", { url: `${BASE}/?zh=${Date.now()}` });
await sleep(1500);
await cdp.js("document.querySelector('#lang').click()");
await sleep(600);
/* 在中文标题上来回划：禁区跟着新标题走了的话，标题盒子里一格都不该亮 */
const h1 = JSON.parse(await cdp.js("JSON.stringify(document.querySelector('.tile.hero h1').getBoundingClientRect())"));
for (let i = 0; i <= 20; i++) { await cdp.mouse("mouseMoved", h1.left + h1.width * i / 20, h1.top + h1.height / 2); await sleep(20); }
await sleep(40);
const sz = JSON.parse(await cdp.js("JSON.stringify(window.__field.stats())"));
console.log("\nzh, pointer dragged along the h1:", JSON.stringify(sz));
check(sz.drawn > 0 && sz.rings > 0, `zh: lens lit ${sz.drawn} cells, ${sz.rings} rings drawn`);
await cdp.shot(join(OUT, "desktop-zh-title.png"));

cdp.close();
console.log(`\n${fails.length ? fails.length + " check(s) failed" : "all checks passed"} — shots in ${OUT}/`);
process.exit(fails.length ? 1 : 0);
