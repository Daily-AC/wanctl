#!/usr/bin/env node
/* Responsive sweep for wc.z10.dev (product site) and wc.z10.dev/docs.
 *
 * Why this exists: phone-layout defects were being found one at a time, by hand,
 * on a real device. One at a time is the expensive way to find ten of them. This
 * walks every state of both sites at every viewport in both languages and writes
 * down what it measures, so a fix round starts from a list instead of a report.
 *
 * It drives its own headless Chrome over CDP. Two reasons it is not `--window-size`:
 * headless clamps the window to 500px wide (see HANDOFF §4), and `Emulation.
 * setDeviceMetricsOverride` sets the *layout* viewport, which is what media
 * queries and overflow actually read. It also does not use the shared Playwright
 * MCP: this run resizes and navigates hundreds of times and would fight anyone
 * else on that browser.
 *
 * Usage
 *   node tools/sweep.mjs --base http://127.0.0.1:8711 --out <dir> [--shots]
 *   node tools/sweep.mjs --list            # print the state list, launch nothing
 *
 * Output
 *   <dir>/findings.json   one record per state × viewport × language
 *   <dir>/summary.txt     human-readable roll-up, worst first
 *   <dir>/shots/*.png     with --shots
 *
 * Prerequisite: `site/` served somewhere (python3 -m http.server) with the docs
 * built into `site/docs/` (uv run tools/docsite/build.py --out site/docs).
 */

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync, rmSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ── viewports ───────────────────────────────────────────────────────
   The five phone widths are the ones people actually hold; 320 is the floor
   nothing may break at. 768/1024 are the tablet turns, 1200/1440/1920 the
   desktop ones. 1440 is also the pixel-diff baseline. */
const VIEWPORTS = [
  { w: 320, h: 568 }, { w: 360, h: 780 }, { w: 390, h: 844 },
  { w: 412, h: 915 }, { w: 430, h: 932 },
  { w: 768, h: 1024 }, { w: 1024, h: 768 },
  { w: 1200, h: 820 }, { w: 1440, h: 900 }, { w: 1920, h: 1080 },
];
const PHONE_MAX = 560;   // below this the site's own "phone" rules apply

/* ── states ──────────────────────────────────────────────────────────
   A state is a URL plus whatever has to be done to the page before measuring.
   `setup` runs in the page and may return a promise. */
const STATES = [
  // ── product site ──
  { id: "home", site: "site", url: "/", note: "all sections, install picker at its defaults" },
  {
    id: "install-unix-cn", site: "site", url: "/", note: "install: macOS·Linux × China mirror",
    setup: `document.querySelector('[data-src="cn"]').click()`,
  },
  {
    id: "install-win-gh", site: "site", url: "/", note: "install: Windows × GitHub",
    setup: `document.querySelector('[data-os="win"]').click()`,
  },
  {
    id: "install-win-cn", site: "site", url: "/", note: "install: Windows × China mirror",
    setup: `document.querySelector('[data-os="win"]').click();document.querySelector('[data-src="cn"]').click()`,
  },
  {
    id: "install-copied", site: "site", url: "/", note: "install: the copy button after a click",
    setup: `document.querySelector('#copy').click()`,
  },
  {
    id: "hero-asleep", site: "site", url: "/", note: "hero scene, phone still dark (opening frame)",
    setup: `document.querySelector('#phone').classList.add('asleep')`,
  },
  {
    id: "hero-answered", site: "site", url: "/", note: "hero scene after the approval is answered",
    setup: `window.__demo && window.__demo.act('y')`,
  },

  // ── docs site ──
  { id: "docs-index", site: "docs", url: "/docs/", note: "the contents page" },
  { id: "docs-zh-guide", site: "docs", url: "/docs/enroll-device/", note: "Chinese-source guide; also the page with no h2 (no on-page contents)" },
  { id: "docs-en-tech", site: "docs", url: "/docs/architecture/", note: "English technical doc, 18 headings, 6 code blocks" },
  { id: "docs-audit", site: "docs", url: "/docs/security-audit-2026-07-23/", note: "the audit with its two wide tables" },
  { id: "docs-longest", site: "docs", url: "/docs/android/", note: "the longest page: 10 tables, 32 code blocks" },
  { id: "docs-tables", site: "docs", url: "/docs/environment/", note: "12 tables, the densest table page" },
  { id: "docs-code", site: "docs", url: "/docs/self-hosting/", note: "30 code blocks, the widest commands" },
  {
    id: "docs-drawer-open", site: "docs", url: "/docs/architecture/", note: "phone drawer open (no-op above 1099)",
    setup: `(document.querySelector('.dnav-open')||{click(){}}).click()`,
  },
  {
    id: "docs-midpage", site: "docs", url: "/docs/architecture/",
    note: "scrolled to the middle, then the language toggled — the sticky columns and the on-page contents highlight",
    setup: `scrollTo({top: Math.round(document.body.scrollHeight*0.45), behavior:'instant'});
            await new Promise(r=>setTimeout(r,120));
            document.querySelector('#lang').click();
            await new Promise(r=>setTimeout(r,160));`,
  },
];

