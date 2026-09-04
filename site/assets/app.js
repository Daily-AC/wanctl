/* wanctl product site — the live demo inside the two screens.
 *
 * 笔记本屏里是控制端（agent 在敲命令），手机屏里是设备主人（你）。
 * 两端都是页内模拟；官网上不声称这是真机，措辞一律「demo / 演示」。
 * 所有设备名、域名、路径都是虚构示例——这是公开页面，不渲染任何真实部署信息。
 *
 * 状态机
 *   asking   手机上挂着一条请求，等 allow / always / refuse
 *   running  命令在跑，输出流进笔记本
 *   refused  拒绝后 agent 退让成一条只读命令，回到 asking
 *   done     走完，停在一句话上
 *
 * 验收接口 window.__demo：state() / act('y'|'a'|'n') / reset()
 */
(function () {
  'use strict';

  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  /* ── 虚构示例设备 ──────────────────────────────────────────────── */
  var DEVICES = [
    { host: 'studio-01', en: 'Workshop',    zh: '工作台',   os: 'macOS 15.6',       via: 'ws',   on: true },
    { host: 'bench-02',  en: 'Test bench',  zh: '测试机',   os: 'Windows 11',       via: 'ws',   on: true },
    { host: 'build-01',  en: 'Build box',   zh: '构建机',   os: 'Ubuntu 24.04',     via: 'ws',   on: true },
    { host: 'edge-fra',  en: 'Edge node',   zh: '边缘节点', os: 'Debian 13',        via: 'http', on: true },
    { host: 'handset-a', en: 'Handset',     zh: '手机',     os: 'Android 13',       via: '—',    on: false },
    { host: 'gateway-r', en: 'Gateway',     zh: '网关',     os: 'OpenWrt 24.10',    via: 'http', on: true }
  ];

  var T = {
    en: {
      from: 'wants to run on', once: 'Allow once', always: 'Always allow this', refuse: 'Refuse',
      waiting: 'waiting for the owner…', ruled: 'matched a rule you signed',
      lesson: 'A rule covers that command, not this one — so it asks again.',
      refusedBy: 'refused by owner, trying a read-only check',
      allowed: 'Allowed', refusedWord: 'Refused', idle: 'Nothing waiting.',
      k: { by: 'Answered by', device: 'Device', from2: 'From', cmd: 'Command', scope: 'Scope' },
      you: 'you', scopeOnce: 'once', scopeAlways: 'always, this command',
      scopeRule: 'matched a signed rule', offline: 'offline',
      count: function (n, m) { return n + ' devices · ' + m + ' online'; },
      verifying: 'Reading the other side\u2019s fingerprint\u2026',
      finale: 'Three commands. Three answers. Nothing you did not allow.',
      replay: 'Run the demo again'
    },
    zh: {
      from: '想在这台上跑', once: '允许一次', always: '这条命令一直允许', refuse: '拒绝',
      waiting: '等设备主人…', ruled: '命中了你签过的规则',
      lesson: '规则管的是那条命令，不管这一条——所以它还要再问一次。',
      refusedBy: '被主人拒绝，改用一条只读命令再试',
      allowed: '已允许', refusedWord: '已拒绝', idle: '没有等待中的请求。',
      k: { by: '回答人', device: '设备', from2: '来自', cmd: '命令', scope: '范围' },
      you: '你', scopeOnce: '仅此一次', scopeAlways: '这条命令一直允许',
      scopeRule: '命中已签规则', offline: '离线',
      count: function (n, m) { return n + ' 台 · ' + m + ' 台在线'; },
      verifying: '正在读取对方的指纹…',
      finale: '三条命令，三个回答。没有一件是你没允许的。',
      replay: '再跑一遍演示'
    }
  };
  var lang = 'en';
  var t = function () { return T[lang]; };

  /* ── 剧本 ──────────────────────────────────────────────────────── */
  var SCRIPT = [
    { host: 'bench-02', by: 'claude@workstation',
      cmd: 'wanctl exec --target bench-02 "python train.py --epochs 3 --resume"',
      raw: 'python train.py --epochs 3 --resume',
      out: ['loading checkpoint epoch_02.pt', 'epoch 3/3   loss 0.417', 'saved epoch_03.pt · 4m 51s'] },
    { host: 'bench-02', by: 'claude@workstation',
      cmd: 'wanctl exec --target bench-02 "python train.py --epochs 3 --resume --seed 7"',
      raw: 'python train.py --epochs 3 --resume --seed 7',
      out: ['seed 7 · epoch 3/3   loss 0.402', 'saved epoch_03-seed7.pt'] },
    { host: 'bench-02', by: 'claude@workstation', lesson: true,
      cmd: 'wanctl exec --target bench-02 "nvidia-smi --query-gpu=memory.used --format=csv"',
      raw: 'nvidia-smi --query-gpu=memory.used --format=csv',
      out: ['memory.used [MiB]', '21344 MiB'] }
  ];
  var SAFER = { host: 'bench-02', by: 'claude@workstation',
    cmd: 'wanctl exec --target bench-02 "python train.py --dry-run"',
    raw: 'python train.py --dry-run',
    out: ['dry run: would resume from epoch_02.pt', 'no weights written'] };

  var st = { phase: 'asking', round: 0, rules: [], answers: 0, retried: false };

  var term = $('#term'), ask = $('#ask'), cred = $('#cred'),
      credtime = $('#credtime'), rows = $('#rows'), fleetcount = $('#fleetcount'),
      ledger = $('#ledger'), pin = $('#pin'), pinstate = $('#pinstate');

  function job() { return st.retried ? SAFER : SCRIPT[st.round]; }

  /* 忠实移植 internal/policy 的 matchCommand + exec 分支：
     规则记的是**命令**，不是「这台机器上的任意命令」。 */
  function matchCommand(pat, cmd) {
    pat = String(pat || '').trim(); cmd = String(cmd || '').trim();
    if (!pat) return false;
    if (cmd === pat || pat === '*') return true;
    if (/[;&|><`$(){}\n]/.test(cmd)) return false;
    if (/ \*$/.test(pat)) return cmd.indexOf(pat.slice(0, -1)) === 0;
    return cmd.indexOf(pat + ' ') === 0;
  }
  function ruleHit(j) {
    for (var i = 0; i < st.rules.length; i++) if (matchCommand(st.rules[i], j.raw)) return st.rules[i];
    return null;
  }

  /* ── 笔记本屏 ──────────────────────────────────────────────────── */
  function line(cls, text) {
    var d = document.createElement('div');
    d.className = 'row ' + (cls || '');
    d.textContent = text;
    term.appendChild(d);
    while (term.childNodes.length > 12) term.removeChild(term.firstChild);
    return d;
  }
  var typer = null;
  function type(text, instant, done) {
    if (typer) { clearTimeout(typer); typer = null; }
    var d = line('cmd', '');
    if (instant) { d.textContent = text; if (done) done(); return; }
    var i = 0;
    d.innerHTML = '<span class="caret"></span>';
    (function step() {
      if (i >= text.length) { d.textContent = text; if (done) done(); return; }
      i++;
      d.innerHTML = esc(text.slice(0, i)) + '<span class="caret"></span>';
      typer = setTimeout(step, 15);
    })();
  }
  function stream(lines, done) {
    var i = 0;
    (function step() {
      if (i >= lines.length) { if (done) done(); return; }
      line('out', lines[i]); i++;
      setTimeout(step, 260);
    })();
  }

  /* ── 手机屏 ────────────────────────────────────────────────────── */
  function renderAsk() {
    var j = job();
    if (st.phase !== 'asking' || !j) return;
    ask.innerHTML =
      '<div class="from">' + esc(j.by) + '</div>' +
      '<div class="from">' + esc(t().from) + '</div>' +
      '<div class="host">' + esc(j.host) + '</div>' +
      '<div class="what">' + esc(j.raw) + '</div>' +
      '<div class="acts">' +
        '<button class="yes"  data-act="y">' + esc(t().once) + '</button>' +
        '<button class="alt"  data-act="a">' + esc(t().always) + '</button>' +
        '<button class="no"   data-act="n">' + esc(t().refuse) + '</button>' +
      '</div>';
  }
  function renderDone(ok) {
    ask.innerHTML =
      '<div class="done">' +
        '<span class="tickring' + (ok ? '' : ' refused') + '">' +
          (ok ? '<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">' +
                  '<path d="M3.5 9.4l3.4 3.4L14.5 5.2" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>'
              : '<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">' +
                  '<path d="M5 5l8 8M13 5l-8 8" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>') +
        '</span>' +
        '<span class="doneword">' + esc(ok ? t().allowed : t().refusedWord) + '</span>' +
      '</div>';
  }
  function renderIdle(msg) {
    ask.innerHTML = '<div class="idle">' + esc(msg || t().idle) + '</div>';
  }

  /* ── 凭证 ──────────────────────────────────────────────────────── */
  function credential(j, ok, scope) {
    st.answers++;
    var k = t().k, d = new Date(), p = function (n) { return (n < 10 ? '0' : '') + n; };
    credtime.textContent = d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
                           ' ' + p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
    var row = function (a, b, mono) {
      return '<dl class="kv"><dt>' + esc(a) + '</dt><dd' + (mono ? ' class="mono"' : '') + '>' + esc(b) + '</dd></dl>';
    };
    cred.innerHTML = row(k.by, t().you) + row(k.device, j.host, 1) +
                     row(k.from2, j.by, 1) + row(k.cmd, j.raw, 1) +
                     row(k.scope, ok ? scope : t().refusedWord);
    /* 中转那边的一整条记录：INSERT INTO audit (namespace, device, event)。
       没有命令、没有输出、没有文件名——就这四格。 */
    if (ledger) ledger.textContent = 'acme   ' + j.host + '   dial   ' + credtime.textContent;
  }

  /* ── 决定 ──────────────────────────────────────────────────────── */
  function act(v) {
    if (st.phase !== 'asking') return;
    var j = job();
    if (!j) return;

    if (v === 'n') {
      st.phase = 'refused';
      renderDone(false);
      credential(j, false, '');
      line('no', j.by + ': ' + t().refusedBy);
      setTimeout(function () {
        if (st.retried) { st.round++; st.retried = false; next(); return; }
        st.retried = true; st.phase = 'asking';
        type(SAFER.cmd, false, function () { line('wait', t().waiting); renderAsk(); });
      }, 1300);
      return;
    }

    var scope = v === 'a' ? t().scopeAlways : t().scopeOnce;
    if (v === 'a') st.rules.push(j.raw);
    st.phase = 'running';
    renderDone(true);
    credential(j, true, scope);
    stream(j.out, function () {
      setTimeout(function () { st.round++; st.retried = false; next(); }, 700);
    });
  }

  function next() {
    var cur = SCRIPT[st.round];
    if (!cur) { finale(); return; }
    var hit = ruleHit(cur);
    if (hit) {
      st.phase = 'running';
      renderIdle(t().ruled);
      type(cur.cmd, false, function () {
        line('out', '↳ ' + t().ruled);
        credential(cur, true, t().scopeRule);
        stream(cur.out, function () { setTimeout(function () { st.round++; next(); }, 700); });
      });
      return;
    }
    st.phase = 'asking';
    type(cur.cmd, false, function () {
      if (cur.lesson && st.rules.length) line('wait', t().lesson);
      line('wait', t().waiting);
      renderAsk();
    });
  }

  function finale() {
    st.phase = 'done';
    line('', '');
    line('cmd', t().finale);
    ask.innerHTML = '<div class="done">' +
      '<span class="doneword">' + esc(t().finale) + '</span>' +
      '<button class="yes" id="again" style="margin-top:8px">' + esc(t().replay) + '</button></div>';
    $('#again').addEventListener('click', function () { location.reload(); });
  }

  ask.addEventListener('click', function (e) {
    var b = e.target.closest('[data-act]');
    if (b) act(b.dataset.act);
  });

  /* ── 设备列表 ──────────────────────────────────────────────────── */
  function renderFleet() {
    rows.innerHTML = DEVICES.map(function (d) {
      return '<li>' +
        '<span class="dot' + (d.on ? '' : ' off') + '"></span>' +
        '<span class="name">' + esc(d[lang]) + '</span>' +
        '<span class="host">' + esc(d.host) + '</span>' +
        '<span class="meta">' + esc(d.os) + (d.on ? ' · ' + esc(d.via) : ' · ' + esc(t().offline)) + '</span>' +
      '</li>';
    }).join('');
    var on = DEVICES.filter(function (d) { return d.on; }).length;
    fleetcount.textContent = t().count(DEVICES.length, on);
  }

  /* ── 指纹当着你的面钉一次 ──────────────────────────────────────
     CSS 里的默认状态就是终局，这里只是短暂退回未钉状态再放开。
     无 JS、无头渲染、reduced-motion 三种情况下看到的都是已钉住的真相。 */
  function playPin() {
    if (!pin || !pinstate) return;
    if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    var settled = pinstate.innerHTML;
    pin.classList.add('verifying');
    pinstate.textContent = t().verifying;
    setTimeout(function () {
      pin.classList.remove('verifying');
      setTimeout(function () { pinstate.innerHTML = settled; }, 620);
    }, 620);
  }
  if (pin && 'IntersectionObserver' in window) {
    var io = new IntersectionObserver(function (es) {
      es.forEach(function (e) {
        if (e.isIntersecting) { io.disconnect(); playPin(); }
      });
    }, { threshold: .35 });
    io.observe(pin);
  }

  /* ── 语言 ──────────────────────────────────────────────────────── */
  function applyLang(l) {
    lang = l;
    document.documentElement.lang = l === 'zh' ? 'zh-CN' : 'en';
    $('#lang').textContent = l === 'en' ? '中文' : 'EN';
    $$('[data-en]').forEach(function (el) {
      var v = el.getAttribute('data-' + l);
      if (v != null) el.innerHTML = v;
    });
    renderFleet();
    boot(true);
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }
  $('#lang').addEventListener('click', function () { applyLang(lang === 'en' ? 'zh' : 'en'); });

  var copy = $('#copy');
  copy.addEventListener('click', function () {
    var txt = $('#installcmd').textContent.trim();
    var ok = function () {
      copy.textContent = lang === 'en' ? 'Copied' : '已复制';
      setTimeout(function () { copy.textContent = lang === 'en' ? 'Copy' : '复制'; }, 1600);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(txt).then(ok, function () {});
    else {
      var ta = document.createElement('textarea');
      ta.value = txt; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); ok(); } catch (_) {}
      document.body.removeChild(ta);
    }
  });

  /* ── 启动：首屏一次画满，不靠动画才可见 ────────────────────────── */
  function boot(instant) {
    st = { phase: 'asking', round: 0, rules: [], answers: 0, retried: false };
    term.innerHTML = '';
    cred.innerHTML = '';
    credtime.textContent = '—';
    var j = SCRIPT[0];
    credential(j, true, t().scopeOnce);   /* 深色屏那块面板首屏就要有内容 */
    st.answers = 0;
    line('cmd', 'wanctl peers');
    DEVICES.forEach(function (d) {
      line('out', '  ' + d.host + (d.on ? '   online   ' + d.via : '   offline'));
    });
    line('out', '');
    type(j.cmd, instant !== false, function () { line('wait', t().waiting); renderAsk(); });
  }

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  if (saved === 'zh' || (!saved && /^zh/i.test(navigator.language || ''))) applyLang('zh');
  else { renderFleet(); boot(true); }

  window.__demo = {
    state: function () {
      return { phase: st.phase, round: st.round, rules: st.rules.slice(),
               answers: st.answers, retried: st.retried, lang: lang };
    },
    act: act, reset: function () { location.reload(); }
  };
})();
