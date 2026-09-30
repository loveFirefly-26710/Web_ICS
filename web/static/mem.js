// 顶栏内存指标。首页与文档页共用这一份，免得两处各写一遍轮询。
//
// 数字来自 /healthz 的 memMB，服务端向上取整到 50 MB 的倍数后才给出，
// 所以看到的是档位值而不是精确 RSS（精确值在 /debug/healthz，默认关闭）。
// warn / danger 由服务端的 memLevel 决定，前端不写死阈值：
// 软硬限都能用命令行改，写死必然对不上。
window.WebICSMem = {
  attach: function (el) {
    if (!el) { return; }

    function poll() {
      fetch('/healthz').then(function (r) { return r.json(); }).then(function (d) {
        el.textContent = '内存 ' + d.memMB + ' MB';
        el.classList.toggle('warn', d.memLevel === 'high');
        el.classList.toggle('danger', d.memLevel === 'critical');
      }).catch(function () {});
    }

    poll();
    setInterval(poll, 10000);
  }
};