const LANGS = ["en", "zh"];

/* ── the measuring tape, run inside the page ─────────────────────────
   Everything here is a measurement, not a judgment. Deciding whether a
   scrollable command pill is a defect is a person's job; the script's job is
   to say it scrolls and by how much. */
const PROBE = `(() => {
  const vw = innerWidth, vh = innerHeight;
  const de = document.documentElement;
  const name = (el) => {
    if (!el || el === de) return 'html';
    const id = el.id ? '#' + el.id : '';
    const cls = (el.getAttribute('class') || '').trim().split(/\\s+/).filter(Boolean).slice(0,3).map(c=>'.'+c).join('');
    return el.tagName.toLowerCase() + id + cls;
  };
  const text = (el) => (el.textContent || '').replace(/\\s+/g, ' ').trim().slice(0, 60);
  const vis = (el) => {
    const s = getComputedStyle(el);
    if (s.display === 'none' || s.visibility === 'hidden' || +s.opacity === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const all = Array.from(document.querySelectorAll('body *')).filter(vis);

  /* 1. horizontal overflow, and who sticks out */
  const docOverflow = de.scrollWidth - vw;
  const out = [];
  for (const el of all) {
    const r = el.getBoundingClientRect();
    // an element inside a scroll container is not sticking out of the page
    let clipped = false;
    for (let p = el.parentElement; p && p !== de; p = p.parentElement) {
      const ox = getComputedStyle(p).overflowX;
      if (ox === 'hidden' || ox === 'auto' || ox === 'scroll' || ox === 'clip') { clipped = true; break; }
    }
    if (clipped) continue;
    if (r.right > vw + 1 || r.left < -1) {
      out.push({ el: name(el), right: +r.right.toFixed(1), left: +r.left.toFixed(1), w: +r.width.toFixed(1), text: text(el) });
    }
  }

  /* 2. content wider than its own box */
  const clip = [];
  for (const el of all) {
    if (el.scrollWidth <= el.clientWidth + 1) continue;
    if (!el.clientWidth) continue;
    const s = getComputedStyle(el);
    const ox = s.overflowX;
    if (ox === 'visible') continue;
    clip.push({
      el: name(el), overflowX: ox, ellipsis: s.textOverflow === 'ellipsis',
      client: el.clientWidth, scroll: el.scrollWidth, hidden: el.scrollWidth - el.clientWidth,
      text: text(el),
    });
  }

  /* 3. the sticky header, and whether the language button is reachable */
  const nav = document.querySelector('.nav');
  const lang = document.querySelector('#lang');
  const navBox = nav ? nav.getBoundingClientRect() : null;
  const langBox = lang ? lang.getBoundingClientRect() : null;
  const header = {
    navRight: navBox ? +navBox.right.toFixed(1) : null,
    navH: navBox ? +navBox.height.toFixed(1) : null,
    lang: langBox ? {
      left: +langBox.left.toFixed(1), right: +langBox.right.toFixed(1),
      w: +langBox.width.toFixed(1), h: +langBox.height.toFixed(1),
      inside: langBox.left >= -0.5 && langBox.right <= vw + 0.5,
    } : null,
    linksVisible: Array.from(document.querySelectorAll('.nav .links a')).filter(vis).map(a => text(a)),
    linksClipped: (() => {
      const l = document.querySelector('.nav .links');
      return l ? l.scrollWidth - l.clientWidth : 0;
    })(),
    brandLeft: (() => { const b = document.querySelector('.nav .brand'); return b ? +b.getBoundingClientRect().left.toFixed(1) : null; })(),
  };

  /* 4. section heights — DESIGN.md §5 wants one subject per screen on phones */
  const sections = Array.from(document.querySelectorAll('.tile')).map(s => ({
    id: s.id || '(hero)', h: Math.round(s.getBoundingClientRect().height), vh,
    over: Math.round(s.getBoundingClientRect().height - vh),
  }));

  /* 5. text below 13px. The phone mock's own type is deliberately tiny. */
  const small = [];
  for (const el of all) {
    if (el.closest('.stage')) continue;              // the device render, on purpose
    const own = Array.from(el.childNodes).some(n => n.nodeType === 3 && n.textContent.trim());
    if (!own) continue;
    const fs = parseFloat(getComputedStyle(el).fontSize);
    if (fs < 13) small.push({ el: name(el), fs: +fs.toFixed(2), text: text(el) });
  }

  /* 6. tap targets.
     A link sitting in a sentence is 19px tall on every site on the web and is
     not a defect; a control is. So the two are counted apart, and only controls
     are held to 40x40: the header buttons, the segmented picker, the copy key,
     the drawer trigger, and every link that is laid out as its own row. */
  const CONTROL = '.nav, .picker, .cmdline, .dnav, .dtoc, .crumbs, .foot .flinks, .pager, .dindex .g, .dnav-head, .doors, .cta-row';
  /* The box is not the target: a transparent ::after can extend the reachable
     area without moving a pixel of ink, and .nav .lang does exactly that. So
     hit-test outward from the centre instead of reading the rect. */
  const reach = (el, r, dx, dy) => {
    let n = 0;
    for (; n <= 24; n += 2) {
      const x = r.left + r.width / 2 + dx * (n + 1);
      const y = r.top + r.height / 2 + dy * (n + 1);
      if (x < 0 || y < 0 || x > vw || y > vh) break;
      const hit = document.elementFromPoint(x, y);
      if (!hit || (hit !== el && !el.contains(hit))) break;
    }
    return n;
  };
  const taps = [];
  for (const el of all) {
    if (el.closest('.stage')) continue;
    if (!el.matches('a, button, [role="button"], input, select, summary')) continue;
    const r = el.getBoundingClientRect();
    const w = Math.max(r.width, reach(el, r, 1, 0) + reach(el, r, -1, 0));
    const h = Math.max(r.height, reach(el, r, 0, 1) + reach(el, r, 0, -1));
    if (w >= 40 && h >= 40) continue;
    const disp = getComputedStyle(el).display;
    const control = el.matches('button, [role="button"], input, select, summary')
      || !!el.closest(CONTROL) || disp === 'block' || disp === 'flex' || disp === 'grid';
    taps.push({ el: name(el), w: +w.toFixed(1), h: +h.toFixed(1),
                box: (+r.width.toFixed(1)) + '×' + (+r.height.toFixed(1)), control, text: text(el) });
  }

  /* 7. tables: wider than the box that is meant to scroll them */
  const tables = Array.from(document.querySelectorAll('table')).filter(vis).map(t => {
    const box = t.closest('.tw') || t.parentElement;
    const bs = box ? getComputedStyle(box) : null;
    return {
      wrapper: box ? name(box) : null,
      overflowX: bs ? bs.overflowX : null,
      table: Math.round(t.scrollWidth),
      box: box ? box.clientWidth : null,
      boxScrolls: box ? box.scrollWidth > box.clientWidth + 1 : false,
    };
  });

  /* 8. anything that sits on top of the text.
     Only at scroll 0: the sticky bar is *meant* to have content slide under it
     (DESIGN.md §8 keeps the backdrop-filter for exactly that), so measuring it
     mid-scroll reports the design as a defect. */
  const overlaps = [];
  if (navBox && scrollY < 1) {
    const main = document.querySelector('main, .dpage, .dindex');
    if (main) {
      for (const el of Array.from(main.querySelectorAll('h1, h2, h3, p, li, code')).filter(vis)) {
        const r = el.getBoundingClientRect();
        if (r.top < navBox.bottom - 0.5 && r.bottom > navBox.top + 0.5 && r.left < navBox.right && r.right > navBox.left) {
          const z = getComputedStyle(el).zIndex;
          overlaps.push({ el: name(el), top: +r.top.toFixed(1), navBottom: +navBox.bottom.toFixed(1), z });
          if (overlaps.length > 4) break;
        }
      }
    }
  }

  /* 9. the docs side columns: are they still on screen after a scroll, and is
        the on-page contents marking where the reader is */
  const stick = (sel) => {
    const e = document.querySelector(sel);
    if (!e || !vis(e)) return null;
    const r = e.getBoundingClientRect();
    return { top: +r.top.toFixed(1), bottom: +r.bottom.toFixed(1), h: +r.height.toFixed(1),
             pos: getComputedStyle(e).position, onScreen: r.bottom > 0 && r.top < vh };
  };
  const columns = { dnav: stick('.dnav'), dtoc: stick('.dtoc'),
                    tocNow: !!document.querySelector('.dtoc a.now'),
                    scrollY: Math.round(scrollY) };

  return {
    vw, vh, docScrollWidth: de.scrollWidth, docOverflow, columns,
    docHeight: de.scrollHeight,
    out, clip, header, sections, small, taps, tables, overlaps,
    lang: de.lang,
  };
})()`;

