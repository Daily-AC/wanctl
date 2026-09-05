/* wanctl 的三张认证页共用的脚本：登录 / 等待邀请 / 设备授权。
   不能直接用 app.js —— 它在模块作用域里就去找 #grid / #who 这些只有 SPA
   才有的元素，拿到 null 之后下一句属性访问就抛，整个脚本死掉。
   所以这是独立的一小份，只做这三张页面真的需要的事。 */
(function () {
  'use strict';

  var $ = function (s) { return document.querySelector(s); };
  var $$ = function (s) { return Array.prototype.slice.call(document.querySelectorAll(s)); };

  /* ── 双语 ──────────────────────────────────────────────────────────
     跟 SPA 同一套机制、同一个 localStorage 键：在登录页选了中文，
     进了应用还是中文。

     注意变量部分（登录名、空间、指纹、授权码）都在自己的元素里，不进
     data-en/data-zh —— 那些属性是拿 innerHTML 写回去的，把服务端注进来的
     值放进去就等于给它开了第二次解码的机会。 */
  var lang = 'en';
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
    $$('[data-ph-en]').forEach(function (el) {
      el.setAttribute('placeholder', el.getAttribute('data-ph-' + l));
    });
    // 等待邀请页上「还有几天可以再申请」。数字由服务端算，句子在这里拼 ——
    // 「in 1 days」是错的英文，而单复数不是 data-en/data-zh 装得下的东西。
    var retry = $('#retry');
    if (retry) {
      var days = parseInt(retry.getAttribute('data-days'), 10) || 0;
      retry.textContent = l === 'en'
        ? (days === 1 ? 'You can ask again tomorrow.' : 'You can ask again in ' + days + ' days.')
        : (days === 1 ? '明天可以再申请。' : days + ' 天后可以再申请。');
    }
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }
  $('#lang').onclick = function () { applyLang(lang === 'en' ? 'zh' : 'en'); };

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  applyLang(saved === 'zh' || saved === 'en' ? saved
    : (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en');

  function csrf() {
    var p = document.cookie.split('; ').find(function (x) { return x.indexOf('wanctl_csrf=') === 0; });
    return p ? decodeURIComponent(p.slice('wanctl_csrf='.length)) : '';
  }
  function post(path, body) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf() },
      body: JSON.stringify(body || {})
    });
  }

  /* ── 设备授权页：复制授权码 ────────────────────────────────────────
     复制成功后「点一下复制」换成「已复制」，有效期那半句留着 —— 它在
     复制之后依然是这一行里唯一还会变的信息。 */
  var code = $('#acode');
  if (code && navigator.clipboard) {
    var copy = function () {
      navigator.clipboard.writeText(code.textContent.trim()).then(function () {
        $('#hint').hidden = true;
        $('#copied').hidden = false;
      });
    };
    code.onclick = copy;
    code.onkeydown = function (e) {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); copy(); }
    };
  }

  /* ── 等待邀请页：兑换邀请码 / 退出登录 ─────────────────────────── */
  var form = $('#redeem');
  if (form) {
    var err = $('#err');
    var btn = form.querySelector('button');
    var say = function (en, zh) { err.textContent = lang === 'en' ? en : zh; };

    form.onsubmit = function (e) {
      e.preventDefault();
      var v = $('#code').value.trim();
      err.textContent = '';
      if (!v) { say('Paste an invite code first.', '请先粘贴邀请码。'); return; }
      btn.disabled = true;
      post('/auth/redeem', { code: v }).then(function (r) {
        if (r.ok) { location.href = '/'; return; }
        return r.text().then(function (t) {
          btn.disabled = false;
          // 403 就是「这个码不行」，说人话；其余状态原样透出服务端的说明 ——
          // 它们少见，而少见的时候原因比语言一致更值钱。
          if (r.status === 403) say('That code was not accepted.', '这个邀请码没被接受。');
          else err.textContent = t.trim() || ('HTTP ' + r.status);
        });
      }).catch(function () {
        btn.disabled = false;
        say('Network error — try again.', '网络错误，请重试。');
      });
    };

    $('#out').onclick = function () {
      post('/auth/logout').then(function () { location.href = '/'; });
    };
  }

  /* ── 等待邀请页：申请访问 ───────────────────────────────────────────
     交完之后不在这里自己画「已提交」——重新加载，让服务端说它现在是什么
     状态。四种状态本来就是服务端算的，客户端再算一遍就是第二份真源。 */
  var ask = $('#ask');
  if (ask) {
    var askErr = $('#askErr');
    var askBtn = ask.querySelector('button');
    ask.onsubmit = function (e) {
      e.preventDefault();
      askErr.textContent = '';
      askBtn.disabled = true;
      post('/auth/request-access', { note: $('#note').value.trim() }).then(function (r) {
        if (r.ok) { location.reload(); return; }
        return r.text().then(function (t) {
          askBtn.disabled = false;
          var token = t.trim();
          // 中继用固定的错误 token 说话（形状同好友那套），认识的翻译成
          // 人话，不认识的原样透出——少见的时候原因比语言一致更值钱。
          if (token === 'request-open' || token === 'request-approved') { location.reload(); return; }
          if (token === 'request-cooldown') {
            askErr.textContent = lang === 'en'
              ? 'You asked recently. Try again later.' : '你刚申请过，过些天再来。';
            return;
          }
          askErr.textContent = token || ('HTTP ' + r.status);
        });
      }).catch(function () {
        askBtn.disabled = false;
        askErr.textContent = lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。';
      });
    };
  }
})();
