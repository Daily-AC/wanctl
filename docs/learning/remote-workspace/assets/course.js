/* Local teaching interactions only: no requests, analytics or learner records. */
(function () {
  'use strict';
  document.querySelectorAll('.course-check').forEach(function (box) {
    var choices = Array.from(box.querySelectorAll('[data-choice]'));
    var feedback = box.querySelector('.course-feedback');
    var explanation = box.querySelector('details p');
    choices.forEach(function (button) {
      button.addEventListener('click', function () {
        choices.forEach(function (b) { b.setAttribute('aria-pressed', String(b === button)); });
        var correct = button.dataset.choice === box.dataset.correct;
        feedback.textContent = (correct ? '这个判断符合本篇的架构边界。' : '再看状态或权限由谁持有。') + explanation.textContent;
      });
    });
  });
  var events = {
    disconnect: '网络中断：设备进程仍活着时，原 shell 和已受理任务可以继续。重连后使用原引用和命令编号查询。',
    controller: '控制端重启：远端状态可能仍在。调用方需要保存的引用重新附着，不能让服务器按账号猜测当前工作区。',
    agent: '设备端重启：当前实现丢失 shell 和执行账本。项目文件不因此自动删除，但不能假装原来的内存状态已恢复。',
    exit: '显式退出：工作区关闭。旧引用不能继续工作；调用方结束自己的绑定。'
  };
  document.querySelectorAll('.course-lifetime').forEach(function (box) {
    box.querySelectorAll('[data-event]').forEach(function (button) {
      button.addEventListener('click', function () {
        box.querySelectorAll('[data-event]').forEach(function (b) { b.setAttribute('aria-pressed', String(b === button)); });
        box.querySelector('.course-event-result').textContent = events[button.dataset.event];
      });
    });
  });
  var printDetails = [];
  window.addEventListener('beforeprint', function () {
    printDetails = Array.from(document.querySelectorAll('.course-check details')).map(function (el) { return {el: el, open: el.open}; });
    printDetails.forEach(function (item) { item.el.open = true; });
  });
  window.addEventListener('afterprint', function () { printDetails.forEach(function (item) { item.el.open = item.open; }); });
}());