/* ── minimal CDP client ──────────────────────────────────────────────
   Node 22 has a global WebSocket, so this needs nothing installed. */
async function connect(port) {
  let tabs = null;
  for (let i = 0; i < 40 && !tabs; i++) {
    for (const host of ["127.0.0.1", "localhost"]) {
      try {
        const r = await fetch(`http://${host}:${port}/json`, { signal: AbortSignal.timeout(1200) });
        if (r.ok) { const t = await r.json(); if (t.length) { tabs = t.map(x => ({ ...x, host })); break; } }
      } catch { /* not up yet */ }
    }
    if (!tabs) await sleep(400);
  }
  if (!tabs) throw new Error(`CDP port ${port} never came up`);
  /* Attaching to the wrong browser is silent and total: the run finishes, the
     numbers are real, and they describe somebody else's page. It happened once
     here — another agent's headless Chrome held the guessed port, our spawn
     failed to bind, and a whole sweep came back measuring the device portal.
     Hence: the port is read out of our own profile, never guessed. */
  const page = tabs.find(t => t.type === "page");
  const ws = new WebSocket(page.webSocketDebuggerUrl.replace("localhost", page.host));
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

  let id = 0;
  const pending = new Map();
  const events = [];
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
    else if (m.method) events.push(m);
  };
  const send = (method, params = {}, timeoutMs = 30000) =>
    new Promise((res, rej) => {
      const i = ++id;
      const timer = setTimeout(() => { pending.delete(i); rej(new Error(`${method}: timeout`)); }, timeoutMs);
      pending.set(i, (m) => { clearTimeout(timer); m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result); });
      ws.send(JSON.stringify({ id: i, method, params }));
    });

  await send("Page.enable");
  await send("Runtime.enable");
  await send("Log.enable");
  await send("Network.enable");

  return {
    send, events,
    drainEvents() { const e = events.slice(); events.length = 0; return e; },
    async raw(expr) {
      const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? "eval failed");
      return r.result.value;
    },
    async shot(file, full = false) {
      const p = { format: "png" };
      if (full) p.captureBeyondViewport = true;
      const r = await send("Page.captureScreenshot", p, 60000);
      writeFileSync(file, Buffer.from(r.data, "base64"));
    },
    close() { ws.close(); },
  };
}

