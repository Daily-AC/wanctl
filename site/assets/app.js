/* wanctl product site.
 *
 * 首屏是一支片子（assets/film-zh.mp4），这个文件管四件事：
 *   片子      静音自动循环（甲方 09-27 定），一个按钮开声音；它也是播放键
 *   语言      data-en / data-zh 两份文案，按浏览器语言和上次的选择切
 *   安装      系统 × 下载源两个分段控件，命令永远只有一份（两行：装上、启动）
 *   配对卡    门户里那张「控制端想连进来」放大出来，按钮真能按
 *
 * 页面上的设备名、指纹都是虚构示例；地址和命令是真的。
 * 验收接口 window.__site：film() / lang() / install()
 */
(function () {
  'use strict';

  var REDUCED = !!(window.matchMedia &&
                   window.matchMedia('(prefers-reduced-motion: reduce)').matches);
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  var T = {
    en: {
      copy: 'Copy', copied: 'Copied',
      prompt: 'Set up wanctl for me: https://wc.z10.dev',
      sound: { on: 'Sound on', off: 'Sound off', play: 'Play' },
      trusted: 'Trusted. Ask the AI to run the command again.', refused: 'Refused.',
      src: {
        gh: 'Straight from the GitHub release.',
        cn: 'Served by the official relay, for when GitHub is slow to reach. Same binaries, same signature.'
      },
      os: { unix: '', win: ' PowerShell 5.1 and up; no OpenSSL needed.' }
    },
    zh: {
      copy: '复制', copied: '已复制',
      prompt: '帮我接上这个：https://wc.z10.dev',
      sound: { on: '开声音', off: '关声音', play: '播放' },
      trusted: '已信任。回去让 AI 再跑一次命令。', refused: '已拒绝。',
      src: {
        gh: '直接从 GitHub release 拉。',
        cn: '从官方 relay 拉，GitHub 拉不动的时候走这条。同样的二进制，同样的签名。'
      },
      os: { unix: '', win: ' 需要 PowerShell 5.1 以上；不需要装 OpenSSL。' }
    }
  };
  var lang = 'en';
  var t = function () { return T[lang]; };

  function copyText(txt, btn, after) {
    var ok = function () {
      btn.textContent = t().copied;
      if (after) after();
      setTimeout(function () { btn.textContent = t().copy; }, 1600);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(txt).then(ok, function () {});
      return;
    }
    var ta = document.createElement('textarea');
    ta.value = txt; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); ok(); } catch (_) {}
    document.body.removeChild(ta);
  }

  /* ── 片子 ──────────────────────────────────────────────────────
     一个按钮三种说法，看的是 <video> 自己的状态，不另记一份：
       暂停着 → 「播放」（带声音播）。减弱动态效果时不自动播；iOS 省电模式之类
                 拦下自动播放时 play() 会被拒，也落到这里。
       静音在播 → 「开声音」
       有声在播 → 「关声音」
     开声音不从头放：接着当前这一帧往下，访客点的是声音，不是重播。 */
  var film = $('#film'), sound = $('#sound'), soundlbl = $('#soundlbl');

  function renderSound() {
    var state = film.paused ? 'play' : (film.muted ? 'on' : 'off');
    sound.className = 'sound is-' + state;
    soundlbl.textContent = t().sound[state];
  }
  if (film && sound) {
    ['play', 'pause', 'volumechange'].forEach(function (e) { film.addEventListener(e, renderSound); });
    sound.addEventListener('click', function () {
      if (film.paused) { film.muted = false; film.play().catch(function () {}); }
      else film.muted = !film.muted;
    });
    sound.hidden = false;
    if (!REDUCED) film.play().catch(function () {});
    renderSound();
  }

  /* ── 装它 ───────────────────────────────────────────────────────
     两条真实的路：GitHub release，和官方 relay。relay 发出去的脚本里烧着
     它自己的地址（RELAY_SELF），所以从它拉的脚本也从它装二进制，整条链路不碰 GitHub。
     见 internal/relay/dist.go 的 installerHandler 与 WANCTL_PUBLIC_ORIGIN。
     第二行 `wanctl start` 在两个系统上一样：起 agent，第一次会开浏览器登录、要你把授权码贴回来。 */
  var GH = 'https://github.com/Daily-AC/wanctl/releases/latest/download';
  var CN = 'https://wanctl-relay.z10.dev';
  var INSTALL = {
    'unix-gh': 'curl -fsSL ' + GH + '/install.sh | sh',
    'unix-cn': 'curl -fsSL ' + CN + '/install.sh | sh',
    'win-gh':  'irm ' + GH + '/install.ps1 | iex',
    'win-cn':  'irm ' + CN + '/install.ps1 | iex'
  };
  var pick = { os: /Windows/i.test(navigator.userAgent || '') ? 'win' : 'unix',
               src: 'gh', srcTouched: false };
  var osseg = $('#osseg'), srcseg = $('#srcseg'),
      installcmd = $('#installcmd'), srcnote = $('#srcnote');

  function renderInstall() {
    installcmd.textContent = INSTALL[pick.os + '-' + pick.src] + '\nwanctl start';
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
  /* 「还有」这个信号（app.css 的 .cmdline.more）：量的是溢出本身，不是猜宽度。 */
  function markScroll() {
    $$('.cmdline code, .cmdline pre').forEach(function (c) {
      c.parentNode.classList.toggle('more', c.scrollWidth - c.clientWidth > 1);
    });
  }
  if (window.ResizeObserver) {
    var ro = new ResizeObserver(markScroll);
    $$('.cmdline code, .cmdline pre').forEach(function (c) { ro.observe(c); });
  }
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
  copy.addEventListener('click', function () { copyText(installcmd.textContent.trim(), copy); });

  /* 首屏那句是写给人看的（「把 wc.z10.dev 甩给你的 Agent」），复制走的是一句
     能直接贴给 Agent 的话。复制之后胶囊里换成剪贴板里的那句，你知道自己拿到了什么。 */
  var prompt = $('#prompt'), copyprompt = $('#copyprompt');
  copyprompt.addEventListener('click', function () {
    copyText(t().prompt, copyprompt, function () { prompt.textContent = '› ' + t().prompt; markScroll(); });
  });

  /* ── 配对卡 ───────────────────────────────────────────────────── */
  var pair = $('#pair'), toast = $('#pairtoast'), pairTimer = null;
  $('#pairacts').addEventListener('click', function (e) {
    var b = e.target.closest('[data-v]');
    if (!b) return;
    var yes = b.dataset.v === 'y';
    toast.textContent = yes ? t().trusted : t().refused;
    pair.classList.add('answered');
    pair.classList.toggle('refused', !yes);
    clearTimeout(pairTimer);
    pairTimer = setTimeout(function () { pair.classList.remove('answered', 'refused'); toast.textContent = ''; }, 2600);
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
    $$('[data-lbl-en]').forEach(function (el) {
      el.setAttribute('aria-label', el.getAttribute('data-lbl-' + l));
    });
    if (!pick.srcTouched) pick.src = l === 'zh' ? 'cn' : 'gh';
    renderInstall();
    if (film && sound) renderSound();
    if (toast.textContent) toast.textContent = pair.classList.contains('refused') ? t().refused : t().trusted;
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }
  $('#lang').addEventListener('click', function () { applyLang(lang === 'en' ? 'zh' : 'en'); });

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  if (saved === 'zh' || (!saved && /^zh/i.test(navigator.language || ''))) applyLang('zh');
  else renderInstall();

  window.__site = {
    film: function () {
      return film ? { paused: film.paused, muted: film.muted, t: film.currentTime,
                      ready: film.readyState, label: soundlbl.textContent } : null;
    },
    lang: function () { return lang; },
    install: function () { return installcmd.textContent; }
  };
})();
