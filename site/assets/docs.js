/* wanctl docs site — the only script on these pages.
   三件事：语言开关、窄屏上的导航抽屉、右栏跟着读到哪一节走。

   语言用的是官网那套 data-en / data-zh + localStorage 'wanctl.lang'，
   同一个键，所以从官网切过来切过去是一件事而不是两件。
   切的是整页：外壳（顶栏、分组名、面包屑、目录标题、翻页、页脚、<title>）走
   data-en / data-zh，正文和本页目录各有两份、由 data-lang 挑一份出来
   （tools/docsite/build.py 生成）。两件事在同一次 applyLang 里做完，
   所以不会出现外壳已经中文、正文还是英文的中间态。
   URL 不动：一页两份正文，切语言不跳转、不刷新。 */
(function () {
  'use strict';
  var $ = function (s) { return document.querySelector(s); };
  var $$ = function (s) { return Array.prototype.slice.call(document.querySelectorAll(s)); };

  var btn = $('#lang');
  var lang = 'en';

  function applyLang(l) {
    lang = l;
    document.documentElement.lang = l === 'zh' ? 'zh-CN' : 'en';
    if (btn) btn.textContent = l === 'en' ? '中文' : 'EN';
    $$('[data-en]').forEach(function (el) {
      var v = el.getAttribute('data-' + l);
      if (v != null) el.innerHTML = v;
    });
    /* 两份正文、两份目录，露出对得上的那一份。
       用 hidden 而不是 style.display：它是「这份内容现在不适用」的语义，
       读屏和页内查找都跟着走，CSS 那边一条 [hidden] 就够。 */
    $$('[data-lang]').forEach(function (el) {
      el.hidden = el.getAttribute('data-lang') !== l;
    });
    try { localStorage.setItem('wanctl.lang', l); } catch (_) {}
  }

  if (btn) {
    btn.addEventListener('click', function () { applyLang(lang === 'en' ? 'zh' : 'en'); });
  }

  var saved = null;
  try { saved = localStorage.getItem('wanctl.lang'); } catch (_) {}
  if (saved === 'zh' || (!saved && /^zh/i.test(navigator.language || ''))) applyLang('zh');

  /* ── 窄屏的导航抽屉 ────────────────────────────────────────────────
     CSS 里每一条抽屉规则都挂在 `<html class="js">` 上，所以没有脚本的时候
     导航仍旧是正文上面那一整份清单，而不是一块被推到画外、谁也叫不回来的面板。
     那个 class 由 <head> 里的一行加上（build.py 的 `head()`）—— 本文件在
     </body> 前才执行，等到这里再加，手机上会先闪一屏摊开的目录。 */
  var root = document.documentElement;

  var nav = $('#dnav');
  var opener = $('.dnav-open');
  var scrim = $('.dscrim');
  var open = false;

  function setNav(v, fromPop) {
    if (v === open) return;
    open = v;
    root.classList.toggle('dnav-on', v);
    if (opener) opener.setAttribute('aria-expanded', v ? 'true' : 'false');
    if (v) {
      /* 推一条历史记录，好让手机的返回键关抽屉而不是离开这一页。 */
      history.pushState({ dnav: 1 }, '');
      var first = nav && nav.querySelector('a');
      if (first) first.focus();
    } else {
      if (!fromPop && history.state && history.state.dnav) history.back();
      if (opener) opener.focus();
    }
  }

  if (opener) opener.addEventListener('click', function () { setNav(true); });
  if (scrim) scrim.addEventListener('click', function () { setNav(false); });
  var closer = $('.dnav-close');
  if (closer) closer.addEventListener('click', function () { setNav(false); });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && open) setNav(false);
  });
  window.addEventListener('popstate', function () { setNav(false, true); });

  /* 在抽屉里选一篇：走 location.replace，把「抽屉开着」那条历史记录换掉，
     而不是叠在它上面。否则从新文章按返回，会先回到同一页的一个幽灵条目。
     带修饰键的点击（新标签页）不拦。 */
  if (nav) {
    nav.addEventListener('click', function (e) {
      var a = e.target.closest && e.target.closest('a');
      if (!open || !a || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button) return;
      e.preventDefault();
      location.replace(a.href);
    });
  }

  /* ── 右栏跟着读到哪一节走 ──────────────────────────────────────────
     标出正文里最后一个已经越过读区上沿的标题。

     116 这个数不是估的：点右栏一条，标题停在吸顶条 48 加 scroll-margin-top 64
     ＝ 112 的位置，所以线要画在它下面一点。

     **没有用 IntersectionObserver**，试过，它在这件事上是错的工具：它只在标题
     穿过某条线的那一刻回调，而锚点跳转的落点在最后一次穿越之后 —— 标题停在 112
     就不动了，越线的回调早就发完了。实测点「Residual boundaries」，亮的还是上
     一节「Findings」。改成 rAF 节流的 scroll：一帧最多算一次，算的是十几个
     标题的 rect，只在滚动时发生。
     语言一切，隐藏那半边的标题 offsetParent 是 null，自动不参与。 */
  var heads = $$('.dbody article :is(h2,h3)[id]');
  if (heads.length) {
    var tocLinks = $$('.dtoc a[href^="#"]');
    var ticking = false;
    var mark = function () {
      ticking = false;
      var cur = '';
      heads.forEach(function (h) {
        if (h.offsetParent && h.getBoundingClientRect().top <= 116) cur = h.id;
      });
      tocLinks.forEach(function (a) {
        a.classList.toggle('now', !!cur && decodeURIComponent(a.hash.slice(1)) === cur);
      });
    };
    addEventListener('scroll', function () {
      if (!ticking) { ticking = true; requestAnimationFrame(mark); }
    }, { passive: true });
    mark();
  }
})();