/* ── driver ──────────────────────────────────────────────────────────── */
const args = process.argv.slice(2);
const flag = (n, d) => { const i = args.indexOf(n); return i >= 0 ? args[i + 1] : d; };

if (args.includes("--list")) {
  for (const s of STATES) console.log(`${s.site.padEnd(5)} ${s.id.padEnd(20)} ${s.url.padEnd(38)} ${s.note}`);
  console.log(`\n${STATES.length} states × ${VIEWPORTS.length} viewports × ${LANGS.length} languages = ${STATES.length * VIEWPORTS.length * LANGS.length} measurements`);
  process.exit(0);
}

const BASE = flag("--base", "http://127.0.0.1:8711").replace(/\/$/, "");
const OUT = flag("--out", "sweep-out");
const SHOTS = args.includes("--shots");
const ONLY = flag("--only", null);          // substring filter on state id
const SHOTW = flag("--shotw", "412,1440").split(",").map(Number);   // which widths get screenshots

mkdirSync(OUT, { recursive: true });
if (SHOTS) mkdirSync(join(OUT, "shots"), { recursive: true });

const CHROME = process.env.CHROME_BIN || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
/* Port 0 lets Chrome pick a free one and write it into our own profile, so a
   sweep can never end up driving another agent's browser (see connect()). */
