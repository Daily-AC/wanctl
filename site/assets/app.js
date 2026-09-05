/* wanctl product site — the live demo inside the two screens.
 *
 * 笔记本屏里是控制端（agent 在敲命令），手机屏里是设备主人（你）。
 * 两端都是页内模拟；官网上不声称这是真机，措辞一律「demo / 演示」。
 * 所有设备名、域名、路径都是虚构示例——这是公开页面，不渲染任何真实部署信息。
 *
 * 开场（这一段是 09-04 加的，起因：静止的首屏被当成一张图片）
 *   落地时手机屏是黑的 —— DESIGN.md 第 1 节那句「旁边手机亮了一下，它在问你」，
 *   现在真的亮一下：笔记本带光标敲出命令 → 终端开始**计时**等待 → 手机亮起，请求升上来。
 *   会走字的东西不可能是一张图片，所以那个秒表是主要的活体证明，不是装饰。
 *   醒着是 CSS 的默认状态，睡着只是 JS 临时加的一件事 ——
 *   无 JS / 无头渲染 / prefers-reduced-motion 三种情况看到的都是醒着的样子。
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

  var REDUCED = !!(window.matchMedia &&
                   window.matchMedia('(prefers-reduced-motion: reduce)').matches);

  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  /* 手机屏窄，命令必然要折行。浏览器允许在连字符后断开，于是
     `--epochs` 会被断成 `--` / `epochs` —— 读起来像两个不同的参数。
     每个 token 各自 nowrap，折行就只发生在空格处；
     长到一行装不下的 token（如 --query-gpu=memory.used）才放开内部断行，
     那时断点已经离开头的 -- 很远，不会再造出一个假参数。 */
  var TOK_FITS = 16;
  function cmdHTML(s) {
    return String(s).split(' ').map(function (w) {
      return '<span class="tok' + (w.length > TOK_FITS ? ' split' : '') + '">' + esc(w) + '</span>';
    }).join(' ');
  }

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
      nudge: 'still waiting \u2014 it\u2019s your call',
      pending: 'pending',
      finale: 'Three commands. Three answers. Nothing you did not allow.',
      replay: 'Run the demo again',
      src: {
        gh: 'Straight from the GitHub release.',
        cn: 'Served from the official relay, which runs in mainland China \u2014 for when GitHub is slow to reach. Same binaries, same signature.'
      },
      os: { unix: '', win: ' PowerShell 5.1 and up; no OpenSSL needed.' }
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
      nudge: '还在等 —— 这事得你点头',
      pending: '已等待',
      finale: '三条命令，三个回答。没有一件是你没允许的。',
      replay: '再跑一遍演示',
      src: {
        gh: '直接从 GitHub release 拉。',
        cn: '从官方 relay 拉，它在国内 —— GitHub 拉不动的时候走这条。同样的二进制，同样的签名。'
      },
      os: { unix: '', win: ' 需要 PowerShell 5.1 以上；不需要装 OpenSSL。' }
    }
  };
  var lang = 'en';
  var t = function () { return T[lang]; };

  /* ── 装它 ───────────────────────────────────────────────────────
     两条真实的路：GitHub release，和官方 relay。后者服务在国内，
     而且它发出去的脚本里烧着自己的地址（RELAY_SELF），
     所以从镜像拉的脚本会从镜像装二进制，整条链路不碰 GitHub。
     见 internal/relay/dist.go 的 installerHandler 与 WANCTL_PUBLIC_ORIGIN。 */
  var GH = 'https://github.com/Daily-AC/wanctl/releases/latest/download';
  var CN = 'https://wanctl-relay.z10.dev';
  var INSTALL = {
    'unix-gh': 'curl -fsSL ' + GH + '/install.sh | sh',
    'unix-cn': 'curl -fsSL ' + CN + '/install.sh | sh',
    'win-gh':  'irm ' + GH + '/install.ps1 | iex',
    'win-cn':  'irm ' + CN + '/install.ps1 | iex'
  };

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
  /* transport.ShortFingerprint 的形状：fp[:14] + … + fp[-6:]。
     指纹是虚构的，格式是真的，和「端到端身份」那一屏同源。 */
  var CTRL_FP = 'SHA256:fJoz5aA\u2026DOAEo=';

  var SAFER = { host: 'bench-02', by: 'claude@workstation',
    cmd: 'wanctl exec --target bench-02 "python train.py --dry-run"',
    raw: 'python train.py --dry-run',
    out: ['dry run: would resume from epoch_02.pt', 'no weights written'] };

  var st = { phase: 'asking', round: 0, rules: [], answers: 0, retried: false };

  var term = $('#term'), ask = $('#ask'), cred = $('#cred'),
      credtime = $('#credtime'), rows = $('#rows'), fleetcount = $('#fleetcount'),
      ledger = $('#ledger'), pin = $('#pin'), pinstate = $('#pinstate'),
      phone = $('#phone');

  /* 重放一段动画：先摘类再强制重排，否则同名 animation 不会重新开始 */
  function replay(el, cls) {
    if (!el || REDUCED) return;
    el.classList.remove(cls);
    void el.offsetWidth;
    el.classList.add(cls);
  }

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
    if (instant) { d.innerHTML = cmdHTML(text); if (done) done(); return; }
    var i = 0;
    d.innerHTML = '<span class="caret"></span>';
    (function step() {
      if (i >= text.length) { d.innerHTML = cmdHTML(text); if (done) done(); return; }
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

  /* ── 等待是有时长的 ────────────────────────────────────────────
     秒表每秒跳一次。这既是活体证明（照片不会走字），也是真的：
     wanctl exec 就是阻塞在那儿等人回答。 */
  var waitEl = null, waitAt = 0, waitTick = null, nudged = false;

  function clock(s) { return Math.floor(s / 60) + ':' + (s % 60 < 10 ? '0' : '') + (s % 60); }
  function waited() { return waitAt ? Math.floor((Date.now() - waitAt) / 1000) : 0; }
  /* 手机上也挂一份秒表：移动端终端在折线以下，落地时看不见那一份 */
  function pendText() { return t().pending + ' ' + clock(waited()); }

  function startWait() {
    stopWait();
    waitEl = line('wait', t().waiting + '  0:00');
    waitAt = Date.now();
    waitTick = setInterval(function () {
      if (!waitEl || !waitEl.parentNode) { stopWait(); return; }
      var n = waited();
      waitEl.textContent = t().waiting + '  ' + clock(n);
      /* 只改那颗时钟的文本 —— 早先写成 pend.textContent，第一次跳动就把
         那颗活体指示点一起抹掉了。 */
      var pt = ask.querySelector('.pend .pt');
      if (pt) pt.textContent = pendText();
      /* 十秒还没人动，让 agent 自己再说一句 —— 不给页面加新元素 */
      if (n >= 10 && !nudged) { nudged = true; line('wait', t().nudge); }
    }, 1000);
  }
  function stopWait() {
    if (waitTick) { clearInterval(waitTick); waitTick = null; }
    waitEl = null;
  }

  /* ── 手机屏 ────────────────────────────────────────────────────── */
  function renderAsk() {
    var j = job();
    if (st.phase !== 'asking' || !j) return;
    ask.innerHTML =
      '<div class="from">' + esc(j.by) + '</div>' +
      '<div class="fp">' + esc(CTRL_FP) + '</div>' +
      '<div class="from">' + esc(t().from) + '</div>' +
      '<div class="host">' + esc(j.host) + '</div>' +
      '<div class="what">' + cmdHTML(j.raw) + '</div>' +
      '<div class="pend mono"><span class="dot"></span>' +
        '<span class="pt">' + esc(pendText()) + '</span></div>' +
      '<div class="acts">' +
        '<button class="yes breathe" data-act="y">' + esc(t().once) + '</button>' +
        '<button class="alt"  data-act="a">' + esc(t().always) + '</button>' +
        '<button class="no"   data-act="n">' + esc(t().refuse) + '</button>' +
      '</div>';
    /* 「需要你」= 内容升上来 + 手机抬一下。命中已签规则那轮走 renderIdle，
       什么都不动 —— 动=需要你，不动=不需要你，这个区别本身就在讲产品。 */
    replay(ask, 'rise');
    replay(phone, 'attn');
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
    /* 命令这一格走 cmdHTML，不走 esc：窄屏上 dd 会折行，
       而浏览器允许在连字符后断开，`--epochs` 就成了 `--` / `epochs`。 */
    var cmdRow = '<dl class="kv"><dt>' + esc(k.cmd) + '</dt>' +
                 '<dd class="mono cmd">' + cmdHTML(j.raw) + '</dd></dl>';
    cred.innerHTML = row(k.by, t().you) + row(k.device, j.host, 1) +
                     row(k.from2, j.by, 1) + cmdRow +
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

    stopWait();
    if (v === 'n') {
      st.phase = 'refused';
      renderDone(false);
      credential(j, false, '');
      line('no', j.by + ': ' + t().refusedBy);
      setTimeout(function () {
        if (st.retried) { st.round++; st.retried = false; next(); return; }
        st.retried = true; st.phase = 'asking';
        type(SAFER.cmd, false, function () { startWait(); renderAsk(); });
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
    stopWait();
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
      startWait();
      renderAsk();
    });
  }

  function finale() {
    stopWait();
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
  function applyLang(l, animate) {
    lang = l;
    document.documentElement.lang = l === 'zh' ? 'zh-CN' : 'en';
    $('#lang').textContent = l === 'en' ? '中文' : 'EN';
    $$('[data-en]').forEach(function (el) {
      var v = el.getAttribute('data-' + l);
      if (v != null) el.innerHTML = v;
    });
    $$('[data-lbl-en]').forEach(function (el) {
      el.setAttribute('aria-label', el.getAttribute('data-lbl-' + l));
    });
    if (!pick.srcTouched) pick.src = l === 'zh' ? 'cn' : 'gh';
    renderInstall();
    renderFleet();
    boot(!!animate);
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }
  $('#lang').addEventListener('click', function () { applyLang(lang === 'en' ? 'zh' : 'en', false); });

  /* ── 分段控件：系统 × 下载源，命令永远只有一条 ─────────────────
     系统按 UA 猜一个初值，下载源跟着页面语言给一个初值（中文默认走镜像）；
     一旦用户自己点过，就再也不替他改主意了。 */
  var pick = { os: /Windows/i.test(navigator.userAgent || '') ? 'win' : 'unix',
               src: 'gh', srcTouched: false };
  var osseg = $('#osseg'), srcseg = $('#srcseg'),
      installcmd = $('#installcmd'), srcnote = $('#srcnote');

  function renderInstall() {
    installcmd.textContent = INSTALL[pick.os + '-' + pick.src];
    srcnote.textContent = t().src[pick.src] + t().os[pick.os];
    $$('[data-os]', osseg).forEach(function (b) {
      b.classList.toggle('on', b.dataset.os === pick.os);
      b.setAttribute('aria-pressed', b.dataset.os === pick.os);
    });
    $$('[data-src]', srcseg).forEach(function (b) {
      b.classList.toggle('on', b.dataset.src === pick.src);
      b.setAttribute('aria-pressed', b.dataset.src === pick.src);
    });
    markScroll();
  }

  /* 「还有」这个信号（app.css 的 .cmdline.more，一道 24px 的遮罩）。
     它以前挂在 ≤560 上，理由是那个宽度下四条命令全都溢出。但溢出跟不跟得上
     宽度不是同一件事：实测 768 下英文那条 GitHub 命令 762px 装进 624px 的框，
     还有 138px 在外面，而遮罩早在 560 就下线了 —— 命令被硬切在复制键上，
     一点提示都没有。宽度是猜，scrollWidth 是量的，所以改成量。
     顺带把 1440 那一档还原成没有遮罩：那里本来就不溢出。 */
  function markScroll() {
    var box = installcmd.parentNode;
    box.classList.toggle('more', installcmd.scrollWidth - installcmd.clientWidth > 1);
  }
  if (window.ResizeObserver) new ResizeObserver(markScroll).observe(installcmd);
  /* 字体是 font-display:swap 的：换字之前量到的宽度是回退字体的宽度。 */
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(markScroll);
  osseg.addEventListener('click', function (e) {
    var b = e.target.closest('[data-os]');
    if (b) { pick.os = b.dataset.os; renderInstall(); }
  });
  srcseg.addEventListener('click', function (e) {
    var b = e.target.closest('[data-src]');
    if (b) { pick.src = b.dataset.src; pick.srcTouched = true; renderInstall(); }
  });

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

  /* ── 启动 ──────────────────────────────────────────────────────
     boot(true) 播开场，boot(false) 直接落到终局（切语言时用，
     没人想为了换个语言再看一遍四秒的开场）。
     两条路的**终局完全相同**，所以无头渲染和 reduced-motion 走短路也不缺内容。 */
  function boot(animate) {
    stopWait();
    if (typer) { clearTimeout(typer); typer = null; }
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

    if (!animate || REDUCED) {
      if (phone) phone.classList.remove('asleep');
      type(j.cmd, true, function () { startWait(); renderAsk(); });
      return;
    }
    if (phone) phone.classList.add('asleep');
    setTimeout(function () {
      type(j.cmd, false, function () {
        startWait();
        setTimeout(function () {
          if (phone) phone.classList.remove('asleep');
          renderAsk();
        }, 240);
      });
    }, 420);
  }

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  if (saved === 'zh' || (!saved && /^zh/i.test(navigator.language || ''))) applyLang('zh', true);
  else { renderInstall(); renderFleet(); boot(true); }

  window.__demo = {
    state: function () {
      return { phase: st.phase, round: st.round, rules: st.rules.slice(),
               answers: st.answers, retried: st.retried, lang: lang,
               asleep: !!(phone && phone.classList.contains('asleep')),
               waiting: waitEl ? waitEl.textContent : null, nudged: nudged };
    },
    act: act, reset: function () { location.reload(); }
  };
})();
