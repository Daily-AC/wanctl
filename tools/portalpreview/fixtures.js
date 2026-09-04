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

  var now = Date.now();
  var ago = function (s) { return new Date(now - s * 1000).toISOString(); };

  var devices = [
    { name: 'bench-02', alias: 'workshop', fingerprint: 'SHA256:fJoz5aAqXxK1r0LmT9vQe8YhCwPd3NuBiGs7DOAEo=', online: true, last_seen: ago(4) },
    { name: 'atlas', alias: '', fingerprint: 'SHA256:Kq2mVb7ThLnW4xRc0PjZa9eYsDgU6iBoM1XfNvQtEr=', online: true, last_seen: ago(9) },
    { name: 'kestrel', alias: '', fingerprint: 'SHA256:9WpLmXc3RtYb8QnZk1FvJaHe5UgSoD2iN7xMrTqBEw=', online: true, last_seen: ago(31) },
    { name: 'mill-01', alias: '', fingerprint: 'SHA256:3TzQrK8mBvNc5XwPd1LhJf7YaGeSoU9iR2xMtCqDEn=', online: false, last_seen: ago(7 * 3600) },
    { name: 'orchard', alias: '', fingerprint: 'SHA256:Vb6NmXq2RtZc9WpLk4FvJa1He8UgSoD5iN3xMrTqEy=', online: false, last_seen: ago(4 * 86400) },
    { name: 'slate', alias: '', fingerprint: 'SHA256:Hq4LmZb8TvNc2XwRd6PjKf3YaGeSoU1iM9xNtCrDEp=', online: true, last_seen: ago(12), shared: true, owner: 'rowan', perms: 'exec,read' }
  ];

  var waiting = [
    {
      device: 'bench-02', id: 'a41f9c22', kind: 'exec',
      cmd: 'rm -rf /data/checkpoints/run-2291 --force',
      cwd: '/data', peer: 'studio · SHA256:Pn7VqL2mXb…', created: ago(7)
    },
    {
      device: 'atlas', fp: 'SHA256:Zx4TmQb9RvNc7WpLd2FjKa5YeGhSoU8iM1xNrTqCEw=',
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
        { fp: 'SHA256:Pn7VqL2mXb4TcZw9…', name: 'studio', label: 'studio (my laptop)', last_seen: ago(60) },
        { fp: 'SHA256:e+gYUIcZfC7HcEGi…', name: 'portal', label: 'portal', last_seen: ago(3) }
      ],
      lan: { enabled: true, connected: true, relay: 'lan-relay.internal:7443' }
    },
    'atlas': {
      mode: 'normal', pending: [],
      pending_pairings: [waiting[1]],
      rules: [], trusted: [{ fp: 'SHA256:e+gYUIcZfC7HcEGi…', name: 'portal', label: 'portal', last_seen: ago(5) }],
      lan: { enabled: false, connected: false, relay: 'lan-relay.internal:7443' }
    },
    'kestrel': {
      mode: 'bypass', pending: [], pending_pairings: [],
      rules: [{ kind: 'exec', pattern: '*', scope: 'global' }],
      trusted: [{ fp: 'SHA256:e+gYUIcZfC7HcEGi…', name: 'portal', label: 'portal', last_seen: ago(2) }],
      lan: null
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
      namespace: 'acme', provider: 'github', role: 'admin',
      lark: true, relay_origin: 'https://relay.example.com'
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
    '/api/acl': { acl: [{ id: 2, device: 'bench-02', grantee: 'rowan', perms: 'exec,read' }] },
    '/api/friends': {
      friends: [
        { namespace: 'rowan', status: 'accepted', since: ago(60 * 86400) },
        { namespace: 'juniper', status: 'pending', direction: 'incoming' },
        { namespace: 'fenn', status: 'pending', direction: 'outgoing' }
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
    if (p === '/api/devices/console') return consoles[new URLSearchParams(url.split('?')[1]).get('device')] || { mode: 'normal', pending: [], pending_pairings: [], rules: [], trusted: [] };
    if (p === '/api/devices/logs') return { logs: logs.slice().reverse() };
    if (p === '/api/devices/lark') return { approval_enabled: true, pairing_from_card: false, notify_email: 'you@example.com', delivery_health: { result: 'success', attempted_at: ago(300) } };
    if (p === '/api/devices/notify') return { enabled: false };
    if (p === '/api/users/lookup') return {};
    // POST-only endpoints still need an entry here: match() returning undefined
    // is what produces the preview's 404, before the write branch is reached.
    if (p === '/api/devices/alias') return {};
    return db[p];
  }

  var realFetch = window.fetch.bind(window);
  window.fetch = function (url, opts) {
    url = '' + url;
    // 真正出网的只有 GitHub 的 releases API（下载页要资产大小），让它过。
    if (url.indexOf('http') === 0 && url.indexOf(location.origin) !== 0) return realFetch(url, opts);

    var body = match(url);
    if (body === undefined) {
      return Promise.resolve(new Response('preview: no fixture for ' + url, { status: 404 }));
    }
    if (opts && opts.method === 'POST') {
      // 写操作一律成功。工装不模拟状态机 —— 它是给眼睛看的，
      // 真正的裁决路径由 go test 覆盖。
      var out = { ok: true };
      if (url.indexOf('/api/devices/alias') === 0) {
        var want = JSON.parse((opts && opts.body) || '{}');
        var row = devices.filter(function (x) { return x.name === want.device; })[0];
        if (row) row.alias = ('' + (want.alias || '')).trim();
        out = { name: want.device, alias: row ? row.alias : '' };
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