const profile = join(tmpdir(), `wanctl-sweep-${process.pid}-${Date.now()}`);
const chrome = spawn(CHROME, [
  "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
  "--no-default-browser-check", "--disable-extensions", "--force-device-scale-factor=1",
  `--user-data-dir=${profile}`, "--remote-debugging-port=0", "about:blank",
], { stdio: "ignore" });

const portFile = join(profile, "DevToolsActivePort");
let PORT = null;
for (let i = 0; i < 80 && !PORT; i++) {
  if (existsSync(portFile)) {
    const first = readFileSync(portFile, "utf8").split("\n")[0].trim();
    if (/^\d+$/.test(first)) PORT = Number(first);
  }
  if (!PORT) await sleep(250);
}
if (!PORT) { chrome.kill(); throw new Error("Chrome never wrote DevToolsActivePort"); }

const cdp = await connect(PORT);

/* Freeze the two live clocks. Both are real (the stopwatch is the product
 * blocking on a person), but a wall clock in a screenshot is 838px of fake
 * diff — HANDOFF §3.11 hit this. */
const FREEZE = `
  (() => {
    const t = document.querySelector('#credtime'); if (t) t.textContent = '00:00:00';
    const l = document.querySelector('#ledger');
    if (l) l.textContent = 'acme  bench-02  dial  2026-09-05 00:00:00';
  })()`;

const findings = [];
let seed = null;
let n = 0;
const total = STATES.filter(s => !ONLY || s.id.includes(ONLY)).length * VIEWPORTS.length * LANGS.length;

for (const state of STATES) {
  if (ONLY && !state.id.includes(ONLY)) continue;
  for (const lang of LANGS) {
    for (const vp of VIEWPORTS) {
      n++;
      const isPhone = vp.w <= 768;
      await cdp.send("Emulation.setDeviceMetricsOverride", {
        width: vp.w, height: vp.h, deviceScaleFactor: 1, mobile: isPhone,
        screenOrientation: { angle: 0, type: "portraitPrimary" },
      });
      // seed the language before any page script runs. Remove the previous one
      // first — these stack, and 320 of them is 320 scripts per navigation.
      if (seed) await cdp.send("Page.removeScriptToEvaluateOnNewDocument", { identifier: seed });
      seed = (await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
        source: `try{localStorage.setItem('wanctl.lang',${JSON.stringify(lang)})}catch(e){}`,
      })).identifier;
      cdp.drainEvents();
      await cdp.send("Page.navigate", { url: BASE + state.url });
      await sleep(state.site === "site" ? 900 : 500);
      await cdp.raw(FREEZE);
      if (state.setup) {
        try { await cdp.raw(`(async () => { ${state.setup} })()`); } catch (e) { /* recorded below */ }
        await sleep(350);
      }
      await sleep(150);

      let probe;
      try { probe = await cdp.raw(PROBE); }
      catch (e) { probe = { error: String(e) }; }

      const evs = cdp.drainEvents();
      const consoleErrors = evs
        .filter(e => e.method === "Log.entryAdded" && ["error", "warning"].includes(e.params.entry.level))
        .map(e => `${e.params.entry.level}: ${e.params.entry.text}`.slice(0, 200));
      const netFail = evs
        .filter(e => e.method === "Network.responseReceived" && e.params.response.status >= 400)
        .map(e => `${e.params.response.status} ${e.params.response.url}`);
      const netLoadFail = evs
        .filter(e => e.method === "Network.loadingFailed")
        .map(e => `${e.params.errorText} ${e.params.type}`);

      findings.push({ state: state.id, site: state.site, url: state.url, note: state.note, lang, vp: `${vp.w}x${vp.h}`, w: vp.w, ...probe, consoleErrors, netFail, netLoadFail });

      if (SHOTS && SHOTW.includes(vp.w)) {
        const base = `${state.site}-${state.id}-${vp.w}-${lang}`;
        await cdp.shot(join(OUT, "shots", `${base}-full.png`), true);
        // one viewport-sized frame per section, so a defect can be pointed at
        if (state.site === "site" && (state.id === "home")) {
          const ids = (probe.sections || []).map(s => s.id);
          for (const sid of ids) {
            const sel = sid === "(hero)" ? ".tile.hero" : `#${sid}`;
            await cdp.raw(`(() => { const e = document.querySelector(${JSON.stringify(sel)}); if (e) scrollTo({top: e.getBoundingClientRect().top + scrollY - 48, behavior:'instant'}); })()`);
            await sleep(220);
            await cdp.shot(join(OUT, "shots", `${base}-${sid.replace(/[()]/g, "")}.png`));
          }
          await cdp.raw(`scrollTo({top: document.body.scrollHeight, behavior:'instant'})`);
          await sleep(220);
          await cdp.shot(join(OUT, "shots", `${base}-footer.png`));
        }
      }
      if (n % 20 === 0) process.stderr.write(`  ${n}/${total}\n`);
    }
  }
}

