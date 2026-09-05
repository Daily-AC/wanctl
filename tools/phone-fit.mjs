#!/usr/bin/env node
/* 首屏手机屏里那份审批装不装得下：按视口宽度量「拒绝」底边到 home 条顶的距离。
 *
 * 为什么要量：屏内字号是 vw 的 clamp，手机宽度也是 vw 的 clamp，两边的地板不在同一个视口宽度
 * 生效 —— 761~1119 这一档手机停在 150px 的地板上，字号却要到 1024 以下才全部落地，
 * 内容是定高的而屏不是。124px 地板那版（真机之前的 CSS 壳）在 768 下连「拒绝」都看不见。
 * 装得下的意思：「拒绝」的底边没有越过 .ask 的内容盒（下内边距是 home 条的安全区），
 * .ask 自己也没有溢出屏。spare 是「拒绝」底边到内容盒底边的距离，负数就是被顶出去了。
 * 顺带查蓝按钮在不在折线以上。
 *
 * 量之前等 .rise 动画（.42s）走完 —— 半路上量到的是升起来一半的位置，第一版就被这个骗过。
 *
 * 用法：site/ 下起 python3 -m http.server <端口>，然后
 *   node tools/phone-fit.mjs --base http://127.0.0.1:8688
 * 任何一档装不下就以非零码退出。改屏内字号、.ask 内边距或 .phone 的宽度之后跑一遍。 */

import { spawn } from "node:child_process";
import { rmSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const profile = join(tmpdir(), `wanctl-fit-${process.pid}`);
const chrome = spawn(CHROME, ["--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
  "--disable-extensions", `--user-data-dir=${profile}`, "--remote-debugging-port=0", "about:blank"], { stdio: "ignore" });
process.on("exit", () => { try { chrome.kill(); } catch {} try { rmSync(profile, { recursive: true, force: true }); } catch {} });
let port = 0; for (let i = 0; i < 50 && !port; i++) { try { port = +readFileSync(join(profile, "DevToolsActivePort"), "utf8").split("\n")[0]; } catch { await sleep(200); } }
let tabs = null; for (let i = 0; i < 40 && !tabs; i++) { try { const r = await fetch(`http://127.0.0.1:${port}/json`); if (r.ok) tabs = await r.json(); } catch { await sleep(200); } }
const ws = new WebSocket(tabs.find((t) => t.type === "page").webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map();
ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id;
  pending.set(i, (m) => m.error ? rej(new Error(m.error.message)) : res(m.result)); ws.send(JSON.stringify({ id: i, method, params })); });
const js = async (expr) => { const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? JSON.stringify(r.exceptionDetails)); return r.result.value; };
await send("Page.enable"); await send("Runtime.enable");
const args = process.argv.slice(2);
const flag = (n, d) => { const i = args.indexOf(n); return i >= 0 ? args[i + 1] : d; };
const BASE = flag("--base", "http://127.0.0.1:8688").replace(/\/$/, "");
const MEASURE = `JSON.stringify((() => {
  const g = (s) => { const e = document.querySelector(s); if (!e) throw new Error('missing ' + s + ' phase=' + (window.__demo ? window.__demo.state().phase : '?')); return e.getBoundingClientRect(); };
  const ph = g('.phone'), sc = g('.phone .screen'), no = g('.ask .no'), ar = g('.ask');
  const pb = parseFloat(getComputedStyle(document.querySelector('.ask')).paddingBottom);
  const lap = document.querySelector('.laptop'); const lr = lap && lap.offsetParent ? lap.getBoundingClientRect() : null;
  return { phoneW: +ph.width.toFixed(1), phoneH: +ph.height.toFixed(1), screenH: +sc.height.toFixed(1),
    laptopW: lr ? +lr.width.toFixed(1) : 0, laptopH: lr ? +lr.height.toFixed(1) : 0,
    spare: +(ar.bottom - pb - no.bottom).toFixed(1), askOver: +(ar.bottom - sc.bottom).toFixed(1),     stageBottom: +g('.stage').bottom.toFixed(1), yesBottom: +g('.ask .yes').bottom.toFixed(1), innerH: innerHeight };
})())`;
let bad = 0;
console.log("width  lang  phone       screenH  laptop       spare(refuse→box)  askOver  yesBottom/innerH");
for (const [w, h] of [[390, 844], [430, 932], [761, 900], [768, 1024], [900, 900], [1024, 768], [1280, 720],
                      [1366, 768], [1440, 900], [1536, 864], [1920, 1080], [2000, 1117], [2560, 1440]]) {
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: w < 560 });
  await send("Page.navigate", { url: `${BASE}/?fit=${Date.now()}` });
  let ready = false;
  for (let i = 0; i < 60 && !ready; i++) { await sleep(200);
    try { ready = await js("!!document.querySelector('.ask .no') && !document.getElementById('phone').classList.contains('asleep')"); } catch {} }
  await sleep(700);   /* .rise 动画 .42s：没等完量到的是半路上的位置 */
  if (!ready) { console.log(w, "not ready:", await js("location.href + ' | ' + document.title + ' | ' + document.body.innerHTML.length")); continue; }
  for (const lang of ["en", "zh"]) {
    if (lang === "zh") { await js("document.querySelector('#lang').click()"); await sleep(500); }
    const m = JSON.parse(await js(MEASURE));
    const ok = m.spare >= -0.5 && m.askOver <= 0.5 && m.yesBottom <= m.innerH;
    if (!ok) bad++;
    console.log(`${String(w).padEnd(6)} ${lang}   ${String(m.phoneW + "×" + m.phoneH).padEnd(11)} ${String(m.screenH).padEnd(8)} ${String(m.laptopW + "×" + m.laptopH).padEnd(12)} ${String(m.spare).padEnd(18)} ${String(m.askOver).padEnd(8)} ${m.yesBottom}/${m.innerH}${ok ? "" : "   <-- FAIL"}`);
  }
}
ws.close();
console.log(bad ? `\n${bad} 档装不下` : "\n每一档都装得下，蓝按钮都在折线以上");
process.exit(bad ? 1 : 0);
