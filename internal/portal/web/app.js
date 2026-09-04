/* wanctl 门户
   视觉真源 docs/design/DESIGN.md，这一轮的账本 docs/design/PORTAL-HANDOFF.md。

   后端契约不动：所有 /api/* 调用与旧版逐一对应，只多了一个 /api/pending
   （跨设备聚合待审批）。 */
(function () {
  'use strict';

  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  function esc(s) {
    return ('' + (s == null ? '' : s)).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  /* ── 文案 ────────────────────────────────────────────────────────────
     静态文案挂在 HTML 的 data-en / data-zh 上；这里只放 JS 渲染出来的。
     与官网 site/assets/app.js 同一套机制。 */
  var T = {
    en: {
      never: 'never', justNow: 'just now', min: 'm ago', hour: 'h ago', day: 'd ago',
      offline: 'offline', online: 'online', waiting: 'waiting', shared: 'shared by',
      noDevices: 'No devices yet.',
      firstDevice: 'Install wanctl on a machine, then run it there to bring it in.',
      install: 'Install',
      loading: 'Loading…',
      nothingWaiting: 'Nothing is waiting for you.',
      noneOnDevice: 'Nothing waiting on this device.',
      cantReach: 'Cannot reach this device — it may be offline.',
      kind: { exec: 'command', read: 'read a file', write: 'write a file' },
      wantsTo: function (d) { return d; },
      from: 'from', cwd: 'in',
      kName: 'name', kFP: 'fingerprint', kDevice: 'device', kController: 'controller',
      once: 'Allow once', dir: 'Always here', global: 'Always anywhere', refuse: 'Refuse',
      scopeDir: function (p) { return p ? '(' + p + ')' : ''; },
      scopeGlobal: 'every directory',
      pairing: 'wants to be trusted',
      pairMsg: 'This controller is connecting for the first time. Trust it and it can drive this device.',
      trustIt: 'Trust it',
      idChanged: 'This device is not the one we saw before',
      idBody: 'The fingerprint it reports now does not match the one the portal pinned. Reinstalling it, or clearing its config, does this. If you did neither, treat it as an impersonation and investigate.',
      idPinned: 'pinned', idOffered: 'now offering',
      idAccept: 'I reinstalled it — accept',
      idAcceptT: 'Accept the new identity?',
      idAcceptM: 'Run wanctl on that machine and check the fingerprint it prints matches the new one here, then confirm.',
      roConsole: 'Only the owner can answer this.',
      roBanner: function (o) { return 'Shared with you by ' + o + '. Approvals and activity belong to the owner; use the CLI or MCP for what you were granted.'; },
      roGuard: 'Shared device, read-only. Ask the owner.',
      allowed: 'Allowed', refused: 'Refused',
      trusted: 'Trusted', refusedPair: 'Refused',
      webConsole: 'this portal', revoke: 'Revoke', remove: 'Remove', del: 'Delete',
      noTrusted: 'No controller is trusted yet.',
      noRules: 'No rules — every request asks you.',
      noLog: 'No activity yet.',
      noTokens: 'No tokens yet.',
      noInvites: 'No invites yet.',
      noFriends: 'No friends yet.',
      noACL: 'Nothing shared yet.',
      noAudit: 'No relay events yet.',
      addRule: 'Add a rule', ruleKind: 'Kind', rulePattern: 'Command or directory',
      rulePlaceholder: 'echo *   or   /data',
      issue: 'Issue', label: 'Label', labelPlaceholder: 'laptop, ci, …',
      expiry: 'Expires in days (0 = never)',
      newInvite: 'New invite', inviteLogin: 'GitHub login (optional)',
      invitePlaceholder: 'fill in to pre-register; leave empty for a one-time code',
      inviteCode: 'code', invitePre: 'pre-registered', invitePending: 'unused', inviteUsed: 'used by',
      addFriend: 'Add friend', friendNS: "Their username (namespace)", send: 'Send request',
      friendYes: 'friend', friendIn: 'wants to add you', friendOut: 'waiting for them',
      accept: 'Accept', decline: 'Decline', withdraw: 'Withdraw',
      shareDevice: 'Share a device', shareGrantee: 'Grant to', sharePerms: 'Permissions', share: 'Share',
      never2: 'never', revoked: 'revoked', active: 'active',
      copied: 'Copied', copy: 'Copy',
      saved: 'Saved', cleared: 'Cleared', sent: 'Test delivered',
      notifyOff: 'No webhook configured', notifyOn: function (f) { return 'Delivering to ' + f; },
      noAttempts: 'No delivery attempted yet.',
      lastOK: function (w) { return 'Last delivery succeeded (' + w + ')'; },
      lastFail: function (w, e) { return 'Last delivery failed (' + w + '): ' + e; },
      lanOn: function (r) { return 'Direct LAN link — connected to ' + r; },
      lanTrying: function (r) { return 'Direct LAN link — on, trying ' + r; },
      lanOff: 'Direct LAN link — off, public relay only',
      larkOn: function (m) { return 'Feishu approvals — on, cards go to ' + m; },
      larkOff: 'Feishu approvals — off',
      notifyDevOn: 'Webhook notifications — on', notifyDevOff: 'Webhook notifications — off',
      removeDevT: 'Remove this device?',
      removeDevM: function (d) { return d + ' leaves your namespace. Running wanctl on that machine again brings it back.'; },
      revokeTrustT: 'Revoke trust?',
      revokeTrustM: 'This controller will not be able to drive the device until it pairs again.',
      removeFriendT: 'Remove this friend?',
      removeFriendM: 'Every device share between you, in both directions, is taken back.',
      revokeInviteT: 'Revoke this invite?', revokeInviteM: 'It can no longer be redeemed.',
      clearNotifyT: 'Clear the webhook?', clearNotifyM: 'The account stops sending notifications. Per-device switches are kept.',
      bypassT: 'Allow everything on this device?',
      bypassM: 'Every command and file operation runs without asking you. Only for machines you trust and have isolated. The whole period is recorded in the audit log.',
      confirm: 'Confirm', current: 'current',
      failed: function (e) { return 'Failed: ' + e; },
      notSignedIn: 'Not signed in',
      dlLatest: 'latest', dlSource: 'Released on GitHub',
      dlPhone: 'Android phone or tablet',
      dlPhoneSub: 'Install the app — no terminal, no Termux. Open it, sign in, then switch on “Enable wanctl”.',
      dlApk: 'Download the APK',
      dlApkAlt: 'Older or x86 devices:',
      dlDesktop: 'Computers: one line',
      dlDesktopSub: 'The installer carries the release public key and checks the signature, size and SHA-256 before it writes anything.',
      dlJoin: 'Point it at this instance (once; change it later with wanctl config)',
      dlAll: 'All builds',
      pairOwnerOnly: function (o) { return 'This device is shared with you by ' + o + '. Only the owner can add a controller to its allow-list — forward this link to them.'; },
      ok: 'Got it', trustedNow: 'Trusted. Ask the AI to run the command again.'
    },
    zh: {
      never: '从未', justNow: '刚刚', min: ' 分钟前', hour: ' 小时前', day: ' 天前',
      offline: '离线', online: '在线', waiting: '在等你', shared: '来自',
      noDevices: '还没有设备。',
      firstDevice: '在一台机器上装好 wanctl，然后在那儿运行它，它就进来了。',
      install: '下载安装',
      loading: '加载中…',
      nothingWaiting: '没有人在等你。',
      noneOnDevice: '这台设备没有待审批。',
      cantReach: '连不上这台设备，它可能不在线。',
      kind: { exec: '命令', read: '读取文件', write: '写入文件' },
      wantsTo: function (d) { return d; },
      from: '来自', cwd: '工作目录',
      kName: '名称', kFP: '指纹', kDevice: '设备', kController: '控制端',
      once: '允许一次', dir: '这个目录一直允许', global: '所有目录一直允许', refuse: '拒绝',
      scopeDir: function (p) { return p ? '（' + p + '）' : ''; },
      scopeGlobal: '全部目录',
      pairing: '想被信任',
      pairMsg: '这个控制端第一次连接。信任之后它就能控制这台设备。',
      trustIt: '信任它',
      idChanged: '这台设备跟上次不是同一个了',
      idBody: '它现在报出的指纹，和门户上次记住的对不上。重装过它、或者清过它的配置目录，都会这样；如果你没做过这些，就要当成被冒充来查。',
      idPinned: '门户记住的', idOffered: '现在报出的',
      idAccept: '是我重装的，接受新身份',
      idAcceptT: '接受新的设备身份？',
      idAcceptM: '先到那台机器上跑一次 wanctl，核对它打印的指纹跟这里的新指纹一致，再确认。',
      roConsole: '只有设备主人能回答。',
      roBanner: function (o) { return o + ' 把这台设备共享给你。审批和活动属于设备主人；你被授予的操作请直接用 CLI 或 MCP。'; },
      roGuard: '共享设备只读，请联系设备主人。',
      allowed: '已允许', refused: '已拒绝',
      trusted: '已信任', refusedPair: '已拒绝',
      webConsole: '本门户', revoke: '撤销', remove: '解除', del: '删除',
      noTrusted: '还没有信任任何控制端。',
      noRules: '还没有规则，每条请求都会问你。',
      noLog: '还没有活动记录。',
      noTokens: '还没有令牌。',
      noInvites: '还没有邀请。',
      noFriends: '还没有好友。',
      noACL: '还没有共享授权。',
      noAudit: '还没有中继事件。',
      addRule: '加一条规则', ruleKind: '类型', rulePattern: '命令或目录',
      rulePlaceholder: 'echo *   或   /data',
      issue: '签发', label: '标签', labelPlaceholder: 'laptop、ci…',
      expiry: '多少天后过期（0 = 永不）',
      newInvite: '新建邀请', inviteLogin: 'GitHub 用户名（可选）',
      invitePlaceholder: '填了就预录对方；留空则生成一次性邀请码',
      inviteCode: '邀请码', invitePre: '已预录', invitePending: '待使用', inviteUsed: '已用于',
      addFriend: '加好友', friendNS: '对方的用户名（命名空间）', send: '发送请求',
      friendYes: '好友', friendIn: '想加你为好友', friendOut: '等待对方接受',
      accept: '接受', decline: '拒绝', withdraw: '撤回',
      shareDevice: '共享一台设备', shareGrantee: '授权给', sharePerms: '权限', share: '授权',
      never2: '永不', revoked: '已吊销', active: '有效',
      copied: '已复制', copy: '复制',
      saved: '已保存', cleared: '已清除', sent: '测试通知已送达',
      notifyOff: '未配置 webhook', notifyOn: function (f) { return '投递到 ' + f; },
      noAttempts: '还没有投递尝试。',
      lastOK: function (w) { return '最近一次投递成功（' + w + '）'; },
      lastFail: function (w, e) { return '最近一次投递失败（' + w + '）：' + e; },
      lanOn: function (r) { return '内网直连 —— 已连上 ' + r; },
      lanTrying: function (r) { return '内网直连 —— 已开启，正在连 ' + r; },
      lanOff: '内网直连 —— 已关闭，仅走公网中继',
      larkOn: function (m) { return '飞书审批 —— 已开启，卡片推给 ' + m; },
      larkOff: '飞书审批 —— 已关闭',
      notifyDevOn: 'Webhook 通知 —— 已开启', notifyDevOff: 'Webhook 通知 —— 已关闭',
      removeDevT: '解除这台设备？',
      removeDevM: function (d) { return d + ' 会离开你的命名空间。在那台机器上重新运行 wanctl 可再次纳管。'; },
      revokeTrustT: '撤销信任？',
      revokeTrustM: '这个控制端将无法再控制本设备，除非重新配对。',
      removeFriendT: '删除这个好友？',
      removeFriendM: '你们之间双向的设备共享会全部收回。',
      revokeInviteT: '撤销这个邀请？', revokeInviteM: '它将不能再被兑换。',
      clearNotifyT: '清除 webhook 配置？', clearNotifyM: '账号将停止发送通知，每台设备的开关保留。',
      bypassT: '这台设备全放行？',
      bypassM: '所有命令和文件操作都会自动放行、不再问你。只给可信且已隔离的机器用。这段时间会一直记进审计。',
      confirm: '确定', current: '当前版本',
      failed: function (e) { return '失败：' + e; },
      notSignedIn: '未登录',
      dlLatest: '最新', dlSource: '发布源 GitHub',
      dlPhone: '安卓手机 / 平板',
      dlPhoneSub: '装这个 App 就行，不需要终端、不需要 Termux。装好打开，点登录，再打开「启用 wanctl」。',
      dlApk: '下载 APK',
      dlApkAlt: '老机器或 x86 设备：',
      dlDesktop: '电脑：一行装好',
      dlDesktopSub: '安装脚本自带发布公钥，落盘前核对签名、大小和 SHA-256，核不上就不装。',
      dlJoin: '装完接入这个实例（一次配置，之后 wanctl config 随时改）',
      dlAll: '全部构建产物',
      pairOwnerOnly: function (o) { return '这台设备是 ' + o + ' 共享给你的。把新控制端加进白名单只能由设备主人来做 —— 请把这条链接原样转给 ta。'; },
      ok: '知道了', trustedNow: '已信任。回去让 AI 再跑一次命令。'
    }
  };

  var lang = 'en';
  var t = function () { return T[lang]; };

  function applyLang(l) {
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
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
    repaint();
  }

  /* ── 时间 ────────────────────────────────────────────────────────── */
  function ago(v) {
    if (!v) return t().never;
    var d = new Date(v);
    if (isNaN(d)) return esc(v);
    var s = (Date.now() - d.getTime()) / 1000;
    if (s < 60) return t().justNow;
    if (s < 3600) return Math.floor(s / 60) + t().min;
    if (s < 86400) return Math.floor(s / 3600) + t().hour;
    if (s < 2592000) return Math.floor(s / 86400) + t().day;
    return d.toLocaleDateString();
  }
  /* 绝对时间统一成 2026-08-26 16:14。toLocaleString 会按浏览器地区给出
     8/26/2026, 4:14:35 PM 那样的串：秒是噪声，日月顺序还随地区变，
     而这些数字常常要跟日志或别人的截图对齐。 */
  function pad(n) { return ('0' + n).slice(-2); }
  function fmt(v) {
    if (!v) return '—';
    var d = new Date(v);
    if (isNaN(d)) return esc(v);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }
  function day(v) {
    if (!v) return '—';
    var d = new Date(v);
    return isNaN(d) ? esc(v) : d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }
  function clock(v) {
    var d = new Date(v);
    if (!v || isNaN(d)) return '';
    var s = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    return Math.floor(s / 60) + ':' + ('0' + (s % 60)).slice(-2);
  }

  /* ── HTTP ────────────────────────────────────────────────────────── */
  function httpErr(status, text) {
    var e = new Error(text || status);
    e.status = status;
    try { e.data = JSON.parse(text); } catch (_) {}
    return e;
  }
  function jget(u) {
    return fetch(u).then(function (r) {
      if (!r.ok) return r.text().then(function (x) { throw httpErr(r.status, x); });
      return r.json();
    });
  }
  function csrf() {
    var p = document.cookie.split('; ').find(function (x) { return x.indexOf('wanctl_csrf=') === 0; });
    return p ? decodeURIComponent(p.slice(p.indexOf('=') + 1)) : '';
  }
  function jpost(u, b) {
    return fetch(u, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf() },
      body: JSON.stringify(b || {})
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (x) { throw httpErr(r.status, x); });
      return r;
    });
  }

  /* ── toast ──────────────────────────────────────────────────────────
     反馈只有这一套。以前顶上还挂着一条 6 秒的错误条，两套机制做同一件事。 */
  var toastT;
  function toast(msg, bad) {
    var el = $('#toast');
    el.className = 'toast' + (bad ? ' no' : '');
    el.textContent = msg;
    requestAnimationFrame(function () { el.classList.add('show'); });
    clearTimeout(toastT);
    toastT = setTimeout(function () { el.classList.remove('show'); }, bad ? 4200 : 2200);
  }
  function oops(e) { toast(t().failed(e && e.message ? e.message : e), true); }

  /* ── 确认 / 表单 ─────────────────────────────────────────────────── */
  function confirmBox(title, msg, okLabel) {
    return new Promise(function (res) {
      var ov = $('#ask');
      $('#askT').textContent = title;
      $('#askM').textContent = msg;
      $('#askYes').textContent = okLabel || t().confirm;
      ov.classList.add('show');
      function done(v) {
        ov.classList.remove('show');
        $('#askYes').onclick = $('#askNo').onclick = ov.onclick = null;
        document.onkeydown = null;
        res(v);
      }
      $('#askYes').onclick = function () { done(true); };
      $('#askNo').onclick = function () { done(false); };
      ov.onclick = function (e) { if (e.target === ov) done(false); };
      document.onkeydown = function (e) { if (e.key === 'Escape') done(false); };
    });
  }
  function formBox(title, okLabel, fields) {
    return new Promise(function (res) {
      var ov = $('#form');
      $('#formT').textContent = title;
      $('#formYes').textContent = okLabel;
      $('#formF').innerHTML = fields.map(function (f) {
        var input = f.options
          ? '<select data-k="' + f.key + '">' + f.options.map(function (o) {
              return '<option value="' + esc(o) + '">' + esc(o) + '</option>';
            }).join('') + '</select>'
          : '<input data-k="' + f.key + '" type="' + (f.type || 'text') + '" placeholder="' + esc(f.hint || '') + '"' + (f.min != null ? ' min="' + f.min + '"' : '') + '>';
        return '<div class="field"><label>' + esc(f.label) + '</label>' + input + '</div>';
      }).join('');
      ov.classList.add('show');
      var first = $('input,select', $('#formF'));
      if (first) setTimeout(function () { first.focus(); }, 60);
      function done(v) {
        ov.classList.remove('show');
        $('#formYes').onclick = $('#formNo').onclick = ov.onclick = null;
        document.onkeydown = null;
        res(v);
      }
      $('#formYes').onclick = function () {
        var v = {};
        $$('[data-k]', $('#formF')).forEach(function (el) { v[el.dataset.k] = ('' + el.value).trim(); });
        done(v);
      };
      $('#formNo').onclick = function () { done(null); };
      ov.onclick = function (e) { if (e.target === ov) done(null); };
      document.onkeydown = function (e) { if (e.key === 'Escape') done(null); };
    });
  }

  /* ── 命令排版 ────────────────────────────────────────────────────────
     浏览器允许在连字符后折行，`--epochs` 会被断成 `--` / `epochs`，读起来
     像两个参数。每个 token 各自 nowrap；只有超过 16 字的长 token 才放开
     内部折行，否则一条长路径会把整块撑出去。 */
  var DANGER = /^(rm|rmdir|dd|mkfs\S*|shutdown|reboot|halt|poweroff|kill|killall|pkill|chmod|chown|chgrp|truncate|format|del|fdisk|wipefs)$/;
  function cmdHTML(c) {
    c = '' + (c || '');
    if (!c) return '<span class="dim">—</span>';
    return c.split(/(\s+)/).map(function (tok, i) {
      if (/^\s+$/.test(tok)) return tok;
      var cls = 'tok' + (tok.length > 16 ? ' split' : '');
      if (i === 0 && DANGER.test(tok)) cls += ' rm';
      return '<span class="' + cls + '">' + esc(tok) + '</span>';
    }).join('');
  }

  /* ── 待审批：门户的主体 ──────────────────────────────────────────────
     一个组件，两处挂载：主屏顶部（跨设备聚合）与设备下钻的待审批页
     （过滤到这一台）。 */
  function askCard(p, dev, readOnly, hideHost) {
    var isExec = p.kind === 'exec';
    var kv = '<dt>' + esc(t().from) + '</dt><dd>' + esc((p.peer || '—').slice(0, 40)) + '</dd>';
    if (p.cwd) kv += '<dt>' + esc(t().cwd) + '</dt><dd>' + esc(p.cwd) + '</dd>';
    // 层级按后果排。三个次级动作等重，作用域写在标签旁边让后果看得见。
    var acts = readOnly
      ? '<span class="dim">' + esc(t().roConsole) + '</span>'
      : '<button class="btn" data-v="y">' + esc(t().once) + '</button>' +
        '<button class="btn soft" data-v="a">' + esc(t().dir) +
          '<span class="scope">' + esc(t().scopeDir(p.cwd)) + '</span></button>' +
        '<button class="btn soft" data-v="g">' + esc(t().global) + '</button>' +
        '<button class="btn soft spacer" data-v="n">' + esc(t().refuse) + '</button>';
    return '<div class="ask" data-pid="' + esc(p.id) + '" data-dev="' + esc(dev) + '">' +
      '<div class="head">' + (hideHost ? '' : '<span class="host">' + esc(dev) + '</span>') +
        '<span class="what">' + esc(t().kind[p.kind] || p.kind) + '</span>' +
        '<span class="wait" data-since="' + esc(p.created || '') + '">' + esc(clock(p.created)) + '</span></div>' +
      '<div class="body"><p class="cmd">' + (isExec ? cmdHTML(p.cmd) : esc(p.path || '—')) + '</p>' +
        '<dl class="kv">' + kv + '</dl></div>' +
      '<div class="acts">' + acts + '</div></div>';
  }

  function pairCard(pr, dev, readOnly, hideHost) {
    var kv = '';
    if (pr.name) kv += '<dt>' + esc(t().kName) + '</dt><dd>' + esc(pr.name) + '</dd>';
    kv += '<dt>' + esc(t().kFP) + '</dt><dd>' + esc(pr.fp || '') + '</dd>';
    var acts = readOnly
      ? '<span class="dim">' + esc(t().roConsole) + '</span>'
      : '<button class="btn" data-pv="y">' + esc(t().trustIt) + '</button>' +
        '<button class="btn soft spacer" data-pv="n">' + esc(t().refuse) + '</button>';
    return '<div class="ask" data-fp="' + esc(pr.fp) + '" data-dev="' + esc(dev) + '">' +
      '<div class="head"><span class="host">' + esc(pr.label || pr.name || '—') + '</span>' +
        '<span class="what">' + esc(t().pairing) + (hideHost ? '' : ' · ' + esc(dev)) + '</span>' +
        '<span class="wait" data-since="' + esc(pr.created || '') + '">' + esc(clock(pr.created)) + '</span></div>' +
      '<div class="body"><p class="cmd" style="font-family:inherit">' + esc(t().pairMsg) + '</p>' +
        '<dl class="kv">' + kv + '</dl></div>' +
      '<div class="acts">' + acts + '</div></div>';
  }

  function bindAsks(root) {
    $$('.ask', root).forEach(function (el) {
      $$('.btn[data-v]', el).forEach(function (b) {
        b.onclick = function () { decide(el.dataset.dev, el.dataset.pid, b.dataset.v, el); };
      });
      $$('.btn[data-pv]', el).forEach(function (b) {
        b.onclick = function () { pairDecide(el.dataset.dev, el.dataset.fp, b.dataset.pv, el); };
      });
    });
  }

  function decide(dev, id, v, el) {
    if (roGuard(dev)) return;
    jpost('/api/devices/decide', { device: dev, id: id, verdict: v }).then(function () {
      if (el) el.classList.add('gone');
      toast(v === 'n' ? t().refused : t().allowed, v === 'n');
      setTimeout(refreshAsks, 340);
    }).catch(oops);
  }
  function pairDecide(dev, fp, v, el) {
    if (roGuard(dev)) return;
    jpost('/api/devices/pair', { device: dev, fp: fp, verdict: v }).then(function () {
      if (el) el.classList.add('gone');
      toast(v === 'n' ? t().refusedPair : t().trusted, v === 'n');
      setTimeout(refreshAsks, 340);
    }).catch(oops);
  }

  /* 秒表每秒跳一次。它是真的 —— `wanctl exec` 就阻塞在那儿等人回答。 */
  setInterval(function () {
    $$('.ask .wait[data-since]').forEach(function (el) {
      var s = el.getAttribute('data-since');
      if (s) el.textContent = clock(s);
    });
  }, 1000);

  /* ── 设备 ────────────────────────────────────────────────────────── */
  var devMeta = {};   // name -> {shared, owner}；自有设备优先（与后端 requireDevice 一致）
  var devices = [];
  var me = null;

  function roGuard(dev) {
    var m = devMeta[dev];
    if (m && m.shared) { toast(t().roGuard, true); return true; }
    return false;
  }

  function noteDevices(xs) {
    devices = xs || [];
    devices.forEach(function (x) {
      var sh = x.shared === true;
      if (!(x.name in devMeta) || !sh) devMeta[x.name] = { shared: sh, owner: x.owner || '' };
    });
  }

  var waitCount = {};   // device -> 待审批条数，用于设备卡上的角标

  function renderDevices() {
    var xs = devices;
    $('#onboard').hidden = xs.length > 0;
    if (!xs.length) {
      $('#onboard').innerHTML = '<div class="onboard"><p>' + esc(t().firstDevice) + '</p>' +
        '<button class="btn" id="obGo">' + esc(t().install) + '</button></div>';
      $('#obGo').onclick = function () { go('settings/downloads'); };
      $('#grid').innerHTML = '';
      return;
    }
    $('#grid').innerHTML = xs.map(function (x) {
      var on = x.online;
      var n = waitCount[x.name] || 0;
      var name = x.ambiguous ? (x.name + ' · ' + (x.owner || '')) : x.name;
      return '<button class="dev' + (on ? '' : ' offline') + '" data-dev="' + esc(x.name) + '">' +
        '<span class="nm"><span class="dot' + (on ? '' : ' off') + '"></span>' + esc(name) +
          (n ? '<span class="badge waiting">' + n + ' ' + esc(t().waiting) + '</span>' : '') + '</span>' +
        // 「最近」只在离线时出现：在线设备这一行永远是「刚刚」，等于空转。
        (on ? '' : '<span class="seen">' + esc(t().offline) + ' · ' + esc(ago(x.last_seen)) + '</span>') +
        (x.shared ? '<span class="shared">' + esc(t().shared) + ' ' + esc(x.owner || '—') +
          (x.perms ? ' · ' + esc(x.perms) : '') + '</span>' : '') +
        '</button>';
    }).join('');
    $$('#grid .dev').forEach(function (c) {
      c.onclick = function () { openDevice(c.dataset.dev); };
    });
  }

  function loadDevices() {
    return jget('/api/devices').then(function (d) {
      noteDevices(d.devices);
      renderDevices();
    }).catch(function (e) {
      oops(e);
      $('#grid').innerHTML = '<div class="blank">' + esc(t().cantReach) + '</div>';
    });
  }

  /* ── 跨设备聚合待审批 ────────────────────────────────────────────────
     打开门户第一眼该是「有没有人在等我」，不是「我有几台机器」。
     服务端做扇出：门户本来就为每台在线设备维持着 deviceConn，
     从浏览器打 N 个请求是白费一轮往返。 */
  function refreshAsks() {
    if (cur) return renderConsole();
    return jget('/api/pending').then(function (d) {
      var items = d.items || [];
      waitCount = {};
      items.forEach(function (i) { waitCount[i.device] = (waitCount[i.device] || 0) + 1; });
      $('#asks').innerHTML = items.map(function (i) {
        var ro = (devMeta[i.device] || {}).shared;
        return i.fp ? pairCard(i, i.device, ro) : askCard(i, i.device, ro);
      }).join('');
      bindAsks($('#asks'));
      renderDevices();
    }).catch(function () { /* 瞬时失败下轮再来，不要在页面上闪一条错误 */ });
  }

  /* ── 设备下钻 ────────────────────────────────────────────────────── */
  var cur = null, pollGen = 0, curMode = 'normal';

  function openDevice(name) {
    cur = name;
    $('#dName').textContent = name;
    var m = devMeta[name] || {};
    $('#dShared').hidden = !m.shared;
    if (m.shared) $('#dShared').textContent = t().roBanner(m.owner || '—');
    $('#dGear').hidden = !!m.shared;
    $('#dMode').disabled = !!m.shared;
    var d = devices.filter(function (x) { return x.name === name; })[0];
    $('#dFp').textContent = d ? (d.fingerprint || '') : '';
    showView('device');
    selTab('asks');
    var h = '#device/' + encodeURIComponent(name);
    if (location.hash !== h) history.pushState(null, '', h);
    if (m.shared) { $('#dAsks').innerHTML = ''; $('#dBlank').hidden = false; $('#dBlank').textContent = t().roConsole; return; }
    $('#dAsks').innerHTML = '';
    $('#dBlank').hidden = false;
    $('#dBlank').textContent = t().loading;
    renderConsole();
    loadDeviceSettings(name);
    poll(name);
  }
  function closeDevice() { pollGen++; cur = null; }

  /* 长轮询快照，而不是依赖异步推送 —— 推送穿不过中继那一跳的 HTTP 长轮询。
     换设备或离开时用世代号停掉旧循环。 */
  function poll(name) {
    var gen = ++pollGen;
    (function loop() {
      if (cur !== name || gen !== pollGen) return;
      setTimeout(function () {
        if (cur !== name || gen !== pollGen) return;
        jget('/api/devices/console?device=' + encodeURIComponent(name)).then(function (st) {
          if (cur !== name || gen !== pollGen) return;
          applyState(st);
          loop();
        }).catch(function (e) {
          if (cur !== name || gen !== pollGen) return;
          // 身份变了是终局，交给人决定：显示卡片并停掉轮询，
          // 而不是每 3 秒在一个过期视图后面重复失败。
          if (e.data && e.data.error === 'device_identity_changed') return renderIdentityChanged(e.data);
          loop();
        });
      }, 3000);
    })();
  }

  function renderConsole() {
    if (!cur) return Promise.resolve();
    return jget('/api/devices/console?device=' + encodeURIComponent(cur))
      .then(applyState)
      .catch(function (e) {
        if (e.data && e.data.error === 'device_identity_changed') return renderIdentityChanged(e.data);
        $('#dAsks').innerHTML = '';
        $('#dBlank').hidden = false;
        $('#dBlank').textContent = t().cantReach;
      });
  }

  var lastState = null;
  function applyState(st) {
    if (!st || !st.mode) return;
    lastState = st;
    curMode = st.mode;
    $('#dMode').value = st.mode;
    $('#dMode').className = 'modesel' + (st.mode === 'bypass' ? ' bypass' : '');

    var pend = st.pending || [], pairs = st.pending_pairings || [];
    var total = pend.length + pairs.length;
    $('#nWait').hidden = !total;
    $('#nWait').textContent = total;
    var ro = (devMeta[cur] || {}).shared;
    $('#dAsks').innerHTML =
      pairs.map(function (p) { return pairCard(p, cur, ro, true); }).join('') +
      pend.map(function (p) { return askCard(p, cur, ro, true); }).join('');
    bindAsks($('#dAsks'));
    $('#dBlank').hidden = total > 0;
    $('#dBlank').textContent = t().noneOnDevice;

    var rules = st.rules || [];
    $('#rules').innerHTML = rules.length ? rules.map(function (r, i) {
      return '<tr><td>' + esc(r.kind) + '</td><td class="mono">' + esc(r.pattern) + '</td>' +
        '<td>' + (r.scope === 'dir' ? esc(r.dir || 'dir') : 'global') + '</td>' +
        '<td><button class="act danger" data-i="' + i + '">' + esc(t().del) + '</button></td></tr>';
    }).join('') : '<tr><td colspan="4" class="tempty">' + esc(t().noRules) + '</td></tr>';
    $$('#rules .act').forEach(function (b) {
      b.onclick = function () {
        if (roGuard(cur)) return;
        jpost('/api/devices/rules', { device: cur, op: 'rm', index: +b.dataset.i })
          .then(renderConsole).catch(oops);
      };
    });

    var tr = st.trusted || [];
    $('#trusted').innerHTML = tr.length ? tr.map(function (x) {
      var isPortal = x.name === 'portal';
      return '<tr><td><b>' + esc(x.label || x.name || '—') + '</b>' +
        (x.name && x.label ? '<span class="sub">' + esc(x.name) + '</span>' : '') + '</td>' +
        '<td class="mono">' + esc((x.fp || '').slice(0, 30)) + '</td>' +
        '<td>' + (x.last_seen ? esc(ago(x.last_seen)) : '—') + '</td>' +
        '<td>' + (isPortal ? '<span class="dim">' + esc(t().webConsole) + '</span>'
          : '<button class="act danger" data-fp="' + esc(x.fp) + '">' + esc(t().revoke) + '</button>') + '</td></tr>';
    }).join('') : '<tr><td colspan="4" class="tempty">' + esc(t().noTrusted) + '</td></tr>';
    $$('#trusted .act').forEach(function (b) {
      b.onclick = function () {
        if (roGuard(cur)) return;
        confirmBox(t().revokeTrustT, t().revokeTrustM, t().revoke).then(function (ok) {
          if (!ok) return;
          jpost('/api/devices/untrust', { device: cur, fp: b.dataset.fp })
            .then(function () { toast(t().revoke); return renderConsole(); }).catch(oops);
        });
      };
    });

    paintLan(st.lan);
  }

  function renderIdentityChanged(d) {
    var ro = (devMeta[cur] || {}).shared;
    $('#dBlank').hidden = true;
    $('#dAsks').innerHTML = '<div class="ask">' +
      '<div class="head"><span class="host no">' + esc(t().idChanged) + '</span></div>' +
      '<div class="body"><p style="margin:0 0 16px">' + esc(t().idBody) + '</p><dl class="kv">' +
        '<dt>' + esc(t().idPinned) + '</dt><dd>' + esc(d.pinned || '') + '</dd>' +
        '<dt>' + esc(t().idOffered) + '</dt><dd class="no">' + esc(d.offered || '') + '</dd>' +
      '</dl></div>' +
      '<div class="acts">' + (ro ? '<span class="dim">' + esc(t().roConsole) + '</span>'
        : '<button class="btn" id="idOK">' + esc(t().idAccept) + '</button>') + '</div></div>';
    var b = $('#idOK');
    if (b) b.onclick = function () {
      confirmBox(t().idAcceptT, t().idAcceptM, t().confirm).then(function (ok) {
        if (!ok) return;
        jpost('/api/devices/identity/accept', { device: cur, fingerprint: d.offered }).then(function () {
          toast(t().saved);
          var n = cur; renderConsole(); poll(n);
        }).catch(oops);
      });
    };
  }

  function selTab(name) {
    $$('.tab').forEach(function (x) { x.classList.toggle('on', x.dataset.tab === name); });
    $$('.pane').forEach(function (p) { p.classList.toggle('show', p.dataset.pane === name); });
    if (name === 'log') loadLog();
  }
  $$('.tab').forEach(function (x) { x.onclick = function () { selTab(x.dataset.tab); }; });
  $('#back').onclick = function () { go('devices'); };

  $('#dMode').onchange = function () {
    if (roGuard(cur)) { $('#dMode').value = curMode; return; }
    var next = $('#dMode').value;
    var apply = function () {
      jpost('/api/devices/mode', { device: cur, mode: next }).then(renderConsole).catch(function (e) {
        $('#dMode').value = curMode; oops(e);
      });
    };
    if (next === 'bypass') {
      confirmBox(t().bypassT, t().bypassM, t().confirm).then(function (ok) {
        if (ok) apply(); else $('#dMode').value = curMode;
      });
      return;
    }
    apply();
  };

  $('#rAdd').onclick = function () {
    if (roGuard(cur)) return;
    formBox(t().addRule, t().addRule, [
      { key: 'kind', label: t().ruleKind, options: ['exec', 'read', 'write'] },
      { key: 'pattern', label: t().rulePattern, hint: t().rulePlaceholder }
    ]).then(function (v) {
      if (!v || !v.pattern) return;
      jpost('/api/devices/rules', {
        device: cur, op: 'add', kind: v.kind, pattern: v.pattern,
        scope: v.kind !== 'exec' ? 'dir' : 'global'
      }).then(renderConsole).catch(oops);
    });
  };

  function loadLog() {
    if (!cur) return;
    $('#devlog').innerHTML = '<tr><td colspan="5" class="tempty">' + esc(t().loading) + '</td></tr>';
    jget('/api/devices/logs?device=' + encodeURIComponent(cur)).then(function (d) {
      var xs = (d.logs || []).slice().reverse();
      $('#devlog').innerHTML = xs.length ? xs.map(function (e) {
        var dec = e.decision || '';
        // 退出码由设备控制，必须转义（审计 2026-08-28, SEC-C-01）
        var exit = (e.exit === 0 || e.exit) ? esc('' + e.exit) : '—';
        return '<tr><td>' + esc(fmt(e.ts)) + '</td>' +
          '<td class="mono">' + esc(e.detail || '—') +
            (e.cwd ? '<span class="sub">' + esc(e.cwd) + '</span>' : '') + '</td>' +
          '<td>' + esc(('' + (e.peer_name || e.peer_fp || '—')).slice(0, 20)) + '</td>' +
          '<td' + (/den/i.test(dec) ? ' class="no"' : '') + '>' + esc(dec || '—') + '</td>' +
          '<td class="num">' + exit + '</td></tr>';
      }).join('') : '<tr><td colspan="5" class="tempty">' + esc(t().noLog) + '</td></tr>';
    }).catch(function (e) {
      $('#devlog').innerHTML = '<tr><td colspan="5" class="tempty">' + esc(t().cantReach) + '</td></tr>';
      oops(e);
    });
  }
  $('#logReload').onclick = loadLog;

  /* ── 设备设置（齿轮在哪一层就是哪一层的设置） ─────────────────────── */
  var lanOn = false, lark = {}, devNotify = {};

  function paintLan(lan) {
    $('#dsLan').hidden = !lan;
    if (!lan) return;
    lanOn = !!lan.enabled;
    $('#dsLanSw').className = 'sw' + (lanOn ? ' on' : '');
    $('#dsLanTxt').textContent = lan.connected ? t().lanOn(lan.relay)
      : (lan.enabled ? t().lanTrying(lan.relay) : t().lanOff);
  }
  $('#dsLanSw').onclick = function () {
    if (roGuard(cur)) return;
    jpost('/api/devices/lan', { device: cur, on: !lanOn }).then(renderConsole).catch(oops);
  };

  function health(el, h, empty) {
    if (!h) { el.className = 'note'; el.textContent = empty || t().noAttempts; return; }
    var when = fmt(h.attempted_at);
    if (h.result === 'failure') {
      el.className = 'note warn';
      el.textContent = t().lastFail(when, h.error || ('HTTP ' + (h.http_status || '—')));
      return;
    }
    el.className = 'note';
    el.textContent = t().lastOK(when);
  }

  function loadDeviceSettings(name) {
    $('#dsName').textContent = name;
    // 这套部署没配飞书凭证的话（GitHub OAuth 自部署实例都是），整张卡藏掉
    if (me && me.lark === false) $('#dsLark').hidden = true;
    else jget('/api/devices/lark?device=' + encodeURIComponent(name)).then(function (c) {
      if (cur !== name) return;
      lark = c; $('#dsLark').hidden = false; paintLark();
    }).catch(function () { $('#dsLark').hidden = true; });

    jget('/api/devices/notify?device=' + encodeURIComponent(name)).then(function (c) {
      if (cur !== name) return;
      devNotify = c; $('#dsNotify').hidden = false; paintDevNotify();
    }).catch(function () { $('#dsNotify').hidden = true; });
  }
  function paintLark() {
    var on = !!lark.approval_enabled;
    $('#dsLarkSw').className = 'sw' + (on ? ' on' : '');
    $('#dsLarkTxt').textContent = on ? t().larkOn(lark.notify_email || '—') : t().larkOff;
    $('#dsLarkPair').className = 'sw' + (lark.pairing_from_card ? ' on' : '');
    health($('#dsLarkHealth'), lark.delivery_health);
  }
  function saveLark(field, value) {
    if (roGuard(cur)) return;
    var body = { device: cur, approval_enabled: lark.approval_enabled, pairing_from_card: lark.pairing_from_card };
    body[field] = value;
    jpost('/api/devices/lark', body).then(function (r) { return r.json(); }).then(function (saved) {
      saved.delivery_health = lark.delivery_health;
      lark = saved; paintLark();
    }).catch(oops);
  }
  $('#dsLarkSw').onclick = function () { saveLark('approval_enabled', !lark.approval_enabled); };
  $('#dsLarkPair').onclick = function () { saveLark('pairing_from_card', !lark.pairing_from_card); };

  function paintDevNotify() {
    var on = !!devNotify.enabled;
    $('#dsNotifySw').className = 'sw' + (on ? ' on' : '');
    $('#dsNotifyTxt').textContent = on ? t().notifyDevOn : t().notifyDevOff;
    health($('#dsNotifyHealth'), devNotify.health);
  }
  $('#dsNotifySw').onclick = function () {
    if (roGuard(cur)) return;
    jpost('/api/devices/notify', { device: cur, enabled: !devNotify.enabled })
      .then(function (r) { return r.json(); })
      .then(function (saved) { saved.health = devNotify.health; devNotify = saved; paintDevNotify(); })
      .catch(oops);
  };

  $('#dsRemove').onclick = function () {
    if (!cur || roGuard(cur)) return;
    var name = cur;
    confirmBox(t().removeDevT, t().removeDevM(name), t().remove).then(function (ok) {
      if (!ok) return;
      jpost('/api/devices/remove', { device: name }).then(function () {
        toast(t().remove);
        go('devices');
      }).catch(oops);
    });
  };

  /* ── 设置 ────────────────────────────────────────────────────────────
     设置和设备设置都是页面，顶栏因此一直在 —— 覆盖层会把「你是谁」盖掉。
     更新日志仍是浮层：它是只读速览，不值得一个地址。 */
  $('#gear').onclick = function () { go('settings/' + curSet); };
  $('#sClose').onclick = function () { go('devices'); };
  $('#dGear').onclick = function () { go('device/' + encodeURIComponent(cur) + '/settings'); };
  $('#dsClose').onclick = function () { go('device/' + encodeURIComponent(cur)); };
  $('#clClose').onclick = function () { $('#clSheet').classList.remove('show'); };
  $('#ver').onclick = function () { $('#clSheet').classList.add('show'); loadChangelog(); };
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') $('#clSheet').classList.remove('show');
  });

  var curSet = 'tokens';
  var setLoaders = {
    tokens: loadTokens, notify: loadNotify, invites: loadInvites,
    friends: loadFriends, acl: loadACL, downloads: loadDownloads, audit: loadAudit
  };
  // 角色在这里拦，不只在导航项的 hidden 上拦。服务端的 requireAdmin 一直是好的
  // （TestInvitesRejectNonAdmin 断言 403，relay 根本没被碰到），漏的是路由：
  // 直接敲 #settings/invites 就能把管理员视图打开。见 issue #10。
  function allowedSet(s) {
    if (!setLoaders[s]) return 'tokens';
    if (s === 'invites' && !(me && me.role === 'admin')) return 'tokens';
    return s;
  }
  function openSheet(s) {
    s = allowedSet(s);
    curSet = s;
    $$('.sgroup button[data-s]').forEach(function (b) { b.classList.toggle('on', b.dataset.s === s); });
    $$('.sview').forEach(function (v) { v.classList.toggle('show', v.dataset.s === s); });
    showView('settings');
    if (setLoaders[s]) setLoaders[s]();
  }
  $$('.sgroup button[data-s]').forEach(function (b) {
    b.onclick = function () { go('settings/' + b.dataset.s); };
  });
  // 文档不再由门户渲染。文档站上线后把这个地址换掉即可。
  $('#docsLink').onclick = function () {
    window.open('https://github.com/Daily-AC/wanctl#readme', '_blank', 'noopener');
  };

  /* ── 令牌 ────────────────────────────────────────────────────────── */
  function loadTokens() {
    jget('/api/tokens').then(function (d) {
      var xs = d.tokens || [];
      $('#tokens').innerHTML = xs.length ? xs.map(function (x) {
        var rev = !!x.revoked_at;
        return '<tr' + (rev ? ' class="dim"' : '') + '><td>' + esc(x.label || '—') + '</td>' +
          '<td>' + esc(fmt(x.created_at)) + '</td>' +
          '<td>' + (x.expires_at ? esc(fmt(x.expires_at)) : esc(t().never2)) + '</td>' +
          '<td>' + esc(rev ? t().revoked : t().active) + '</td>' +
          '<td>' + (rev ? '' : '<button class="act danger" data-i="' + x.id + '">' + esc(t().revoke) + '</button>') + '</td></tr>';
      }).join('') : '<tr><td colspan="5" class="tempty">' + esc(t().noTokens) + '</td></tr>';
      $$('#tokens .act').forEach(function (b) {
        b.onclick = function () {
          jpost('/api/tokens/revoke', { id: Number(b.dataset.i) }).then(loadTokens).catch(oops);
        };
      });
    }).catch(oops);
  }
  $('#tAdd').onclick = function () {
    formBox($('#tAdd').textContent, t().issue, [
      { key: 'label', label: t().label, hint: t().labelPlaceholder },
      { key: 'days', label: t().expiry, type: 'number', min: 0 }
    ]).then(function (v) {
      if (!v) return;
      jpost('/api/tokens', { label: v.label || '', days: parseInt(v.days || '0', 10) || 0 })
        .then(function (r) { return r.json(); })
        .then(function (j) { $('#tVal').textContent = j.token; $('#tSlip').classList.add('show'); loadTokens(); })
        .catch(oops);
    });
  };
  function copier(btn, src) {
    btn.onclick = function () {
      if (navigator.clipboard) navigator.clipboard.writeText($(src).textContent);
      var old = btn.textContent;
      btn.textContent = t().copied;
      setTimeout(function () { btn.textContent = old; }, 1400);
    };
  }
  copier($('#tCopy'), '#tVal');
  copier($('#iCopy'), '#iVal');

  /* ── 通知 ────────────────────────────────────────────────────────── */
  var notify = {};
  function loadNotify() {
    jget('/api/notify').then(function (c) {
      notify = c;
      $('#nTxt').textContent = c.configured ? t().notifyOn(c.format) : t().notifyOff;
      $('#nURL').value = ''; $('#nURL').placeholder = c.url || 'https://...';
      $('#nFormat').value = c.format || 'feishu';
      $('#nKeyword').value = c.keyword || '';
      $('#nSecret').value = '';
      $('#nSecret').placeholder = c.secret_set ? '••••••••' : '';
      [['#nApproval', 'on_approval'], ['#nExec', 'on_exec'], ['#nLifecycle', 'on_lifecycle'],
       ['#nSecurity', 'on_security'], ['#nFailures', 'exec_failures_only'], ['#nDetail', 'include_detail']
      ].forEach(function (p) { $(p[0]).checked = !!c[p[1]]; });
      $('#nClear').checked = false;
      health($('#nHealth'), c.health);
    }).catch(oops);
  }
  $('#nSave').onclick = function () {
    var body = {
      format: $('#nFormat').value, keyword: $('#nKeyword').value.trim(),
      on_approval: $('#nApproval').checked, on_exec: $('#nExec').checked,
      on_lifecycle: $('#nLifecycle').checked, on_security: $('#nSecurity').checked,
      exec_failures_only: $('#nFailures').checked, include_detail: $('#nDetail').checked
    };
    var url = $('#nURL').value.trim(), secret = $('#nSecret').value.trim();
    if (url) body.url = url;
    if (secret || $('#nClear').checked) body.secret = secret;
    jpost('/api/notify', body).then(function () { toast(t().saved); loadNotify(); }).catch(oops);
  };
  $('#nTest').onclick = function () {
    var b = $('#nTest'); b.disabled = true;
    jpost('/api/notify/test', {}).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) { $('#nHealth').className = 'note warn'; $('#nHealth').textContent = d.error; }
      else toast(t().sent);
    }).catch(oops).then(function () { b.disabled = false; });
  };
  $('#nDelete').onclick = function () {
    confirmBox(t().clearNotifyT, t().clearNotifyM, t().del).then(function (ok) {
      if (!ok) return;
      jpost('/api/notify', { delete: true }).then(function () { toast(t().cleared); loadNotify(); }).catch(oops);
    });
  };

  /* ── 邀请 ────────────────────────────────────────────────────────── */
  function loadInvites() {
    jget('/api/invites').then(function (xs) {
      xs = xs || [];
      $('#invites').innerHTML = xs.length ? xs.map(function (i) {
        var kind = i.has_code ? t().inviteCode : (t().invitePre + ' ' + (i.github_login || ''));
        var state = i.used_at ? (t().inviteUsed + ' ' + (i.used_by_namespace || '')) : t().invitePending;
        return '<tr><td>' + esc(kind) + '</td><td>' + esc(fmt(i.created_at)) + '</td>' +
          '<td>' + esc(state) + '</td>' +
          '<td>' + (i.used_at ? '' : '<button class="act danger" data-i="' + i.id + '">' + esc(t().revoke) + '</button>') + '</td></tr>';
      }).join('') : '<tr><td colspan="4" class="tempty">' + esc(t().noInvites) + '</td></tr>';
      $$('#invites .act').forEach(function (b) {
        b.onclick = function () {
          confirmBox(t().revokeInviteT, t().revokeInviteM, t().revoke).then(function (ok) {
            if (!ok) return;
            jpost('/api/invites/revoke', { id: parseInt(b.dataset.i, 10) }).then(loadInvites).catch(oops);
          });
        };
      });
    }).catch(oops);
  }
  $('#iAdd').onclick = function () {
    formBox(t().newInvite, t().newInvite, [
      { key: 'login', label: t().inviteLogin, hint: t().invitePlaceholder }
    ]).then(function (v) {
      if (!v) return;
      jpost('/api/invites', { github_login: (v.login || '').trim() })
        .then(function (r) { return r.json(); })
        .then(function (j) {
          if (j.code) { $('#iVal').textContent = j.code; $('#iSlip').classList.add('show'); }
          else toast(t().saved);
          loadInvites();
        }).catch(oops);
    });
  };

  /* ── 好友 ────────────────────────────────────────────────────────── */
  function loadFriends() {
    jget('/api/friends').then(function (d) {
      var xs = d.friends || [];
      $('#friends').innerHTML = xs.length ? xs.map(function (f) {
        var ns = esc(f.namespace), state, act;
        if (f.status === 'accepted') {
          state = t().friendYes;
          act = '<button class="act danger" data-a="remove" data-ns="' + ns + '">' + esc(t().del) + '</button>';
        } else if (f.direction === 'incoming') {
          state = t().friendIn;
          act = '<button class="act" data-a="accept" data-ns="' + ns + '">' + esc(t().accept) + '</button>' +
                '<button class="act danger" data-a="decline" data-ns="' + ns + '">' + esc(t().decline) + '</button>';
        } else {
          state = t().friendOut;
          act = '<button class="act" data-a="remove" data-ns="' + ns + '">' + esc(t().withdraw) + '</button>';
        }
        return '<tr><td><b>' + ns + '</b></td><td>' + esc(state) + '</td>' +
          '<td>' + esc(day(f.since)) + '</td>' +
          '<td>' + act + '</td></tr>';
      }).join('') : '<tr><td colspan="4" class="tempty">' + esc(t().noFriends) + '</td></tr>';
      $$('#friends .act').forEach(function (b) {
        b.onclick = function () {
          var run = function () {
            jpost('/api/friends/' + b.dataset.a, { namespace: b.dataset.ns }).then(loadFriends).catch(oops);
          };
          if (b.dataset.a === 'remove') {
            confirmBox(t().removeFriendT, t().removeFriendM, t().del).then(function (ok) { if (ok) run(); });
          } else run();
        };
      });
    }).catch(oops);
  }
  $('#fAdd').onclick = function () {
    formBox(t().addFriend, t().send, [{ key: 'ns', label: t().friendNS, hint: 'octocat' }])
      .then(function (v) {
        if (!v || !v.ns) return;
        var ns = v.ns.trim().toLowerCase();
        fetch('/api/users/lookup?namespace=' + encodeURIComponent(ns)).then(function (r) {
          if (r.status === 404) throw new Error(ns);
          if (!r.ok) return r.text().then(function (x) { throw httpErr(r.status, x); });
          return jpost('/api/friends/request', { namespace: ns });
        }).then(function () { toast(t().saved); loadFriends(); }).catch(oops);
      });
  };

  /* ── 共享授权 ────────────────────────────────────────────────────── */
  function loadACL() {
    jget('/api/acl').then(function (d) {
      var xs = d.acl || [];
      $('#acl').innerHTML = xs.length ? xs.map(function (a) {
        return '<tr><td>' + esc(a.device) + '</td><td>' + esc(a.grantee) + '</td>' +
          '<td class="mono">' + esc(a.perms) + '</td>' +
          '<td><button class="act danger" data-i="' + a.id + '">' + esc(t().revoke) + '</button></td></tr>';
      }).join('') : '<tr><td colspan="4" class="tempty">' + esc(t().noACL) + '</td></tr>';
      $$('#acl .act').forEach(function (b) {
        b.onclick = function () {
          jpost('/api/acl/revoke', { id: Number(b.dataset.i) }).then(loadACL).catch(oops);
        };
      });
    }).catch(oops);
  }
  $('#aAdd').onclick = function () {
    Promise.all([
      jget('/api/devices').catch(function () { return {}; }),
      jget('/api/namespaces').catch(function () { return {}; })
    ]).then(function (r) {
      var own = (r[0].devices || []).filter(function (x) { return x.shared !== true; })
        .map(function (x) { return x.name; }).filter(Boolean);
      var nss = (r[1].namespaces || []).filter(function (n) { return n && n !== (me && me.namespace); });
      return formBox(t().shareDevice, t().share, [
        { key: 'device', label: t().shareDevice, options: own },
        { key: 'grantee', label: t().shareGrantee, options: nss },
        { key: 'perms', label: t().sharePerms, options: ['exec', 'read', 'exec,read', 'exec,read,write'] }
      ]);
    }).then(function (v) {
      if (!v || !v.device || !v.grantee) return;
      jpost('/api/acl', { device: v.device, grantee: v.grantee, perms: v.perms || 'exec' })
        .then(loadACL).catch(oops);
    }).catch(oops);
  };

  /* ── 审计 ────────────────────────────────────────────────────────── */
  function loadAudit() {
    jget('/api/audit').then(function (d) {
      var xs = d.audit || [];
      $('#audit').innerHTML = xs.length ? xs.map(function (a) {
        return '<tr><td>' + esc(fmt(a.ts)) + '</td><td>' + esc(a.device) + '</td>' +
          '<td class="mono">' + esc(a.event) + '</td></tr>';
      }).join('') : '<tr><td colspan="3" class="tempty">' + esc(t().noAudit) + '</td></tr>';
    }).catch(oops);
  }

  /* ── 下载安装 ────────────────────────────────────────────────────────
     版本与资产大小尽力从 GitHub API 拿 —— 门户服务器不一定够得着 GitHub，
     浏览器这一侧通常可以；拿不到就退回固定的 latest 直链。
     安卓卡在最前面且最大：手机是唯一没有别的入口的平台，你没法在手机上
     curl 一个安装脚本。 */
  var dlDone = false;
  function human(n) { if (!n) return ''; var m = n / 1048576; return m >= 1 ? m.toFixed(1) + ' MB' : Math.round(n / 1024) + ' KB'; }
  function loadDownloads() {
    if (dlDone) return;
    var GH = 'https://github.com/Daily-AC/wanctl', latest = GH + '/releases/latest/download';
    fetch('https://api.github.com/repos/Daily-AC/wanctl/releases/latest')
      .then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; })
      .then(function (rel) {
        var assets = (rel && rel.assets) || [];
        var asset = function (n) { return assets.filter(function (a) { return a.name === n; })[0]; };
        var url = function (n) { var a = asset(n); return a ? a.browser_download_url : latest + '/' + n; };
        // 与 scripts/release-targets.sh 同序；每个系统里 64 位在前。
        var NAMES = [
          ['wanctl-darwin-arm64', 'macOS · Apple silicon'], ['wanctl-darwin-amd64', 'macOS · Intel'],
          ['wanctl-linux-amd64', 'Linux · x86_64'], ['wanctl-linux-arm64', 'Linux · ARM64'],
          ['wanctl-linux-386', 'Linux · x86 32-bit'], ['wanctl-linux-arm', 'Linux · ARM 32-bit'],
          ['wanctl-linux-mipsle', 'Linux · MIPS LE'], ['wanctl-linux-mips', 'Linux · MIPS BE'],
          ['wanctl-windows-amd64.exe', 'Windows · x86_64'], ['wanctl-windows-386.exe', 'Windows · x86 32-bit'],
          ['wanctl-windows-arm64.exe', 'Windows · ARM64'],
          ['wanctl-android-arm64', 'Android · binary (ARM64)'], ['wanctl-android-arm', 'Android · binary (ARM 32-bit)'],
          ['wanctl-android-386', 'Android · binary (x86)'], ['wanctl-android-amd64', 'Android · binary (x86_64)']
        ];
        var apk = asset('wanctl-android-arm64.apk');
        var apkURL = apk ? apk.browser_download_url : latest + '/wanctl-android-arm64.apk';
        var alt = [['arm', 'ARM 32-bit'], ['386', 'x86'], ['amd64', 'x86_64']].map(function (p) {
          var n = 'wanctl-android-' + p[0] + '.apk', a = asset(n);
          return '<a href="' + esc(a ? a.browser_download_url : latest + '/' + n) + '">' + esc(p[1]) + '</a>';
        }).join(' · ');
        var origin = (me && me.relay_origin) || 'https://<your-relay>';
        var join = 'wanctl config set relay=' + origin + ' portal=' + location.origin + '\nwanctl';

        $('#dl').innerHTML =
          '<p class="lead"><a href="' + GH + '/releases" target="_blank" rel="noopener">' + esc(t().dlSource) + '</a>' +
            (rel && rel.tag_name ? ' · ' + esc(t().dlLatest) + ' <b>' + esc(rel.tag_name) + '</b>' : '') + '</p>' +
          '<div class="card" style="margin-top:20px"><h2>' + esc(t().dlPhone) + '</h2>' +
            '<p class="note">' + esc(t().dlPhoneSub) + '</p>' +
            '<p style="margin:14px 0 0"><a class="btn" href="' + esc(apkURL) + '">' + esc(t().dlApk) +
              (apk ? ' · ' + human(apk.size) : '') + '</a></p>' +
            '<p class="note" style="margin-top:14px">' + esc(t().dlApkAlt) + ' ' + alt + '</p></div>' +
          '<div class="card"><h2>' + esc(t().dlDesktop) + '</h2>' +
            '<p class="note">' + esc(t().dlDesktopSub) + '</p>' +
            '<p class="note" style="margin-top:14px">macOS / Linux</p>' +
            '<pre data-copy>' + esc('curl -fsSL ' + latest + '/install.sh | sh') + '</pre>' +
            '<p class="note">Windows (PowerShell)</p>' +
            '<pre data-copy>' + esc('irm ' + latest + '/install.ps1 | iex') + '</pre>' +
            '<p class="note">' + esc(t().dlJoin) + '</p>' +
            '<pre data-copy>' + esc(join) + '</pre></div>' +
          // 15 行全平台表默认折叠：99% 的人用上面那三行，这张表是给 MIPS 路由器那种人的。
          '<details class="card"><summary>' + esc(t().dlAll) + '</summary>' +
            '<table style="margin-top:14px"><tbody>' + NAMES.map(function (p) {
              var a = asset(p[0]);
              return '<tr><td>' + esc(p[1]) + '</td><td class="mono">' + esc(p[0]) + '</td>' +
                '<td>' + (a ? human(a.size) : '—') + '</td>' +
                '<td><a href="' + esc(url(p[0])) + '">↓</a></td></tr>';
            }).join('') + '</tbody></table></details>';
        $$('#dl pre[data-copy]').forEach(function (p) {
          // 标签内容就存在 data-copy 上，CSS 的 ::after 读它 —— 这样切语言
          // 和「已复制」回执都只是改一个属性。
          p.setAttribute('data-copy', t().copy);
          p.onclick = function () {
            if (navigator.clipboard) navigator.clipboard.writeText(p.textContent.trim());
            p.setAttribute('data-copy', t().copied);
            setTimeout(function () { p.setAttribute('data-copy', t().copy); }, 1400);
          };
        });
        dlDone = true;
      });
  }

  /* ── 更新日志 ────────────────────────────────────────────────────── */
  var clDone = false;
  function loadChangelog() {
    if (clDone) return;
    jget('/api/releases').then(function (d) {
      var rs = d.releases || [];
      $('#cl').innerHTML = rs.map(function (r, i) {
        return '<details class="card"' + (i === 0 ? ' open' : '') + '>' +
          '<summary><b>' + esc(r.version) + '</b>' +
          (i === 0 ? ' <span class="dim">· ' + esc(t().current) + '</span>' : '') + '</summary>' +
          '<div class="md">' + md(r.body || '') + '</div></details>';
      }).join('');
      clDone = true;
    }).catch(oops);
  }

  /* 极小 markdown 渲染器：段落、h1–h4、围栏与行内代码、粗斜体、链接、
     有序无序列表、引用、分隔线。输入一律先转义。 */
  function md(src) {
    var lines = ('' + src).replace(/\r\n/g, '\n').split('\n'), out = '', i = 0;
    var eH = function (s) { return s.replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); };
    // 名字和这几条守卫的形状被 security_test.go 盯着 —— 那是一道防止有人
    // 重写渲染器时把 URL 白名单静默丢掉的栅栏，改名之前先改那个测试。
    var mdSafeHref = function (s) {
      s = s.trim();
      if (/^https?:\/\/[^\s]+$/i.test(s)) return s;
      if (!s || /[\s\\]/.test(s) || s.startsWith('//') || /^[a-z][a-z0-9+.-]*:/i.test(s)) return '';
      return s;
    };
    var inl = function (s) {
      s = eH(s)
        .replace(/`([^`]+?)`/g, '<code>$1</code>')
        .replace(/\*\*([^*]+?)\*\*/g, '<strong>$1</strong>')
        .replace(/\*([^*]+?)\*/g, '<em>$1</em>');
      return s.replace(/\[([^\]]+?)\]\(([^)]+?)\)/g, function (_, l, h) {
        var safe = mdSafeHref(h);
        return safe ? '<a href="' + safe + '" target="_blank" rel="noopener noreferrer">' + l + '</a>' : l;
      });
    };
    while (i < lines.length) {
      var l = lines[i];
      if (l.indexOf('```') === 0) {
        var code = ''; i++;
        while (i < lines.length && lines[i].indexOf('```') !== 0) code += lines[i++] + '\n';
        i++; out += '<pre>' + eH(code.replace(/\n$/, '')) + '</pre>'; continue;
      }
      if (/^---+\s*$/.test(l)) { out += '<hr>'; i++; continue; }
      var h = /^(#{1,4})\s+(.*)$/.exec(l);
      if (h) { out += '<h' + h[1].length + '>' + inl(h[2]) + '</h' + h[1].length + '>'; i++; continue; }
      if (l.indexOf('> ') === 0) {
        var q = '';
        while (i < lines.length && lines[i].indexOf('> ') === 0) q += lines[i++].slice(2) + '\n';
        out += '<blockquote>' + inl(q.trim()).replace(/\n/g, '<br>') + '</blockquote>'; continue;
      }
      if (/^[-*]\s+/.test(l)) {
        var ul = '';
        while (i < lines.length && /^[-*]\s+/.test(lines[i])) ul += '<li>' + inl(lines[i++].replace(/^[-*]\s+/, '')) + '</li>';
        out += '<ul>' + ul + '</ul>'; continue;
      }
      if (/^\d+\.\s+/.test(l)) {
        var ol = '';
        while (i < lines.length && /^\d+\.\s+/.test(lines[i])) ol += '<li>' + inl(lines[i++].replace(/^\d+\.\s+/, '')) + '</li>';
        out += '<ol>' + ol + '</ol>'; continue;
      }
      if (l.trim() === '') { i++; continue; }
      var p = l; i++;
      while (i < lines.length && lines[i].trim() !== '' && !/^(```|#{1,4}\s|>\s|[-*]\s|\d+\.\s|---+\s*$)/.test(lines[i])) p += '\n' + lines[i++];
      out += '<p>' + inl(p).replace(/\n/g, '<br>') + '</p>';
    }
    return out;
  }

  /* ── 配对深链（从 AI 的拒绝消息里点进来） ─────────────────────────── */
  function showPair(p) {
    $('#pairM').textContent = t().pairMsg;
    $('#pairKV').innerHTML =
      '<dt>' + esc(t().kDevice) + '</dt><dd>' + esc(p.device) + '</dd>' +
      '<dt>' + esc(t().kController) + '</dt><dd>' + esc(p.label || p.name || '—') + '</dd>' +
      '<dt>' + esc(t().kFP) + '</dt><dd>' + esc(p.fp) + '</dd>';
    $('#pair').classList.add('show');
    var answer = function (v) {
      $('#pairYes').disabled = $('#pairNo').disabled = true;
      jpost('/api/devices/pair', { device: p.device, fp: p.fp, verdict: v }).then(function () {
        hidePair();
        toast(v === 'y' ? t().trustedNow : t().refusedPair, v === 'n');
        setTimeout(function () { try { window.close(); } catch (_) {} }, 1400);
      }).catch(function (e) {
        $('#pairYes').disabled = $('#pairNo').disabled = false;
        oops(e);
      });
    };
    $('#pairYes').onclick = function () { answer('y'); };
    $('#pairNo').onclick = function () { answer('n'); };
    // 谁能发这把钥匙，是设备主人说了算（后端 requireOwnedConsole 同样强制）。
    // 被授予方拿到的是使用权，不是发钥匙的权力 —— 所以别先把按钮给他，
    // 等点下去才回一句 403：那读起来像门户坏了，而不像「这本来就不归你管」。
    var gate = function () {
      var m = devMeta[p.device];
      if (!m || !m.shared) return;
      $('#pairM').textContent = t().pairOwnerOnly(m.owner || '—');
      $('#pairYes').remove();
      $('#pairNo').textContent = t().ok;
      $('#pairNo').onclick = hidePair;
    };
    if (devMeta[p.device] !== undefined) gate();
    else loadDevices().then(gate);
  }
  function hidePair() {
    $('#pair').classList.remove('show');
    if (location.hash.indexOf('#pair') === 0) history.replaceState(null, '', '#devices');
  }

  /* ── 路由 ────────────────────────────────────────────────────────── */
  function showView(v) {
    $$('.view').forEach(function (x) { x.classList.toggle('show', x.dataset.view === v); });
  }
  function go(v) {
    var h = '#' + v;
    if (location.hash !== h) history.pushState(null, '', h);
    route();
  }
  function route() {
    var h = location.hash;
    if (h.indexOf('#pair?') === 0) {
      closeDevice(); showView('devices');
      var q = new URLSearchParams(h.slice('#pair?'.length));
      var p = { device: q.get('device') || '', fp: q.get('fp') || '', name: q.get('name') || '', label: q.get('label') || '' };
      if (p.device && p.fp) showPair(p);
      return;
    }
    if (h.indexOf('#device/') === 0) {
      var rest = h.slice('#device/'.length);
      var settings = /\/settings$/.test(rest);
      var name = decodeURIComponent(settings ? rest.slice(0, -'/settings'.length) : rest);
      if (cur !== name) openDevice(name);
      // 设备的待审批要接着轮询，所以进它的设置页时不断开连接，只换显示的那一屏。
      showView(settings ? 'devsettings' : 'device');
      return;
    }
    // 设置有自己的地址：刷新、后退、把链接发给自己都该回到同一页。
    if (h.indexOf('#settings') === 0) {
      closeDevice();
      openSheet(h.slice('#settings/'.length) || curSet);
      return;
    }
    closeDevice();
    showView('devices');
    refreshAsks();
  }
  window.addEventListener('hashchange', route);
  $('#home').onclick = function () { go('devices'); };

  /* 语言切换后重画所有 JS 生成的内容 */
  function repaint() {
    if (cur && lastState) applyState(lastState);
    else renderDevices();
    dlDone = false; clDone = false;
    if ($('[data-view="settings"]').classList.contains('show') && setLoaders[curSet]) {
      if (curSet === 'downloads') loadDownloads(); else setLoaders[curSet]();
    }
  }
  $('#lang').onclick = function () { applyLang(lang === 'en' ? 'zh' : 'en'); };

  /* ── 启动 ────────────────────────────────────────────────────────── */
  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  applyLang(saved === 'zh' || saved === 'en' ? saved
    : (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en');

  // 路由要等身份到手：allowedSet 靠 me.role 判断，me 还是 null 时
  // #settings/invites 会被放行，那正是 issue #10 那个洞。
  var whoami = jget('/api/me').then(function (m) {
    me = m;
    paintWho(m);
    if (m.role === 'admin') $('#sInvites').hidden = false;
  }).catch(function () {
    $('#who').hidden = false;
    $('#whoName').textContent = t().notSignedIn;
  });

  function paintWho(m) {
    var login = m.login || m.identity || '';
    $('#who').hidden = false;
    // 显示名优先（GitHub 的 name），没有就用登录名。
    $('#whoName').textContent = m.name || login || '—';
    $('#whoName').title = login;
    // 命名空间是从登录名推出来的，通常就是它。只有不一致时才值得占一格。
    var ns = m.namespace || '';
    var differs = ns && login && ns.toLowerCase() !== login.toLowerCase();
    $('#ns').hidden = !differs;
    if (differs) $('#ns').textContent = ns;
    // 头像：拿得到就用，拉不动就退回字母章。onerror 是必须的 ——
    // avatars.githubusercontent.com 不是每个部署地点都通。
    var letter = (login || ns || '?').charAt(0);
    $('#ava').textContent = letter;
    if (m.avatar) {
      var img = new Image();
      img.alt = '';
      img.onload = function () { $('#ava').textContent = ''; $('#ava').appendChild(img); };
      img.src = m.avatar;
    }
  }

  // 版本徽章是外壳的一部分，不是更新日志页的一部分，所以在启动时填。
  jget('/api/releases').then(function (d) {
    if (d && d.current) { $('#ver').textContent = d.current; $('#ver').hidden = false; }
  }).catch(function () {});

  Promise.all([loadDevices(), whoami]).then(route);
  setInterval(function () { if (!cur) refreshAsks(); }, 4000);
})();