writeFileSync(join(OUT, "findings.json"), JSON.stringify(findings, null, 1));

/* ── roll-up ─────────────────────────────────────────────────────────── */
const lines = [];
const push = (s) => lines.push(s);
push(`sweep ${new Date().toISOString()}  base=${BASE}`);
push(`${findings.length} measurements: ${STATES.length} states × ${VIEWPORTS.length} viewports × ${LANGS.length} languages`);
push("");

const overflowing = findings.filter(f => f.docOverflow > 0);
push(`## horizontal overflow (${overflowing.length} of ${findings.length})`);
for (const f of overflowing) {
  push(`  ${f.state} ${f.vp} ${f.lang}: doc ${f.docScrollWidth} > vw ${f.vw} (+${f.docOverflow})`);
  for (const o of (f.out || []).slice(0, 6)) push(`      ${o.el} right=${o.right} left=${o.left} w=${o.w}  "${o.text}"`);
}
push("");

const langOut = findings.filter(f => f.header?.lang && !f.header.lang.inside);
push(`## language button not fully on screen (${langOut.length})`);
for (const f of langOut) push(`  ${f.state} ${f.vp} ${f.lang}: left=${f.header.lang.left} right=${f.header.lang.right}`);
push("");

const linkClip = findings.filter(f => (f.header?.linksClipped || 0) > 0);
push(`## header links clipped (${linkClip.length})`);
for (const f of linkClip) push(`  ${f.state} ${f.vp} ${f.lang}: ${f.header.linksClipped}px hidden; visible = ${f.header.linksVisible.join(" | ")}`);
push("");

push(`## content wider than its box`);
const clipAgg = new Map();
for (const f of findings) {
  for (const c of f.clip || []) {
    const k = `${c.el}|${c.overflowX}`;
    if (!clipAgg.has(k)) clipAgg.set(k, { ...c, where: [] });
    clipAgg.get(k).where.push(`${f.state}/${f.vp}/${f.lang}(+${c.hidden})`);
  }
}
for (const [k, c] of [...clipAgg].sort((a, b) => b[1].where.length - a[1].where.length)) {
  push(`  ${k}  ellipsis=${c.ellipsis}  ×${c.where.length}   e.g. ${c.where.slice(0, 3).join(", ")}`);
  push(`      "${c.text}"`);
}
push("");

push(`## sections taller than the viewport (phones only, ≤${PHONE_MAX})`);
for (const f of findings.filter(f => f.w <= PHONE_MAX)) {
  for (const s of (f.sections || []).filter(s => s.over > 0)) {
    push(`  ${f.state} ${f.vp} ${f.lang}: #${s.id} ${s.h} vs ${s.vh} (+${s.over})`);
  }
}
push("");

