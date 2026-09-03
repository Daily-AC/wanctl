/* TAGOUT — wanctl product site, hero demo.
   设备端与控制端全部为页内模拟；真实版本需要一台沙箱设备 + 演示 relay。
   页面上不声称这是真机，措辞一律「sandbox / 沙箱」。

   状态机（分支在 deny 上）
     awaiting  有牌挂在锁孔下，等 y/a/g/n
     running   命令在跑，输出逐行流进控制端那一列
     denied    拒绝 → agent 退让成一条只读命令 → 回到 awaiting
     done      三轮走完，停在一句话上

   验收接口 window.__demo：state() / act('y'|'a'|'g'|'n') / reset()
*/
(function () {
  'use strict';

  var $  = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  /* ── 六台设备 ──────────────────────────────────────────────────── */
  /* 虚构示例设备。官网是公开页面，绝不渲染任何真实部署的机器名、域名或延迟——
     与 docs/portal/ 的占位符规矩一致（relay.example.com / portal.example.com）。 */
  var DEVICES = [
    { slot: 1, host: 'studio-01',  en: 'Workshop',    zh: '工作台',
      os: 'macOS 15.6',       arch: 'arm64',  via: 'ws',   ver: 'v0.3.4', on: true },
    { slot: 2, host: 'bench-02',   en: 'Test bench',  zh: '测试机',
      os: 'Windows 11',       arch: 'amd64',  via: 'ws',   ver: 'v0.3.4', on: true },
    { slot: 3, host: 'build-01',   en: 'Build box',   zh: '构建机',
      os: 'Ubuntu 24.04',     arch: 'amd64',  via: 'ws',   ver: 'v0.3.4', on: true },
    { slot: 4, host: 'edge-fra',   en: 'Edge node',   zh: '边缘节点',
      os: 'Debian 13',        arch: 'amd64',  via: 'http', ver: 'v0.3.4', on: true },
    { slot: 5, host: 'handset-a',  en: 'Handset',     zh: '手机',
      os: 'Android 13 · APK', arch: 'arm64',  via: '—',    ver: 'v0.3.3', on: false },
    { slot: 6, host: 'gateway-r',  en: 'Gateway',     zh: '网关',
      os: 'OpenWrt 24.10',    arch: 'mipsle', via: 'http', ver: 'v0.3.4', on: true }
  ];

  var T = {
    en: {
      locked: 'Locked · do not override', denied: 'Refused',
      offline: 'offline 3h 12m',
      hintWait: 'Press a key, or pull the tag off.',
      hintRule: 'A rule you signed matched — no tag this time.',
      refused: 'refused', trusted: 'trusted',
      once: 'once', here: 'always, this directory', global: 'always, everywhere',
      byRule: 'matched a signed rule', you: 'you',
      k: { by: 'Signed by', date: 'Date', device: 'Device', via: 'Via', cmd: 'Command', scope: 'Scope' },
      pairTitle: 'Pair request',
      waiting: 'waiting for the owner to pull the tag…',
      deniedBy: 'denied by owner, trying a read-only check',
      deniedPair: 'denied by owner, connection dropped',
      lesson: 'The rule you signed covers that command — not this one. So it asks again.',
      finale: 'Your machines only moved when you said so.',
      signed: 'signatures on record',
      replay: 'Run it again',
      nothing: 'Nothing signed yet. The tag below the board is waiting on you.'
    },
    zh: {
      locked: '已上锁 · 不得越过', denied: '已拒绝',
      offline: '离线 3 小时 12 分',
      hintWait: '按一个键，或者把牌摘下来。',
      hintRule: '命中了你签过的规则——这次不挂牌。',
      refused: '已拒绝', trusted: '已信任',
      once: '仅此一次', here: '本目录一直允许', global: '全局一直允许',
      byRule: '命中已签规则', you: '你',
      k: { by: '签字人', date: '日期', device: '设备', via: '来自', cmd: '命令', scope: '范围' },
      pairTitle: '配对请求',
      waiting: '等设备主人摘牌…',
      deniedBy: '被主人拒绝，改用一条只读命令再试',
      deniedPair: '被主人拒绝，连接已断开',
      lesson: '你签的规则管的是那条命令，不管这一条。所以它还要再问一次。',
      finale: '你的机器只在你点头时动过。',
      signed: '条签名在册',
      replay: '再来一遍',
      nothing: '还没有签过。板子下面那块牌在等你。'
    }
  };

  var lang = 'en';
  var t = function () { return T[lang]; };

  /* ── 剧本 ──────────────────────────────────────────────────────── */
  var SCRIPT = [
    { host: 'bench-02', by: 'claude@workstation', cwd: 'C:\\work\\asr',
      cmd: 'wanctl exec --target bench-02 "python scripts/train.py --epochs 3 --resume"',
      raw: 'python scripts/train.py --epochs 3 --resume',
      short: 'python scripts/train.py --epochs 3 --resume',
      out: ['loading checkpoint epoch_02.pt',
            'epoch 3/3   loss 0.417   wer 11.2%',
            'saved epoch_03.pt  ·  4m 51s'] },
    /* 同一条命令加了个参数：前缀规则命中，这是 a/g 与 y 的分野 */
    { host: 'bench-02', by: 'claude@workstation', cwd: 'C:\\work\\asr',
      cmd: 'wanctl exec --target bench-02 "python scripts/train.py --epochs 3 --resume --seed 7"',
      raw: 'python scripts/train.py --epochs 3 --resume --seed 7',
      short: 'python scripts/train.py --epochs 3 --resume --seed 7',
      out: ['seed 7  ·  epoch 3/3   loss 0.402   wer 10.8%',
            'saved epoch_03-seed7.pt'] },
    /* 换一条命令：即使签了「全局」也照样挂牌——规则是按命令记的 */
    { host: 'bench-02', by: 'claude@workstation', cwd: 'C:\\work\\asr',
      cmd: 'wanctl exec --target bench-02 "nvidia-smi --query-gpu=name,memory.used --format=csv"',
      raw: 'nvidia-smi --query-gpu=name,memory.used --format=csv',
      short: 'nvidia-smi --query-gpu=name,memory.used',
      lesson: true,
      out: ['name, memory.used [MiB]',
            'NVIDIA GeForce RTX 4090, 21344 MiB'] },
    { pair: true, host: 'studio-01', by: 'codex@edge-fra',
      cmd: 'wanctl pair --target studio-01',
      raw: '', short: 'codex@edge-fra → studio-01',
      fp: 'SHA256:9e77…c41a' }
  ];

  var SAFER = { host: 'bench-02', by: 'claude@workstation', cwd: 'C:\\work\\asr',
    cmd: 'wanctl exec --target bench-02 "python scripts/train.py --dry-run --epochs 1"',
    raw: 'python scripts/train.py --dry-run --epochs 1',
    short: 'python scripts/train.py --dry-run --epochs 1',
    out: ['dry run: would resume from epoch_02.pt', 'no weights written'] };

  var st = { phase: 'awaiting', round: 0, rules: [], creds: 0, retried: false };

  var board  = $('#board'),  decide = $('#decide'), agent = $('#agent'),
      creds  = $('#creds'),  hint   = $('#hint'),   npend = $('#npend');

  function job() { return st.retried ? SAFER : SCRIPT[st.round]; }

  /* ── 控制端那一列 ──────────────────────────────────────────────── */
  function aline(cls, text) {
    var d = document.createElement('div');
    d.className = 'line ' + (cls || '');
    d.textContent = text;
    agent.appendChild(d);
    agent.scrollTop = agent.scrollHeight;
    return d;
  }
  var typer = null;
  function typeCmd(text, instant, done) {
    if (typer) { clearTimeout(typer); typer = null; }
    var d = document.createElement('div');
    d.className = 'line cmd';
    agent.appendChild(d);
    if (instant) { d.textContent = text; if (done) done(); return; }
    var i = 0;
    d.innerHTML = '<span class="cursor"></span>';
    (function step() {
      if (i >= text.length) { d.textContent = text; if (done) done(); return; }
      i++;
      d.innerHTML = esc(text.slice(0, i)) + '<span class="cursor"></span>';
      agent.scrollTop = agent.scrollHeight;
      typer = setTimeout(step, 17);
    })();
  }

  /* ── 板 ────────────────────────────────────────────────────────── */
  function screw(pos) {
    return '<svg class="screw ' + pos + '" viewBox="0 0 8 8" aria-hidden="true">' +
           '<circle cx="4" cy="4" r="3.4" fill="none" stroke="currentColor" stroke-width="1"/>' +
           '<path d="M2.1 4h3.8" stroke="currentColor" stroke-width="1"/></svg>';
  }

  function plateHTML(d) {
    var j = job() || {};
    var waiting = st.phase === 'awaiting' && j.host === d.host;
    return '' +
      '<div class="plate' + (d.on ? '' : ' off') + '" id="p-' + d.host + '">' +
        screw('tl') + screw('tr') + screw('bl') + screw('br') +
        '<div class="prow">' +
          '<span class="led ' + (d.on ? (waiting ? 'wait' : '') : 'off') + '"></span>' +
          '<span class="silk slot">Slot ' + d.slot + '</span>' +
          '<span class="via">' + esc(d.via) + '</span>' +
        '</div>' +
        '<div class="name">' + esc(d[lang]) + '</div>' +
        '<div class="tagplate"><span class="host">' + esc(d.host) + '</span></div>' +
        '<div class="spec">' + esc(d.os) + '</div>' +
        '<div class="spec last">' +
          (d.on ? esc(d.arch) + ' · ' + esc(d.ver)
                : '<span style="color:var(--vermil)">' + esc(t().offline) + '</span>') +
        '</div>' +
        '<svg class="keyhole" width="15" height="19" viewBox="0 0 15 19" aria-hidden="true">' +
          '<circle cx="7.5" cy="6.4" r="4.2" fill="none" stroke="currentColor" stroke-width="1.3"/>' +
          '<path d="M5.9 10.2 4.6 17.6h5.8L9.1 10.2" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>' +
        '</svg>' +
      '</div>';
  }

  function renderBoard() {
    board.innerHTML = DEVICES.map(plateHTML).join('');
    if (st.phase === 'awaiting') hangTag();
  }

  function hangTag() {
    var j = job();
    if (!j) return;
    var p = $('#p-' + j.host);
    if (!p) return;

    p.insertAdjacentHTML('beforeend',
      '<svg class="shackle" width="26" height="30" viewBox="0 0 26 30" aria-hidden="true">' +
        '<path d="M13 2v9" stroke="currentColor" stroke-width="2.2"/>' +
        '<path d="M7.4 20a5.6 5.6 0 1 1 11.2 0v6.4H7.4z" fill="none" stroke="currentColor" stroke-width="2.2"/>' +
      '</svg>');

    var el = document.createElement('div');
    el.className = 'tag';
    el.id = 'tag';
    el.tabIndex = 0;
    el.setAttribute('role', 'button');
    el.innerHTML =
      '<span class="hole"></span>' +
      '<h3>' + esc(j.pair ? t().pairTitle : t().locked) + '</h3>' +
      '<div class="from">' + esc(j.by) + '</div>' +
      '<div class="cmd">' + esc(j.pair ? j.fp : j.short) + '</div>';
    p.appendChild(el);

    el.addEventListener('click', function () { act('y'); });
    el.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); act('y'); }
    });

    decide.hidden = false;
    $$('button', decide).forEach(function (b) { b.disabled = false; });
    hint.textContent = t().hintWait;
    npend.textContent = '1';
    burst(1);
  }

  /* ── 凭证 ──────────────────────────────────────────────────────── */
  function credential(j, ok, scope) {
    st.creds++;
    if (st.creds === 1) creds.innerHTML = '';
    var k = t().k, d = new Date();
    var p2 = function (n) { return (n < 10 ? '0' : '') + n; };
    var stamp = d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) +
                ' ' + p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
    var row = function (label, val, cls) {
      return '<div><dt>' + esc(label) + '</dt><dd class="' + (cls || '') + '">' + esc(val) + '</dd></div>';
    };
    var el = document.createElement('dl');
    el.className = 'cred' + (ok ? '' : ' no');
    el.innerHTML = row(k.by, t().you) + row(k.date, stamp) +
                   row(k.device, j.host) + row(k.via, j.by) +
                   row(k.cmd, j.short) + row(k.scope, scope, 'verdict');
    creds.insertBefore(el, creds.firstChild);
  }

  function burst(dir) { if (window.__field) window.__field.burst(dir); }

  function stream(lines, done) {
    burst(-1);
    var i = 0;
    (function step() {
      if (i >= lines.length) { if (done) done(); return; }
      aline('', lines[i]); i++;
      setTimeout(step, 250);
    })();
  }

  /* 忠实移植 internal/policy 的 matchCommand + ruleMatchesShell(exec 分支)。
     关键语义：规则记的是**命令**，不是「这个目录里的任意命令」。
     所以签了「全局」之后换一条命令，照样要挂牌。 */
  function matchCommand(pat, cmd) {
    pat = String(pat || '').trim(); cmd = String(cmd || '').trim();
    if (!pat) return false;
    if (cmd === pat) return true;
    if (pat === '*') return true;
    if (/[;&|><`$(){}\n]/.test(cmd)) return false;      /* 近似 isSingleSimpleCommand */
    if (/ \*$/.test(pat)) return cmd.indexOf(pat.slice(0, -1)) === 0;
    return cmd.indexOf(pat + ' ') === 0;
  }

  function ruleHit(j) {
    for (var i = 0; i < st.rules.length; i++) {
      var r = st.rules[i];
      if (!matchCommand(r.pattern, j.raw)) continue;
      if (r.scope === 'global') return r;
      if (r.scope === 'dir' && r.dir === j.cwd && j.cwd) return r;
    }
    return null;
  }

  /* ── 决定 ──────────────────────────────────────────────────────── */
  function act(v) {
    if (st.phase !== 'awaiting') return;
    var j = job();
    if (!j) return;
    var tag = $('#tag');

    if (v === 'n') {
      st.phase = 'denied';
      if (tag) { tag.classList.add('denied'); $('h3', tag).textContent = t().denied; }
      credential(j, false, t().refused);
      decide.hidden = true;
      npend.textContent = '0';
      aline('no', j.by + ': ' + (j.pair ? t().deniedPair : t().deniedBy));

      setTimeout(function () {
        if (j.pair || st.retried) { st.round++; st.retried = false; nextRound(); return; }
        st.retried = true; st.phase = 'awaiting';
        renderBoard();
        typeCmd(SAFER.cmd, false, function () { aline('wait', t().waiting); });
      }, 1200);
      return;
    }

    var scope = v === 'y' ? t().once : v === 'a' ? t().here : t().global;
    if (v === 'a') st.rules.push({ scope: 'dir', dir: j.cwd, pattern: j.raw });
    if (v === 'g') st.rules.push({ scope: 'global', pattern: j.raw });

    st.phase = 'running';
    decide.hidden = true;
    npend.textContent = '0';
    if (tag) { tag.classList.add('pulled'); setTimeout(function () { tag.remove(); }, 380); }

    credential(j, true, j.pair ? t().trusted : scope);

    if (j.pair) {
      aline('', j.by + ' trusted · fingerprint pinned');
      setTimeout(function () { st.round++; st.retried = false; nextRound(); }, 1000);
      return;
    }
    stream(j.out, function () {
      setTimeout(function () { st.round++; st.retried = false; nextRound(); }, 700);
    });
  }

  function nextRound() {
    var cur = SCRIPT[st.round];
    if (!cur) { finale(); return; }

    var hit = cur.pair ? null : ruleHit(cur);
    if (hit) {
      st.phase = 'running';
      renderBoard();
      decide.hidden = false;
      $$('button', decide).forEach(function (b) { b.disabled = true; });
      hint.textContent = t().hintRule;
      typeCmd(cur.cmd, false, function () {
        aline('wait', '↳ ' + t().byRule + ': ' + hit.pattern);
        credential(cur, true, t().byRule);
        stream(cur.out, function () {
          setTimeout(function () { st.round++; nextRound(); }, 700);
        });
      });
      return;
    }

    st.phase = 'awaiting';
    renderBoard();
    typeCmd(cur.cmd, false, function () {
      if (cur.lesson && st.rules.length) aline('wait', t().lesson);
      aline('wait', t().waiting);
    });
  }

  function finale() {
    st.phase = 'done';
    aline('', '');
    aline('cmd', t().finale);
    aline('wait', st.creds + ' ' + t().signed);
    decide.innerHTML = '';
    var b = document.createElement('button');
    b.className = 'key';
    b.type = 'button';
    b.innerHTML = '<kbd>R</kbd><span>' + esc(t().replay) + '</span>';
    b.addEventListener('click', reset);
    decide.appendChild(b);
    decide.hidden = false;
  }

  function reset() { location.reload(); }

  /* ── 键盘 ──────────────────────────────────────────────────────── */
  document.addEventListener('keydown', function (e) {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    var k = (e.key || '').toLowerCase();
    if (k === 'r' && st.phase === 'done') { reset(); return; }
    if (k.length === 1 && 'yagn'.indexOf(k) > -1) { e.preventDefault(); act(k); }
  });
  decide.addEventListener('click', function (e) {
    var b = e.target.closest('[data-act]');
    if (b && !b.disabled) act(b.dataset.act);
  });

  /* ── 语言 ──────────────────────────────────────────────────────── */
  function applyLang(l) {
    lang = l;
    document.documentElement.lang = l === 'zh' ? 'zh-CN' : 'en';
    $('#lang').textContent = l === 'en' ? '中文' : 'EN';
    $$('[data-en]').forEach(function (el) {
      var v = el.getAttribute('data-' + l);
      if (v != null) el.innerHTML = v;
    });
    if (st.creds === 0) creds.innerHTML = '<p class="empty">' + esc(t().nothing) + '</p>';
    boot(true);
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }
  $('#lang').addEventListener('click', function () { applyLang(lang === 'en' ? 'zh' : 'en'); });

  /* ── 启动：首屏必须一次画满，不靠动画才可见 ─────────────────────── */
  function boot(instant) {
    st = { phase: 'awaiting', round: 0, rules: [], creds: 0, retried: false };
    agent.innerHTML = '';
    creds.innerHTML = '<p class="empty">' + esc(t().nothing) + '</p>';
    renderBoard();
    typeCmd(SCRIPT[0].cmd, instant !== false, function () { aline('wait', t().waiting); });
  }

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  if (saved === 'zh' || (!saved && /^zh/i.test(navigator.language || ''))) applyLang('zh');
  else boot(true);

  var copyBtn = $('#copy');
  if (copyBtn) copyBtn.addEventListener('click', function () {
    var txt = $('#installcmd').textContent.trim();
    var ok = function () {
      copyBtn.classList.add('done');
      copyBtn.textContent = lang === 'en' ? 'Copied' : '已复制';
      setTimeout(function () {
        copyBtn.classList.remove('done');
        copyBtn.textContent = lang === 'en' ? 'Copy' : '复制';
      }, 1600);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(txt).then(ok, function () {});
    } else {
      var ta = document.createElement('textarea');
      ta.value = txt; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); ok(); } catch (_) {}
      document.body.removeChild(ta);
    }
  });

  window.__demo = {
    state: function () {
      return { phase: st.phase, round: st.round, rules: st.rules.slice(),
               creds: st.creds, retried: st.retried, lang: lang };
    },
    act: act, reset: reset
  };
})();
