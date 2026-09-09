/* 门户的离线预览工装。
 *
 * 拦掉 fetch，用一批**全部虚构**的数据喂给真正的 app.js —— 于是改样式不需要
 * relay、不需要 Postgres、不需要登录，也不必拿一台真设备去制造一条待审批。
 * 它不进二进制：tools/ 下的东西只在开发机上跑。
 *
 * 纪律：这里的形状必须跟真实端点一致。它是给眼睛看的工装，不是契约的第二份
 * 真源 —— 端点改了形状，这里跟着改，别让它替真代码撒谎。
 *
 *   tools/portalpreview/serve.sh        然后开 http://127.0.0.1:8712/
 */
(function () {
  'use strict';

  // 场景开关。空态、无人等待、中继不可达这三种形状在真实数据里可遇不可求，
  // 而它们恰恰是布局最容易崩的地方（一张卡都没有 / 一块区整个消失 / 只剩一行字）。
  //   ?scene=empty  设备为零 → 首台设备引导
  //   ?scene=noask  没人在等 → 聚合待审批整块不出现
  //   ?scene=down   /api/devices 502 → 「连不上中继」（纯文本，真形状）
  //   ?scene=oddcode /api/devices 返回一个字典里没有的裸错误码 → 未知码那一路
  //   ?scene=emptylists 令牌/邀请/好友/共享授权四张表全空 → 四处空态那一行
  var scene = new URLSearchParams(location.search).get('scene') || '';

  // 头像三态。真实端点只对 GitHub 会话返回 avatar_url，header(SSO) 模式不返回，
  // 而图片本身还可能加载失败 —— 三条路在界面上是两种样子，工装要两种都摆得出来。
  //   ?avatar=on      默认，一张真的 GitHub 头像
  //   ?avatar=broken  同一个源、必定 404 的路径，走 onerror 那一路
  //   ?avatar=off     不返回这个字段，等于 header(SSO) 模式
  var avatar = new URLSearchParams(location.search).get('avatar') || 'on';

  // 角色。门户里只有 admin 能看见「邀请」那一节，而这个差别过去在工装里
  // 摆不出来 —— /api/me 的 role 是写死的 admin，于是「普通用户看不看得见
  // 邀请入口」这件事，离线预览里问不出来。
  //   ?role=admin  默认
  //   ?role=user   普通用户
  var role = new URLSearchParams(location.search).get('role') === 'user' ? 'user' : 'admin';

  // ?slow=<毫秒> 只把 /api/devices 拖慢。本机上这个请求是即时的，于是「设备页
  // 在设备清单到达之前就画完了」那一类竞态在预览里根本重现不出来 —— 而它在
  // 真网络上是常态（issue #48：共享设备直接开链接，只读门禁没生效）。
  // 拖慢的只有这一个端点：它是那些竞态的唯一源头，全局延迟只会让每次普查更慢。
  var slow = Number(new URLSearchParams(location.search).get('slow')) || 0;

  // ?now=<毫秒> 把「现在」钉住。页面上每一个时间都是从它算出来的，不钉住的话
  // 两次截图之间光是钟走了几分钟就够让每一张都不一样，前后对比无从做起。
  var now = Number(new URLSearchParams(location.search).get('now')) || Date.now();
  var ago = function (s) { return new Date(now - s * 1000).toISOString(); };

  var devices = [
    { name: 'bench-02', alias: 'workshop', fingerprint: 'SHA256:HXCgsXAExAyRONww/y4Wricn4yqc1euNHRaH/47IhfV=', online: true, last_seen: ago(4) },
    // 甲方那台真机的形状：34 个字符、一个断不开的长名字。工装以前最长的是
    // 8 个字符的 bench-02，于是标题行、聚合待审批的 host、解除设备的确认文案
    // 里那些「名字放不下会怎样」的问题，在这里一个都问不出来。
    { name: 'DESKTOP-RQFV0SH-workstation-long', alias: '', fingerprint: 'SHA256:Kpjt1sDLepR/liqYHaA0x4Evg7JPsVZcDouLrjCNm2g=', online: true, last_seen: ago(6) },
    { name: 'atlas', alias: '', fingerprint: 'SHA256:cxUKYdO6FWR/Ah+m9caQdRpnP9aMla6p8igV5ZAz6EV=', online: true, last_seen: ago(9) },
    { name: 'kestrel', alias: '', fingerprint: 'SHA256:h0RDd+bD2hACHcciN6gURa+hq8s2HvrGVHYtuq8/WwS=', online: true, last_seen: ago(31) },
    { name: 'mill-01', alias: '', fingerprint: 'SHA256:MuNP5qNocP+eOX8Mrk4WsY2w75moRVPREKo9OKcf2QM=', online: false, last_seen: ago(7 * 3600) },
    { name: 'orchard', alias: '', fingerprint: 'SHA256:6ZcygFUIXuJXcOnbOLUCnvv2JwanktR0hjN+GiUSt7B=', online: false, last_seen: ago(4 * 86400) },
    { name: 'slate', alias: '', fingerprint: 'SHA256:OR0q3ilFsfmwBgQ7u5z1ejTTiVb5OPLm1n3Y8QzhQib=', online: true, last_seen: ago(12), shared: true, owner: 'rowan', manage: false },
    // 同样是共享来的，但这一份授权带着 manage：控制面归你，属主的东西不归。
    // 两台一起摆着，是因为「能管」和「只能用」在屏幕上必须一眼分得开。
    { name: 'quarry', alias: '', fingerprint: 'SHA256:Yb2NpQ8sMxKe5Wj1TrLvA7ZcHnFo3DiU6gPqRtEwSaM=', online: true, last_seen: ago(20), shared: true, owner: 'rowan', manage: true }
  ];

  var waiting = [
    {
      device: 'bench-02', id: 'a41f9c22', kind: 'exec',
      cmd: 'rm -rf /data/checkpoints/run-2291 --force',
      cwd: '/data', peer: 'studio · SHA256:a5yGk562ILhUW/D+…', created: ago(7)
    },
    {
      device: 'DESKTOP-RQFV0SH-workstation-long', id: 'b7724e10', kind: 'exec',
      cmd: 'python /srv/pipelines/nightly/ingest.py --config /etc/wanctl/ingest.production.yaml --resume',
      cwd: '/srv/pipelines/nightly', peer: 'studio · SHA256:a5yGk562ILhUW/D+…', created: ago(22)
    },
    {
      device: 'atlas', fp: 'SHA256:2lvJifeK+//VCxE0zMA9YVdw76clBF0bGk6n7zbyoaz=',
      name: 'kestrel', label: 'claude-code on kestrel', created: ago(41)
    }
  ];

  var consoles = {
    'bench-02': {
      mode: 'normal',
      pending: [waiting[0]],
      pending_pairings: [],
      rules: [
        { kind: 'exec', pattern: 'nvidia-smi', scope: 'global' },
        { kind: 'exec', pattern: 'python train.py *', scope: 'global' },
        { kind: 'read', pattern: '/data', scope: 'dir', dir: '/data' }
      ],
      trusted: [
        { fp: 'SHA256:a5yGk562ILhUW/D+1nJJZB4lDht0IAe9AW6nXf/Y7x/=', name: 'studio', label: 'studio (my laptop)', last_seen: ago(60) },
        { fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(3) }
      ]
    },
    'DESKTOP-RQFV0SH-workstation-long': {
      mode: 'normal',
      pending: [waiting[1]],
      pending_pairings: [],
      rules: [{ kind: 'exec', pattern: 'python /srv/pipelines/nightly/ingest.py *', scope: 'dir', dir: '/srv/pipelines/nightly' }],
      trusted: [{ fp: 'SHA256:a5yGk562ILhUW/D+1nJJZB4lDht0IAe9AW6nXf/Y7x/=', name: 'studio', label: 'studio (my laptop)', last_seen: ago(15) }]
    },
    'atlas': {
      mode: 'normal', pending: [],
      pending_pairings: [waiting[2]],
      rules: [], trusted: [{ fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(5) }]
    },
    'kestrel': {
      mode: 'bypass', pending: [], pending_pairings: [],
      rules: [{ kind: 'exec', pattern: '*', scope: 'global' }],
      trusted: [{ fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(2) }]
    },
    'mill-01': {
      mode: 'normal', pending: [], pending_pairings: [], rules: [],
      trusted: [{ fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(7 * 3600) }]
    },
    'orchard': {
      mode: 'normal', pending: [], pending_pairings: [], rules: [],
      trusted: [{ fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(4 * 86400) }]
    },
    'slate': {
      mode: 'normal', pending: [], pending_pairings: [], rules: [], trusted: []
    },
    // 能管的那台共享设备。它得有东西可管 —— 一条待审批、一条待信任、一条规则、
    // 一个已信任的控制端 —— 否则「管理权到手了」这一屏和「只能用」那一屏在
    // 截图上长得一样，看不出差别的状态等于没摆出来。
    'quarry': {
      mode: 'normal',
      pending: [{ id: 77, peer: 'rowan-laptop', cmd: 'rsync -a /data/quarry/ /backup/quarry/', cwd: '/data/quarry', created: ago(18) }],
      pending_pairings: [{ fp: 'SHA256:Lq7WdEr2ZmXc4Nv8TbKiA1YpHs6UoJf3RgQeSxCwPaB=', name: 'rowan-phone', label: 'wanctl on rowan-phone', created: ago(40) }],
      rules: [{ kind: 'read', pattern: '/data/quarry/**', scope: 'global' }],
      trusted: [{ fp: 'SHA256:Xy3OMBhwVLhhfU0aYX5DUh5AB+QJVKqniQ1YzC9+osd=', name: 'portal', label: 'portal', last_seen: ago(9) }]
    }
  };

  var logs = [
    { ts: ago(90), type: 'exec', detail: 'nvidia-smi --query-gpu=memory.used --format=csv', peer_name: 'studio', decision: 'allowed by rule', exit: 0 },
    { ts: ago(340), type: 'exec', detail: 'python train.py --epochs 40 --resume', cwd: '/data', peer_name: 'studio', decision: 'allowed by rule', exit: 0 },
    { ts: ago(910), type: 'file', detail: 'read /data/config.yaml', peer_name: 'studio', decision: 'allowed once', exit: 0 },
    { ts: ago(1800), type: 'exec', detail: 'shutdown -h now', peer_name: 'kestrel', decision: 'denied by owner', exit: 1 },
    { ts: ago(3600), type: 'connect', detail: 'session opened', peer_name: 'portal', decision: '', exit: null }
  ];

  var db = {
    // 形状照抄 handleMe。这里曾经把 identity 填成一串 SHA256 指纹，
    // 而真实端点返回的是登录名 —— 工装替真代码撒了谎，页面照着假数据把
    // 门户自己的指纹当成「你的编号」显示在人名旁边。别再这么干。
    '/api/me': {
      identity: 'ardith', login: 'ardith', name: 'Ardith Vale',
      namespace: 'acme', provider: 'github', role: role,
      lark: true, relay_origin: 'https://relay.example.com',
      // 形状照抄 githubAvatarURL：同一个源、u/<数字账号 id>、?s=96。
      avatar_url: avatar === 'off' ? undefined
        : avatar === 'broken' ? 'https://avatars.githubusercontent.com/u/0/nope?s=96'
        : 'https://avatars.githubusercontent.com/u/583231?s=96'
    },
    '/api/devices': { devices: devices },
    '/api/pending': { items: waiting },
    '/api/tokens': {
      tokens: [
        { id: 3, label: 'studio', created_at: ago(9 * 86400), expires_at: null },
        { id: 2, label: 'ci', created_at: ago(40 * 86400), expires_at: ago(-50 * 86400) },
        { id: 1, label: 'old laptop', created_at: ago(120 * 86400), expires_at: null, revoked_at: ago(30 * 86400) }
      ]
    },
    // 两行，因为「可管理」这一列有两种状态，只摆一种就看不出开关长什么样。
    // perms 仍然回 full：中继照旧接受这个字段并忽略它（ADR 0007）。
    '/api/acl': { acl: [
      { id: 2, device: 'bench-02', grantee: 'rowan', perms: 'full', manage: false },
      { id: 5, device: 'DESKTOP-RQFV0SH-workstation-long', grantee: 'juniper', perms: 'full', manage: true }
    ] },
    '/api/friends': {
      friends: [
        { namespace: 'rowan', status: 'accepted', since: ago(60 * 86400) },
        { namespace: 'juniper', status: 'pending', direction: 'incoming' },
        { namespace: 'fenn', status: 'pending', direction: 'outgoing' }
      ]
    },
    // 形状照抄 relay.AccessRequest：note 是已经规整过的纯文本一行，
    // status 只有 pending / approved / declined 三种，decided_at 未决时不出现。
    // 队列是私密的，所以这是管理员才拿得到的那一份。
    '/api/access-requests': {
      requests: [
        {
          id: 12, provider: 'github', subject: '10137', login: 'wren', note: '',
          status: 'pending', created_at: ago(3 * 3600), decided_at: null, decided_by: ''
        },
        {
          // 顶格 200 字的说明：申请人唯一能自己填的东西，也是这块最容易
          // 在手机上把卡片撑坏的地方，所以工装里放的就是上限那一条。
          id: 11, provider: 'github', subject: '20481', login: 'thornbury-analytics-ops', status: 'pending',
          note: 'We run a fleet of build machines across two offices and want to try wanctl for the approval workflow before asking our security team to look at self-hosting it. Happy to answer anything about what we would use it for.',
          created_at: ago(2 * 86400), decided_at: null, decided_by: ''
        },
        {
          id: 9, provider: 'github', subject: '30119', login: 'fenn', note: 'let me in',
          status: 'declined', created_at: ago(12 * 86400), decided_at: ago(11 * 86400), decided_by: 'acme'
        }
      ]
    },
    '/api/invites': [
      { id: 4, has_code: true, created_at: ago(2 * 86400) },
      { id: 3, github_login: 'juniper', created_at: ago(10 * 86400), used_at: ago(9 * 86400), used_by_namespace: 'juniper' }
    ],
    '/api/namespaces': { namespaces: ['acme', 'rowan', 'juniper', 'fenn'] },
    '/api/audit': {
      audit: [
        { ts: ago(120), device: 'bench-02', event: 'dial' },
        { ts: ago(600), device: 'atlas', event: 'dial' },
        { ts: ago(4200), device: 'bench-02', event: 'register' }
      ]
    },
    '/api/notify': {
      configured: true, format: 'feishu', url: 'https://open.example.com/hook/…', secret_set: true,
      on_approval: true, on_exec: false, on_lifecycle: true, on_security: true,
      exec_failures_only: true, include_detail: false,
      health: { result: 'success', attempted_at: ago(400) }
    },
    '/api/releases': {
      current: 'v0.3.4',
      releases: [
        { version: 'v0.3.4', body: '# v0.3.4\n\n**The install script a relay serves now installs from that relay.**\n\n## Install\n\n- A relay rewrites its own `/install.sh` and `/install.ps1` to download from its own `/dl` mirror, so nothing extra has to be set:\n\n  ```bash\n  curl -fsSL https://relay.example.com/install.sh | sh\n  ```\n  ```powershell\n  irm https://relay.example.com/install.ps1 | iex\n  ```\n\n  Until now a relay served the script baked in at release time, whose download base pointed at GitHub — and the people who fetch a script from a relay are usually exactly the ones who cannot reach that page.\n\n## Upgrade\n\n- New `wanctl config set release_base=…`, which picks where `wanctl update` fetches signed artefacts from:\n\n  ```bash\n  wanctl config set release_base=https://relay.example.com/dl\n  ```\n\n  `WANCTL_RELEASE_BASE` still wins over it.\n' },
        { version: 'v0.3.3', body: '## Security\n\n- Release artefacts are verified against their signature, size and SHA-256 before anything is written to disk.\n' }
      ]
    }
  };

  function match(url) {
    var p = url.split('?')[0];
    if (scene === 'empty' && p === '/api/devices') return { devices: [] };
    // 「没人在等你」同时清空两个队列：设备的待审批，和门口的访问申请。
    // 两块都是「有人等你决定」，空掉时都该整块消失。
    if (scene === 'noask' && p === '/api/pending') return { items: [] };
    if (scene === 'noask' && p === '/api/access-requests') return { requests: [] };
    // 四张设置表的空态。每张都是一行 <td colspan> 的 .tempty，而这一行落在
    // 「最后一列」上 —— 那一列是右对齐的。有数据时看不出来，空的时候才看得出来。
    if (scene === 'emptylists') {
      if (p === '/api/tokens') return { tokens: [] };
      if (p === '/api/invites') return [];
      if (p === '/api/friends') return { friends: [], incoming: [], outgoing: [] };
      if (p === '/api/acl') return { acl: [] };
    }
    if (p === '/api/access-requests/decide') return {};
    if (p === '/api/devices/console') { var st = consoles[new URLSearchParams(url.split('?')[1]).get('device')]; if (st) { var android = new URLSearchParams(url.split('?')[1]).get('device') === 'bench-02'; st.info = {platform:android?'android':'linux', adb_pair:android}; } }
    if (p === '/api/devices/console') return consoles[new URLSearchParams(url.split('?')[1]).get('device')] || { mode: 'normal', pending: [], pending_pairings: [], rules: [], trusted: [] };
    if (p === '/api/devices/logs') return { logs: logs.slice().reverse() };
    if (p === '/api/devices/lark') return { approval_enabled: true, pairing_from_card: false, notify_email: 'you@example.com', delivery_health: { result: 'success', attempted_at: ago(300) } };
    if (p === '/api/devices/notify') return { enabled: false };
    if (p === '/api/users/lookup') return {};
    // POST-only endpoints still need an entry here: match() returning undefined
    // is what produces the preview's 404, before the write branch is reached.
    if (p === '/api/devices/remove' || p === '/api/devices/adb-pair') return {};
    if (p === '/api/acl/manage') return {};
    if (p === '/api/devices/alias') return {};
    if (p === '/api/devices/mode') return {};
    return db[p];
  }

  var realFetch = window.fetch.bind(window);
  window.fetch = function (url, opts) {
    url = '' + url;
    // 真正出网的只有 GitHub 的 releases API（下载页要资产大小），让它过。
    if (url.indexOf('http') === 0 && url.indexOf(location.origin) !== 0) return realFetch(url, opts);

    if (scene === 'down' && url.split('?')[0] === '/api/devices') {
      // 形状照抄真代码：门户拿不到中继时走的是 http.Error(w, "relay unreachable: …", 502)
      // —— 纯文本、502、末尾带换行。这里原来编了一个 {"error":"relay_unreachable"} 的
      // JSON，工装第三次替真代码撒谎，而那个假形状正好把 toast 里那条裸 JSON 藏住了。
      return Promise.resolve(new Response('relay unreachable: dial tcp 10.60.0.145:8080: i/o timeout\n',
        { status: 502, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }));
    }
    // 认不出来的码：中继将来加一个新 token，门户不该因此又把原始报文端出来。
    if (scene === 'oddcode' && url.split('?')[0] === '/api/devices') {
      return Promise.resolve(new Response('quota_exceeded',
        { status: 429, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }));
    }
    var body = match(url);
    if (body === undefined) {
      return Promise.resolve(new Response('preview: no fixture for ' + url, { status: 404 }));
    }
    if (slow && url.split('?')[0] === '/api/devices') {
      return new Promise(function (res) {
        setTimeout(function () {
          res(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
        }, slow);
      });
    }
    if (opts && opts.method === 'POST') {
      // 写操作一律成功。工装不模拟状态机 —— 它是给眼睛看的，
      // 真正的裁决路径由 go test 覆盖。
      var out = { ok: true };
      if (url.indexOf('/api/devices/remove') === 0) {
        var removed = JSON.parse(opts.body).device;
        for (var i = devices.length - 1; i >= 0; i--) if (devices[i].name === removed) devices.splice(i, 1);
      }
      if (url.indexOf('/api/devices/adb-pair') === 0) out = { paired: true };
      if (url.indexOf('/api/devices/alias') === 0) {
        var want = JSON.parse((opts && opts.body) || '{}');
        var row = devices.filter(function (x) { return x.name === want.device; })[0];
        if (row) row.alias = ('' + (want.alias || '')).trim();
        out = { name: want.device, alias: row ? row.alias : '' };
      }
      // 模式是唯一一个「写完立刻再读一遍」的写操作：真代码 POST 完就
      // renderConsole，而设备真的换了模式。工装要是不记这一笔，胶囊会在
      // 一次成功的切换之后自己弹回原样 —— 那是工装在替真代码撒谎。
      if (url.indexOf('/api/devices/mode') === 0) {
        var mw = JSON.parse((opts && opts.body) || '{}');
        if (consoles[mw.device]) consoles[mw.device].mode = mw.mode;
      }
      // 形状照抄 /admin/acl/manage 的回包，而且真的把这一笔记下来 ——
      // 不记的话开关会在一次成功的切换之后自己弹回原样，那是工装在撒谎。
      if (url.indexOf('/api/acl/manage') === 0) {
        var mw = JSON.parse((opts && opts.body) || '{}');
        (db['/api/acl'].acl || []).forEach(function (row) {
          if (row.device === mw.device && row.grantee === mw.grantee) row.manage = mw.manage === true;
        });
        out = { device: mw.device, grantee: mw.grantee, manage: mw.manage === true };
      }
      if (url.indexOf('/api/tokens') === 0) out = { token: 'wanctl_9fQ2mXbLpR7tZv4NcKwJaHe1UgSoD5iM3xNrTqCEy' };
      if (url.indexOf('/api/invites') === 0) out = { code: 'winv_4TmQb9RvNc7WpLd2FjKa5Y' };
      return Promise.resolve(new Response(JSON.stringify(out), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  };

  // 让预览页能一键跳到某个状态：?view=device/bench-02、?view=settings/tokens 等
  var q = new URLSearchParams(location.search);
  var want = q.get('view');
  if (want && !location.hash) location.hash = '#' + want;
  // ?lang=zh 预置语言。无头浏览器的 navigator.language 恒为 en-US，
  // 不这样就截不到中文那一版。
  var lang = q.get('lang');
  if (lang) { try { localStorage.setItem('wanctl.lang', lang); } catch (_) {} }
})();