push(`## text under 13px (phones)`);
const smallAgg = new Map();
for (const f of findings.filter(f => f.w <= PHONE_MAX)) {
  for (const s of f.small || []) {
    const k = `${s.el}@${s.fs}`;
    if (!smallAgg.has(k)) smallAgg.set(k, { ...s, where: [] });
    smallAgg.get(k).where.push(`${f.state}/${f.vp}/${f.lang}`);
  }
}
for (const [k, s] of [...smallAgg].sort((a, b) => a[1].fs - b[1].fs)) {
  push(`  ${k}  ×${s.where.length}  "${s.text}"   e.g. ${s.where.slice(0, 2).join(", ")}`);
}
push("");

push(`## controls under 40×40 (phones) — inline prose links excluded`);
const tapAgg = new Map();
let inlineSmall = 0;
for (const f of findings.filter(f => f.w <= PHONE_MAX)) {
  for (const t of f.taps || []) {
    if (!t.control) { inlineSmall++; continue; }
    const k = `${t.el} ${t.w}×${t.h}`;
    if (!tapAgg.has(k)) tapAgg.set(k, { ...t, where: [] });
    tapAgg.get(k).where.push(`${f.state}/${f.vp}/${f.lang}`);
  }
}
for (const [k, t] of [...tapAgg].sort((a, b) => Math.min(a[1].w, a[1].h) - Math.min(b[1].w, b[1].h))) {
  push(`  ${k}  ×${t.where.length}  "${t.text}"   e.g. ${t.where.slice(0, 2).join(", ")}`);
}
push(`  (plus ${inlineSmall} inline prose links under 40px tall — normal running text)`);
push("");

push(`## docs side columns after a scroll`);
for (const f of findings.filter(f => f.site === "docs" && f.columns)) {
  const c = f.columns;
  if (!c.scrollY) continue;
  const bad = (c.dnav && !c.dnav.onScreen) || (c.dtoc && !c.dtoc.onScreen);
  if (!bad && c.tocNow) continue;
  push(`  ${f.state} ${f.vp} ${f.lang} @y${c.scrollY}: dnav=${c.dnav ? c.dnav.pos + "/top" + c.dnav.top + "/on" + c.dnav.onScreen : "hidden"} dtoc=${c.dtoc ? c.dtoc.pos + "/top" + c.dtoc.top + "/on" + c.dtoc.onScreen : "hidden"} tocNow=${c.tocNow}`);
}
push("");

push(`## tables`);
const tblAgg = new Map();
for (const f of findings) {
  for (const t of f.tables || []) {
    if (t.table <= (t.box || 0) + 1) continue;
    const k = `${f.state}|${t.wrapper}|${t.overflowX}`;
    if (!tblAgg.has(k)) tblAgg.set(k, { ...t, where: [] });
    tblAgg.get(k).where.push(`${f.vp}/${f.lang}(${t.table}>${t.box})`);
  }
}
for (const [k, t] of tblAgg) push(`  ${k} scrolls=${t.boxScrolls} ×${t.where.length}  e.g. ${t.where.slice(0, 3).join(", ")}`);
push("");

push(`## header overlapping content`);
for (const f of findings.filter(f => (f.overlaps || []).length)) {
  push(`  ${f.state} ${f.vp} ${f.lang}: ${f.overlaps.map(o => `${o.el}@${o.top}`).join(", ")} (nav bottom ${f.overlaps[0].navBottom})`);
}
push("");

push(`## console / network`);
const noise = new Set();
for (const f of findings) {
  for (const m of [...f.consoleErrors, ...f.netFail, ...f.netLoadFail]) noise.add(`${f.state}: ${m}`);
}
for (const m of noise) push(`  ${m}`);
if (!noise.size) push("  (clean)");

writeFileSync(join(OUT, "summary.txt"), lines.join("\n") + "\n");
console.log(lines.join("\n"));

cdp.close();
chrome.kill();
await sleep(300);
try { rmSync(profile, { recursive: true, force: true }); } catch {}
