/* wanctl 的认证页共用的脚本：登录 / 绑定邮箱 / 确认邮箱 / 申请访问 / 设备授权。
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
    // 申请页上「还有几天可以再申请」。数字由服务端算，句子在这里拼 ——
    // 「in 1 days」是错的英文，而单复数不是 data-en/data-zh 装得下的东西。
    var retry = $('#retry');
    if (retry) {
      var days = parseInt(retry.getAttribute('data-days'), 10) || 0;
      retry.textContent = l === 'en'
        ? (days === 1 ? 'You can ask again tomorrow.' : 'You can ask again in ' + days + ' days.')
        : (days === 1 ? '明天可以再申请。' : days + ' 天后可以再申请。');
    }
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
    updateWebFetchPrompt();
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

  function updateWebFetchPrompt() {
    var prompt = $('#webfetchPrompt'), start = $('#webfetchStart');
    if (!prompt || !start) return;
    prompt.value = lang === 'zh'
      ? '请使用联网工具实际读取这个独立申请的完整 URL：\n' + start.dataset.url + '\n按真实响应返回 approval_url 和完整 continuation_prompt，等待我在 wanctl 中批准。批准后，使用同一个网页读取工具 GET 调用网址，完成我明确请求的任务；回复中保留完整 status_url 和 exec 的 call_url_template，供后续对话继续使用。调用说明：' + start.dataset.help + '\n不要模拟响应或代替我审批；授权完成前不要执行设备命令。'
      : 'Use your URL-reading tool to GET this complete URL for a new request:\n' + start.dataset.url + '\nReturn the real approval_url and complete continuation_prompt, then wait for my approval in wanctl. After approval, use the same URL-reading tool to GET call URLs for tasks I request. Keep the full status_url and exec call_url_template in your reply for later turns. Help: ' + start.dataset.help + '\nDo not simulate responses or approve on my behalf. Do not run device commands before approval.';
  }
  /* 「复制」按钮：把它指向的那段文字放进剪贴板。选不中剪贴板时退回选中
     文本 —— 手动 ⌘C 也是复制，比一句「复制失败」有用。 */
  function say(en, zh) {
    var hint = $('#webfetchCopyHint');
    if (hint) hint.textContent = lang === 'zh' ? zh : en;
  }
  $$('[data-copy]').forEach(function (btn) {
    btn.onclick = function () {
      var target = document.getElementById(btn.getAttribute('data-copy'));
      if (!target) return;
      var text = (target.textContent || '').trim();
      var fallback = function () {
        var range = document.createRange();
        range.selectNodeContents(target);
        var sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        say('Select the line above and copy it.', '请选中上面这行文字复制。');
      };
      if (!navigator.clipboard) { fallback(); return; }
      navigator.clipboard.writeText(text).then(function () {
        say('Copied. Paste it into your AI chat.', '已复制，粘贴到 AI 对话里即可。');
      }).catch(fallback);
    };
  });

  var webfetchCopy = $('#webfetchCopy');
  if (webfetchCopy) {
    webfetchCopy.onclick = function () {
      var bytes = new Uint8Array(24);
      crypto.getRandomValues(bytes);
      var nonce = Array.prototype.map.call(bytes, function (b) { return b.toString(16).padStart(2, '0'); }).join('');
      var start = $('#webfetchStart'), prompt = $('#webfetchPrompt');
      start.dataset.url = start.dataset.relay + '/webfetch/new/' + nonce;
      updateWebFetchPrompt();
      var hint = $('#webfetchCopyHint');
      if (!navigator.clipboard) { prompt.focus(); prompt.select(); return; }
      navigator.clipboard.writeText(prompt.value).then(function () {
        hint.textContent = lang === 'zh' ? '已复制新的接入提示词。' : 'Copied a fresh connection prompt.';
      }).catch(function () {
        prompt.focus(); prompt.select();
        hint.textContent = lang === 'zh' ? '已生成，请复制上方文字。' : 'A fresh prompt is ready; copy the selected text.';
      });
    };
  }

  /* WebFetch uses the same authenticated approval surface as other clients.
     GET displays the request; only this CSRF-protected POST grants access. */
  var delegate = $('#delegate');
  if (delegate) {
    var approve = $('#delegateApprove'), reject = $('#delegateReject');
    var delegateError = $('#delegateError');
    var minutes = $('#delegateMinutes');
    var maxMinutes = Number(minutes.getAttribute('max'));
    var presets = $$('#delegate .duration-set .chip');
    var selectedDevices = function () { return $$('#delegate input[name="device"]:checked'); };
    /* 自己填的分钟数才是提交出去的那个值；四颗 pill 只是往里写数。这样「选预设」
       和「自己填」走同一条路，服务端只看见一个 minutes。 */
    var validMinutes = function () {
      var n = Number(minutes.value);
      return minutes.value !== '' && n === Math.floor(n) && n >= 1 && n <= maxMinutes;
    };
    var updateApprove = function () {
      presets.forEach(function (b) {
        b.setAttribute('aria-pressed', b.dataset.minutes === minutes.value ? 'true' : 'false');
      });
      approve.disabled = !$('#delegateConfirm').checked || !selectedDevices().length || !validMinutes();
    };
    /* 改设备或改时长都作废那次确认：勾的是「我核对过这一份」，不是「我核对过指纹」。 */
    var changed = function (target) {
      if (target === minutes) {
        delegateError.textContent = validMinutes() ? ''
          : lang === 'en' ? 'Choose 1 to ' + maxMinutes + ' minutes.' : '请填 1 到 ' + maxMinutes + ' 分钟。';
      }
      if (target === minutes || target.name === 'device') $('#delegateConfirm').checked = false;
      updateApprove();
    };
    delegate.addEventListener('change', function (event) { changed(event.target); });
    delegate.addEventListener('input', function (event) { changed(event.target); });
    presets.forEach(function (button) {
      button.onclick = function () { minutes.value = button.dataset.minutes; changed(minutes); };
    });
    updateApprove();
    var decide = function (path, body) {
      approve.disabled = reject.disabled = true;
      delegateError.textContent = '';
      post(path, body).then(function (r) {
        if (r.ok) { location.reload(); return; }
        return r.text().then(function (msg) { throw new Error(msg.trim() || ('HTTP ' + r.status)); });
      }).catch(function (err) {
        delegateError.textContent = err.message || (lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。');
        reject.disabled = false;
        updateApprove();
      });
    };
    delegate.onsubmit = function (event) {
      event.preventDefault();
      var selected = selectedDevices(), fingerprints = {};
      if (!selected.length || !$('#delegateConfirm').checked) return;
      selected.forEach(function (input) { fingerprints[input.value] = input.dataset.fingerprint; });
      decide('/api/delegations/approve', {
        request_id: delegate.dataset.request, devices: selected.map(function (input) { return input.value; }),
        minutes: Number($('#delegateMinutes').value), confirmed: true,
        controller_fingerprint: delegate.dataset.controllerFingerprint, device_fingerprints: fingerprints
      });
    };
    reject.onclick = function () { decide('/api/delegations/reject', { request_id: delegate.dataset.request }); };
  }

  /* ── OAuth 同意页：允许 / 拒绝 ─────────────────────────────────────
     决定走 POST（同源 + 双提交 CSRF），服务端算好该跳去哪，这里只负责跳。
     不用 <form action> 直接往客户端的回调地址提交：那是跨站导航，CSP 的
     form-action 'self' 挡它，而放宽 CSP 只为省一次 fetch 是坏交易。 */
  var consent = $('#oauthConsent');
  if (consent) {
    var allow = $('#oauthAllow'), denyBtn = $('#oauthDeny'), consentErr = $('#oauthError');
    var decideOAuth = function (allowed) {
      allow.disabled = denyBtn.disabled = true;
      consentErr.textContent = '';
      post('/api/oauth/decide', { request_id: consent.dataset.request, allow: allowed })
        .then(function (r) {
          if (!r.ok) {
            return r.text().then(function (msg) { throw new Error(msg.trim() || ('HTTP ' + r.status)); });
          }
          return r.json().then(function (body) {
            if (!body.redirect) throw new Error(lang === 'en' ? 'No redirect returned.' : '服务端没有返回跳转地址。');
            location.href = body.redirect;
          });
        })
        .catch(function (err) {
          consentErr.textContent = err.message || (lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。');
          allow.disabled = denyBtn.disabled = false;
        });
    };
    consent.onsubmit = function (e) { e.preventDefault(); decideOAuth(true); };
    denyBtn.onclick = function () { decideOAuth(false); };
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

  /* ── 申请页：头像拉不到就露出首字母 ─────────────────────────────
     GitHub 的图床在有些网络里进不来。图片挂掉时把它拿走，底下那层首字母
     就是这一格的样子。事件可能在脚本跑起来之前就发生了，所以先查一次。 */
  var face = $('.me img');
  if (face) {
    var drop = function () { face.remove(); };
    if (face.complete && !face.naturalWidth) drop();
    else face.addEventListener('error', drop);
  }

  /* ── 申请页：退出登录 ─────────────────────────────────────────── */
  var signOut = $('#out');
  if (signOut) signOut.onclick = function () {
    post('/auth/logout').then(function () { location.href = '/'; });
  };

  /* ── 申请页：提交申请 ─────────────────────────────────────────────
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
          // 邮箱在门页已经绑过；走到这里说明它在别处被改了，回门页重走一遍。
          if (token === 'email-required') { location.href = '/auth/email?next=%2Fpending'; return; }
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

  /* ── 绑定邮箱（门页）────────────────────────────────────────────────
     错误全是 {"error": token} 的形状。认识的翻译成人话；限流的那两种
     带着还要等几秒，换算成分钟说出来。 */
  function emailErr(token, retry) {
    var mins = Math.max(1, Math.ceil((retry || 0) / 60));
    var hrs = Math.max(1, Math.ceil((retry || 0) / 3600));
    var say = {
      'email-invalid': ['That address does not look right.', '邮箱格式不对。'],
      'email-unchanged': ['That is already your address.', '这已经是你现在的邮箱了。'],
      'rate-address': ['A link just went to this address. Try again in ' + mins + (mins === 1 ? ' minute.' : ' minutes.'),
                       '刚给这个地址发过一封，' + mins + ' 分钟后再试。'],
      'rate-identity': ['That is the most links for one day. Try again in ' + hrs + (hrs === 1 ? ' hour.' : ' hours.'),
                        '今天发得太多了，' + hrs + ' 小时后再试。'],
      'mail-failed': ["We couldn't send it. Check the address, or try again later.", '信没发出去。检查一下地址，或者稍后再试。']
    }[token];
    if (say) return lang === 'en' ? say[0] : say[1];
    return token || (lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。');
  }
  function readErr(r) {
    return r.json().catch(function () { return {}; }).then(function (b) { return emailErr(b.error, b.retry_after); });
  }

  var emailForm = $('#emailForm');
  if (emailForm) {
    var body = document.body, next = body.getAttribute('data-next') || '/';
    var emailIn = $('#emailIn'), emailErrEl = $('#emailErr'), sentErr = $('#sentErr');
    var sendBtn = emailForm.querySelector('button');
    var send = function (address, errEl, btn) {
      errEl.textContent = '';
      btn.disabled = true;
      return post('/auth/email/send', { address: address, next: next }).then(function (r) {
        btn.disabled = false;
        if (r.ok) {
          return r.json().then(function (b) {
            $('#sentTo').textContent = b.address || address;
            body.setAttribute('data-req', 'sent');
            sentErr.textContent = '';
            watch();
          });
        }
        return readErr(r).then(function (msg) { errEl.textContent = msg; });
      }).catch(function () {
        btn.disabled = false;
        errEl.textContent = lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。';
      });
    };
    emailForm.onsubmit = function (e) {
      e.preventDefault();
      send(emailIn.value.trim(), emailErrEl, sendBtn);
    };
    $('#resend').onclick = function () {
      send($('#sentTo').textContent.trim(), sentErr, $('#resend'));
    };
    $('#change').onclick = function () {
      body.setAttribute('data-req', 'form');
      emailErrEl.textContent = '';
      emailIn.focus();
      emailIn.select();
    };
    /* 信发出以后，隔几秒问一次确认了没有；标签页藏起来就不问，回来时马上问一次。
       确认了就去原来要去的地方。 */
    var timer = null;
    var check = function () {
      if (document.hidden || body.getAttribute('data-req') !== 'sent') return;
      fetch('/auth/email/status').then(function (r) { return r.ok ? r.json() : {}; }).then(function (b) {
        if (b.confirmed) location.href = next;
      }).catch(function () {});
    };
    var watch = function () {
      if (!timer) timer = setInterval(check, 4000);
    };
    document.addEventListener('visibilitychange', check);
    if (body.getAttribute('data-req') === 'sent') watch();
  }

  /* ── 确认邮箱（信里那个链接）─────────────────────────────────────────
     打开页面什么都不做，按按钮才 POST。确认成功后：如果这个浏览器就是
     发信的那个账号，直接去原来要去的地方；否则（多半是手机）告诉人可以关了。 */
  var confirmForm = $('#confirmForm');
  if (confirmForm) {
    var confirmBtn = confirmForm.querySelector('button'), confirmErr = $('#confirmErr');
    confirmForm.onsubmit = function (e) {
      e.preventDefault();
      confirmBtn.disabled = true;
      confirmErr.textContent = '';
      post('/auth/email/confirm', { token: confirmForm.getAttribute('data-token') }).then(function (r) {
        if (r.ok) {
          return r.json().then(function (b) {
            if (b.next) { location.href = b.next; return; }
            document.body.setAttribute('data-req', 'done');
          });
        }
        return r.json().catch(function () { return {}; }).then(function (b) {
          if (b.error === 'token-used' || b.error === 'token-expired' || b.error === 'token-unknown') {
            document.body.setAttribute('data-req', b.error.slice('token-'.length));
            return;
          }
          confirmBtn.disabled = false;
          confirmErr.textContent = b.error || (lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。');
        });
      }).catch(function () {
        confirmBtn.disabled = false;
        confirmErr.textContent = lang === 'en' ? 'Network error — try again.' : '网络错误，请重试。';
      });
    };
  }
})();
