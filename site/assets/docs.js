/* wanctl docs site — the only script on these pages.
   两件事：语言开关，和窄屏上把分组导航合起来。

   语言用的是官网那套 data-en / data-zh + localStorage 'wanctl.lang'，
   同一个键，所以从官网切过来切过去是一件事而不是两件。
   只切外壳（顶栏、分组名、面包屑、目录标题、页脚）—— 正文这一轮不翻译，
   每篇文章自己带 lang 属性，读屏和断词按它走。 */
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
