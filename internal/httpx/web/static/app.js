// 面板的全部脚本：按 data-refresh 指定的片段做增量刷新。
//
// 三条原则：
//  1. 标签页在后台就完全停止请求——玩客云不该为一个看不见的页面耗 CPU；
//  2. 不用模板字符串在客户端拼 HTML，所有渲染都在服务端完成（省内存、少一处 XSS 面）；
//  3. 刷新失败就当没发生，下一轮再试，不弹错误打扰人。
(function () {
  // 封面加载失败时换成番剧首字的占位块。
  // 用捕获阶段监听：片段刷新插进来的图片也能覆盖到，不必逐张绑定。
  document.addEventListener('error', function (e) {
    var img = e.target;
    if (!img || img.tagName !== 'IMG' || !img.dataset.fallback) return;
    var span = document.createElement('span');
    span.className = (img.getAttribute('class') || '') + ' poster-blank';
    span.textContent = img.dataset.fallback;
    if (img.parentNode) img.parentNode.replaceChild(span, img);
  }, true);

  var nodes = Array.prototype.slice.call(document.querySelectorAll('[data-refresh]'));
  if (!nodes.length) return;

  nodes.forEach(function (n) { n.dataset.last = '0'; });

  function tick() {
    if (document.hidden) return;
    var now = Date.now();
    nodes.forEach(function (n) {
      var every = Number(n.dataset.every || 5000);
      if (now - Number(n.dataset.last || 0) < every) return;
      n.dataset.last = String(now);
      fetch(n.dataset.refresh, { credentials: 'same-origin', cache: 'no-store' })
        .then(function (r) { return r.ok ? r.text() : null; })
        .then(function (html) { if (html !== null) n.innerHTML = html; })
        .catch(function () { /* 网络抖动，忽略 */ });
    });
  }

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) tick();
  });

  // 2 秒的心跳只做本地判断，真正发请求的节奏由每个片段的 data-every 决定。
  setInterval(tick, 2000);
  tick();
})();