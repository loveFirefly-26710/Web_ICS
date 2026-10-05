// 首页：左边是产品领域导航，右边是文档库列表。
//
// 统一检索：一个输入框、一次查询，同时命中标题和正文。服务端返回的
// items 里每条都带 source 字段（title 命中标题 / content 命中正文并带
// snippet 摘要），前端用同一套渲染逻辑，只在样式上区分。
//
// 分类和计数全部来自服务端 /api/categories，前端不硬编码分类规则。
(function () {
  'use strict';

  var ALL = [];            // 全部库
  var CATS = [];           // 全部分类（含"全部"伪分类）
  var TOTAL_LIBS = 0;      // 文档包总数
  var activeCat = 'all';   // 当前选中的领域
  var query = '';          // 搜索词
  var searching = false;   // 是否处于搜索结果态
  var page = 1;            // 当前页码（服务端分页，从 1 开始）
  var sortKey = '';        // 排序字段
  var sortAsc = true;      // 排序方向
  var inflight = null;     // 当前搜索的 AbortController（用于取消过期请求）
  var seq = 0;             // 请求序号，防止旧响应覆盖新结果

  var rowsEl = document.getElementById('librows');
  var statusEl = document.getElementById('status');
  var catListEl = document.getElementById('catlist');
  var titleEl = document.getElementById('listTitle');
  var qEl = document.getElementById('q');
  var metaEl = document.getElementById('meta');
  var searchPane = document.getElementById('searchPane');
  var searchStat = document.getElementById('searchStat');
  var searchResults = document.getElementById('searchResults');
  var searchPager = document.getElementById('searchPager');
  var searchHint = document.getElementById('searchHint');
  var listPane = document.getElementById('listPane');

  // esc 转义 HTML 特殊字符，同时覆盖文本上下文与属性上下文：
  // 有地方把结果拼进 data-* 属性，只转义尖括号挡不住引号闭合。
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // ---------- 左侧领域导航 ----------

  function renderCats() {
    catListEl.innerHTML = '';
    for (var i = 0; i < CATS.length; i++) {
      var c = CATS[i];
      var li = document.createElement('li');
      li.className = 'catitem' + (c.key === activeCat ? ' active' : '');
      li.dataset.key = c.key;
      li.innerHTML =
        '<span class="catname">' + esc(c.name) + '</span>' +
        '<span class="catcount">' + c.count + '</span>';
      li.addEventListener('click', (function (key) {
        return function () {
          activeCat = key;
          renderCats();
          // 换领域相当于换了一个结果集，页码必须回到第 1 页，
          // 否则会请求到超出新结果范围的第 N 页。
          if (searching) { runSearch(1); } else { render(); }
        };
      })(c.key));
      catListEl.appendChild(li);
    }
  }

  // ---------- 文档库列表（表格） ----------

  function visible() {
    var q = query.toLowerCase();
    var list = ALL.filter(function (l) {
      if (activeCat !== 'all' && l.categoryKey !== activeCat) return false;
      if (!q) return true;
      return (l.libName || '').toLowerCase().indexOf(q) >= 0 ||
             (l.libId || '').toLowerCase().indexOf(q) >= 0 ||
             (l.productType || '').toLowerCase().indexOf(q) >= 0 ||
             (l.productVersion || '').toLowerCase().indexOf(q) >= 0;
    });
    if (sortKey) {
      list.sort(function (a, b) {
        var va = (a[sortKey] == null ? '' : String(a[sortKey])).toLowerCase();
        var vb = (b[sortKey] == null ? '' : String(b[sortKey])).toLowerCase();
        var r = va < vb ? -1 : (va > vb ? 1 : 0);
        return sortAsc ? r : -r;
      });
    }
    return list;
  }

  function render() {
    var list = visible();
    var catName = '全部文档库';
    for (var i = 0; i < CATS.length; i++) {
      if (CATS[i].key === activeCat) { catName = CATS[i].name; break; }
    }
    titleEl.textContent = catName;

    if (!list.length) {
      rowsEl.innerHTML = '<div class="blank">' +
        (ALL.length ? '没有匹配的文档库' : '未发现任何文档包，请检查文档库目录') + '</div>';
      statusEl.textContent = '';
      return;
    }
    statusEl.textContent = list.length + ' 个文档包';

    var frag = document.createDocumentFragment();
    for (var j = 0; j < list.length; j++) {
      frag.appendChild(makeRow(list[j]));
    }
    rowsEl.innerHTML = '';
    rowsEl.appendChild(frag);
  }

  // dash 在字段缺失时填占位符，并挂一条说明。
  // 少数 HedEx 2.0 导出的 .hwics 包里没有文档版本这个字段，格子里只能是空的。
  // 不加说明的话看起来像界面坏了。说明挂在单元格自己身上：行的 title 已经
  // 被占用了，子元素的 title 在悬停时优先显示。
  function dash(el, value, what) {
    if (value) {
      el.textContent = value;
      return;
    }
    el.textContent = '—';
    el.title = '该文档包未提供' + what;
  }

  // makeRow 构造表格的一行：名称（含文档图标）/ 产品版本 / 文档版本 / 日期。
  // 不含复选框：本阅读器不提供批量管理操作。
  function makeRow(lib) {
    var row = document.createElement('div');
    row.className = 'librow';

    var name = document.createElement('span');
    name.className = 'td td-name';
    name.innerHTML = '<span class="docicon" aria-hidden="true"></span>' +
      '<span class="docname">' + esc(lib.libName || lib.libId) + '</span>';

    var pv = document.createElement('span');
    pv.className = 'td td-ver';
    dash(pv, lib.productVersion, '产品版本');

    var dv = document.createElement('span');
    dv.className = 'td td-docver';
    dash(dv, lib.libVersion, '文档版本');

    var dt = document.createElement('span');
    dt.className = 'td td-date';
    dash(dt, lib.issueDate, '日期');

    row.appendChild(name);
    row.appendChild(pv);
    row.appendChild(dv);
    row.appendChild(dt);

    row.title = (lib.libName || '') + (lib.topicNumber ? '\n' + lib.topicNumber + ' 篇' : '');
    row.addEventListener('click', function () {
      location.href = '/doc/' + encodeURIComponent(lib.libId) + '/';
    });
    return row;
  }

  // 表头排序
  Array.prototype.forEach.call(document.querySelectorAll('.th[data-sort]'), function (th) {
    th.addEventListener('click', function () {
      var k = th.dataset.sort;
      if (sortKey === k) { sortAsc = !sortAsc; } else { sortKey = k; sortAsc = true; }
      Array.prototype.forEach.call(document.querySelectorAll('.th'), function (x) {
        x.classList.remove('asc', 'desc');
      });
      th.classList.add(sortAsc ? 'asc' : 'desc');
      if (!searching) render();
    });
  });

  // ---------- 统一检索 ----------
  //
  // 没有"模式"切换，只有"范围"收窄：范围由页面上下文决定，
  // 首页搜全库、文档页搜本文档。

  function showSearchPane(on) {
    searching = on;
    searchPane.hidden = !on;
    listPane.hidden = on;
    if (!on) {
      searchPager.hidden = true;
      searchHint.hidden = true;
      searchHint.textContent = '';
    }
  }

  // runSearch 发起一次检索。newPage 为要请求的页码（默认 1）。
  function runSearch(newPage) {
    if (!query) { cancelSearch(); showSearchPane(false); render(); return; }
    // 取消上一次仍在飞的请求：正文扫描可能好几秒，用户中途改词、改范围、
    // 翻页时旧响应必须被丢弃，否则会出现「新查询的结果被旧查询覆盖」。
    // 顺带避免并发扫描。
    cancelSearch();
    showSearchPane(true);
    page = newPage || 1;
    doSearch();
  }

  // cancelSearch 中止当前在飞的搜索请求（若有）。
  function cancelSearch() {
    if (inflight) {
      try { inflight.abort(); } catch (e) { /* 老浏览器忽略 */ }
      inflight = null;
    }
    seq++; // 作废所有更早的响应
  }

  // newReq 为一个新的搜索请求登记 AbortController，并返回本次请求的序号。
  // 调用方应在响应回调里比对序号，确保只渲染最新一次查询的结果。
  function newReq() {
    inflight = (typeof AbortController !== 'undefined') ? new AbortController() : null;
    return seq;
  }

  // searchFailed 统一处理搜索异常，区分「用户主动取消」与「真的连不上」。
  //
  // 「Failed to fetch」是 fetch 的 TypeError，含义很广：服务没起、端口变了、
  // 请求被中断。直接甩给用户没有意义，这里给出可操作的原因与建议。
  function searchFailed(e, mySeq, t0) {
    if (mySeq !== seq) return;                  // 已被更新的查询取代，静默丢弃
    if (e && e.name === 'AbortError') return;   // 用户主动取消，不算失败
    var msg = (e && e.message) ? e.message : String(e);
    var hint = '请确认服务仍在运行（命令行窗口未关闭），然后刷新页面重试。';
    if (/Failed to fetch|NetworkError|Load failed/i.test(msg)) {
      msg = '无法连接服务';
    }
    searchStat.textContent = '';
    searchPager.hidden = true;
    searchHint.hidden = true;
    searchResults.innerHTML = '<div class="blank">检索失败：' + esc(msg) +
      (t0 ? '（' + (Date.now() - t0) + ' ms）' : '') +
      '<br><span class="fail-hint">' + esc(hint) + '</span></div>';
  }

  function catParam() { return activeCat === 'all' ? '' : '&cat=' + encodeURIComponent(activeCat); }

  // doSearch 打统一的 /api/search。
  //
  // 服务端一次返回：本页 items + 精确 total + 分页信息 + 正文覆盖情况。
  function doSearch() {
    var mySeq = newReq();
    var t0 = Date.now();
    searchStat.textContent = '检索标题与正文中…';
    searchResults.innerHTML = '<div class="blank">检索中…</div>';
    searchPager.hidden = true;
    searchHint.hidden = true;

    var url = '/api/search?q=' + encodeURIComponent(query) +
      '&page=' + page + catParam();
    var opt = inflight ? { signal: inflight.signal } : undefined;

    fetch(url, opt)
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (mySeq !== seq) return;   // 已有更新的查询，丢弃本次结果
        if (d.error) {
          searchStat.textContent = '';
          searchHint.hidden = true;
          searchResults.innerHTML = '<div class="blank">' + esc(d.error) + '</div>';
          return;
        }
        renderSearchStat(d, t0);
        var items = d.items || [];
        if (!items.length) {
          renderEmpty(d);
          return;
        }
        renderHits(items);
        renderPager(d);
      })
      .catch(function (e) { searchFailed(e, mySeq, t0); });
  }

  // renderSearchStat 拼状态栏：命中总数 + 来源分布 + 覆盖范围。
  //
  // 总数放在最前面，用同一个词「结果」统一口径。total 是精确的全量命中数，
  // 不是「取回了多少条」。
  function renderSearchStat(d, t0) {
    var c = d.content || {};
    var bits = [];

    var tN = d.titleN || 0;
    var cN = c.enabled ? (c.matched || 0) : 0;
    var total = d.total || 0;
    // 语料库覆盖全部包时，正文命中数是精确的全量值，任何一页都能说「找到 N 条」；
    // 只有在语料还没建完（或退回逐包扫描兜底）时，第 2 页起才是近似值。
    var exact = !!(d.corpus && d.corpus.enabled && !c.incomplete);
    if (page === 1 || exact) {
      bits.push('找到 ' + total + ' 条结果');
      if (page > 1) { bits.push('第 ' + page + ' 页'); }
    } else {
      bits.push('到本页已确认 ' + total + ' 条');
      bits.push('第 ' + page + ' 页');
    }
    // titleN 是去重后的唯一主题数（同一篇文档被多个包收录只算一次），
    // 因此措辞用「篇」而非「条」，避免与上面的「条结果」混淆。
    bits.push('标题 ' + tN + ' 篇');
    if (c.enabled) {
      bits.push('正文 ' + cN + ' 篇');
    }
    if (c.enabled && c.totalLibs) {
      // 措辞是「覆盖」而不是「已扫描」：有磁盘语料库之后，正文段一次请求
      // 就扫完全部文档包，不再是一点一点推进的进度。
      bits.push('正文覆盖 ' + (c.scannedLib || 0) + '/' + c.totalLibs + ' 个文档包');
    }
    bits.push('耗时 ' + (d.elapsedMs || 0) + ' ms');
    searchStat.textContent = bits.join(' · ');

    // 覆盖范围提示。有语料库时正文段每次查询都扫全部文档包，一次请求就是
    // 全库，总数精确、翻页确定；只有在语料还在后台建立时才如实说明
    // 「已就绪 X/N」。
    var cp = d.corpus || {};
    if (cp.enabled) {
      var ready = cp.ready || 0;
      var all = cp.total || 0;
      searchHint.hidden = false;
      searchHint.textContent = ready >= all
        ? '正文检索覆盖全部 ' + all + ' 个文档包（磁盘语料库），结果与命中总数均为精确值。'
        : '正文语料正在建立：已就绪 ' + ready + '/' + all + ' 个文档包' +
          (cp.building ? '，建完后正文检索即覆盖全部文档。' : '。');
    } else if (c.enabled && c.incomplete) {
      var covered = c.scannedLib || 0;
      var totalLibs = c.totalLibs || 0;
      searchHint.hidden = false;
      searchHint.textContent = '标题检索覆盖全部 ' + totalLibs + ' 个文档包；' +
        '正文按文档包逐个扫描，本次已覆盖 ' + covered + '/' + totalLibs + ' 个' +
        (covered < totalLibs ? '，继续翻页会覆盖更多文档包。' : '。');
    } else {
      searchHint.hidden = true;
      searchHint.textContent = '';
    }

    // 正文没扫完时用 title 属性说清楚「还有更多可以继续找」，
    // 而不是暗示"结果到此为止"。
    if (cp.enabled) {
      searchStat.title = (cp.ready || 0) >= (cp.total || 0)
        ? '正文段扫的是磁盘语料库（预先抽取好的纯文本），每次查询都覆盖全部 ' +
          (cp.total || 0) + ' 个文档包。'
        : '正文语料还在后台建立，已就绪 ' + (cp.ready || 0) + '/' +
          (cp.total || 0) + ' 个文档包。';
    } else {
      searchStat.title = c.incomplete
        ? '正文检索按文档包逐包推进：本次已扫描 ' + (c.scannedLib || 0) + '/' +
          (c.totalLibs || 0) + ' 个包，' +
          (c.moreLibs
            ? '还有 ' + Math.max(0, (c.totalLibs || 0) - (c.scannedLib || 0)) +
              ' 个包没轮到，继续翻页会接着往下扫。'
            : '本次扫描未覆盖整个包的末尾，继续翻页会接着往下扫。') +
          (page === 1 ? '想更快看到结果，可选定左侧产品领域来收窄范围。' : '')
        : '';
    }
  }

  // renderEmpty 无结果时的提示。区分「范围内确实没有」与「还没扫到」。
  function renderEmpty(d) {
    var c = d.content || {};
    var msg;
    if (c.enabled && c.incomplete) {
      msg = '已扫描的范围内没有找到「' + esc(query) + '」。正文是逐包扫描的，' +
        '可以点「下一页」接着往下找。';
    } else {
      msg = '没有找到「' + esc(query) + '」。可以换个更短的关键词试试。';
    }
    searchResults.innerHTML = '<div class="blank">' + msg + '</div>';
    searchPager.hidden = true;
  }

  // renderHits 渲染本页结果。
  //
  // 标题命中与正文命中用同一套结构，靠 source 加一个类型标记与不同的样式类。
  // 标题命中显示面包屑，若该主题被多个文档包收录，则列出这些包。
  // 正文命中显示高亮摘要。
  function renderHits(items) {
    var frag = document.createDocumentFragment();
    for (var i = 0; i < items.length; i++) {
      var h = items[i];
      var isContent = h.source === 'content';

      var a = document.createElement('a');
      a.className = 'sr' + (isContent ? ' sr-content' : ' sr-title-hit');
      // 结果落到阅读页（/doc/{lib}/），不是裸文档页（/doc/{lib}/res/…）。
      //
      // 这是关键区别：阅读页带着顶栏（Web_ICS 回首页、面包屑、上下一篇）、
      // 左侧目录树，以及限定在本文档包内的搜索框。直接落到裸文档页，
      // 用户点进去就再也没有回首页的入口，也没法在当前文档里继续搜。
      //
      // node / url 是给阅读页做深链定位用的：阅读页据此把目录树逐层展开
      // 到这一篇并选中。标题命中带 nodeId（精确），正文命中只有 URL。
      a.href = docURL(h.libId, h.url, isContent ? '' : h.nodeId);

      var t = document.createElement('div');
      t.className = 'sr-title';
      // 类型标记只区分「标题命中 / 正文命中」两类来源
      var bodyHit = (h.count || 0) > 0;
      var tag = document.createElement('span');
      tag.className = 'sr-tag ' + (isContent ? 'sr-tag-content' : 'sr-tag-title');
      tag.textContent = isContent ? '正文' : '标题';
      t.appendChild(tag);
      t.appendChild(document.createTextNode(h.title || h.url));
      a.appendChild(t);

      // 正文摘要紧跟在标题下面，次序与官方阅读器一致（标题 / 摘要 / 路径）。
      //
      // 标题命中也有摘要：否则首屏约 3/4 是标题命中，整页看起来就是一堆
      // 光秃秃的标题，必须逐条点进去才知道讲什么。服务端会按 URL 去语料里
      // 取这篇的正文；正文里没有检索词时给的是文档开头一段
      // （sr-snippet-lead，颜色略淡）。
      if (h.snippet) {
        var sn = document.createElement('div');
        sn.className = 'sr-snippet' + (bodyHit ? '' : ' sr-snippet-lead');
        sn.innerHTML = h.snippet; // 服务端已做 HTML 转义与 <em> 高亮
        a.appendChild(sn);
      }

      var m = document.createElement('div');
      m.className = 'sr-meta';
      var bits = [h.libName];
      if (h.breadth) { bits.push(h.breadth); }
      if (bodyHit && h.count > 1) { bits.push('命中 ' + h.count + ' 次'); }
      m.textContent = bits.join('  ›  ');
      a.appendChild(m);

      // 同主题被多个包收录时，列出这些包（替代「同一文档刷屏多行」）。
      if (!isContent && h.groups && h.groups.length > 1) {
        var gl = document.createElement('div');
        gl.className = 'sr-groups';
        var lbl = document.createElement('span');
        lbl.className = 'sr-groups-label';
        lbl.textContent = '收录于 ' + h.groups.length + ' 个文档包：';
        gl.appendChild(lbl);
        var ul = document.createElement('ul');
        for (var g = 0; g < h.groups.length; g++) {
          var grp = h.groups[g];
          var li = document.createElement('li');
          var ga = document.createElement('a');
          ga.href = docURL(grp.libId, grp.url, h.nodeId);
          ga.textContent = grp.libName || grp.libId;
          ga.addEventListener('click', function (ev) { ev.stopPropagation(); });
          li.appendChild(ga);
          ul.appendChild(li);
        }
        gl.appendChild(ul);
        a.appendChild(gl);
      }

      frag.appendChild(a);
    }
    searchResults.innerHTML = '';
    searchResults.appendChild(frag);
  }

  // renderPager 渲染分页控件。
  function renderPager(d) {
    var p = d.page || {};
    if (!p.hasPrev && !p.hasNext) { searchPager.hidden = true; return; }

    searchPager.hidden = false;
    searchPager.innerHTML = '';

    function btn(label, target, on) {
      var b = document.createElement('button');
      b.className = 'pgbtn' + (on ? '' : ' pgbtn-off');
      b.textContent = label;
      b.disabled = !on;
      if (on) {
        b.addEventListener('click', function () {
          runSearch(target);
          searchPane.scrollIntoView({ block: 'start' });
        });
      }
      return b;
    }

    searchPager.appendChild(btn('首页', 1, p.hasPrev));
    searchPager.appendChild(btn('上一页', (p.pageIndex || 1) - 1, p.hasPrev));

    var info = document.createElement('span');
    info.className = 'pginfo';
    info.textContent = '第 ' + (p.pageIndex || 1) + ' 页';
    searchPager.appendChild(info);

    searchPager.appendChild(btn('下一页', (p.pageIndex || 1) + 1, p.hasNext));
  }

  // docURL 拼「阅读页 + 深链」的地址。
  //
  // 形如 /doc/{libId}/?url=admin%2Fxxx.html&node=xxx
  //   url：文档在包内的相对路径（相对 resources/），阅读页据此打开正文
  //   node：目录节点 ID，阅读页据此展开目录树并选中（可选）
  //
  // url 整段编码（斜杠也编成 %2F），交给阅读页用 URLSearchParams 解回原值，
  // 避免包内路径里的 # 被浏览器当成锚点截断。
  function docURL(libId, url, nodeId) {
    var u = '/doc/' + encodeURIComponent(libId) + '/?url=' + encodeURIComponent(url || '');
    if (nodeId) { u += '&node=' + encodeURIComponent(nodeId); }
    return u;
  }

  document.getElementById('btn-search').addEventListener('click', function () {
    query = qEl.value.trim();
    runSearch(1);
  });

  var timer = null;
  qEl.addEventListener('input', function () {
    query = qEl.value.trim();
    clearTimeout(timer);
    // 标题段虽轻，正文段要扫全库，整体是重操作，所以统一等用户停手再发请求。
    timer = setTimeout(function () { runSearch(1); }, 500);
  });
  qEl.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter') return;
    query = qEl.value.trim();
    runSearch(1);
  });

  // Esc 清空并回到列表
  qEl.addEventListener('keyup', function (e) {
    if (e.key === 'Escape') {
      qEl.value = ''; query = '';
      showSearchPane(false);
      render();
    }
  });

  // ---------- 内存指标 ----------

  WebICSMem.attach(document.getElementById('mem'));

  // ---------- 加载 ----------

  Promise.all([
    fetch('/api/libs').then(function (r) { return r.json(); }),
    fetch('/api/categories').then(function (r) { return r.json(); })
  ]).then(function (res) {
    ALL = res[0].items || [];
    TOTAL_LIBS = ALL.length;
    var cats = res[1].items || [];
    CATS = [{ key: 'all', name: '全部', count: ALL.length }].concat(cats);
    metaEl.textContent = ALL.length + ' 个文档包 · ' + cats.length + ' 个领域';
    renderCats();
    render();
  }).catch(function (e) {
    statusEl.textContent = '加载失败：' + e;
  });
})();
