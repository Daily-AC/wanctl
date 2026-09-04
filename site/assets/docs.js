/* wanctl docs site — the only script on these pages.
   两件事：语言开关，和窄屏上把分组导航合起来。

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

  /* 分组导航在宽屏是左栏、窄屏是顶部一个可展开的清单。
     标记里它永远是 open：没有 JS 的时候看得见全部内容，只是长一点。
     窄屏上合起来 —— 十三条目录顶在每一页正文前面，读的人还没开始读就先滚一屏。 */
  var nav = $('#dnav');
  if (nav && window.matchMedia && window.matchMedia('(max-width:1099px)').matches) {
    nav.open = false;
  }
})();
