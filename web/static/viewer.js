// 阅读页：目录树分级懒加载，展开时才向服务端要该层子节点。
// 前端因此不用一次性持有整棵树，跟服务端的内存策略一致。
(function () {
  'use strict';

  var LIB_ID = decodeURIComponent(location.pathname.replace(/^\/doc\//, '').replace(/\/.*$/, ''));
  var treeEl = document.getElementById('tree');
  var frame = document.getElementById('frame');
  var crumb = document.getElementById('crumb');
  var sidebar = document.getElementById('sidebar');

  // 深链参数：从首页搜索结果点进来时带。
  //   url：文档在包内的相对路径（相对 resources/），正文命中用
  //   node：目录节点 ID，标题命中用（比 url 更精确，能直接定位到树节点）
  // 有其一就把目录树展开到那一篇并选中；都没有则打开第一篇。
  var DEEP = (function () {
    var q = new URLSearchParams(location.search);
    return { url: q.get('url') || '', node: q.get('node') || '' };
  })();

  // 扁平状态：id -> {node, el, kids, twisty, loaded, expanded}
  var state = {};
  var order = [];      // 深度优先的可见顺序，用于上/下篇导航
  var current = null;  // 当前选中的节点

  function api(path, params) {
    var q = new URLSearchParams(params || {}).toString();
    return fetch(path + (q ? '?' + q : '')).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    });
  }

  // esc 转义 HTML 特殊字符，同时覆盖文本上下文与属性上下文：
  // 面包屑把节点 ID 拼进 data-goto 属性，只转义尖括号挡不住引号闭合。
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function resURL(node) {
    // 目录里的 url 是相对 resources/ 的路径。
    // 逐段 encodeURIComponent：文件名里可能含空格、# 等字符，
    // 直接 encodeURI 会漏掉 #（会被当成锚点截断）。
    var segs = String(node.url).split('/');
    var enc = [];
    for (var i = 0; i < segs.length; i++) {
      if (segs[i] === '') continue;
      // 保留目录条目自带的 #锚点
      var hashAt = segs[i].indexOf('#');
      if (hashAt >= 0) {
        enc.push(encodeURIComponent(segs[i].slice(0, hashAt)) + segs[i].slice(hashAt));
      } else {
        enc.push(encodeURIComponent(segs[i]));
      }
    }
    return '/doc/' + encodeURIComponent(LIB_ID) + '/res/' + enc.join('/');
  }

  function makeRow(node) {
    var row = document.createElement('div');
    row.className = 'node';
    row.dataset.id = node.id;

    var twisty = document.createElement('span');
    twisty.className = 'twisty' + (node.hasKid ? '' : ' empty');
    twisty.textContent = node.hasKid ? '▸' : '';

    var label = document.createElement('span');
    label.className = 'label';
    label.textContent = node.title || node.url || node.id;
    label.title = node.title || '';

    row.appendChild(twisty);
    row.appendChild(label);

    var kids = document.createElement('div');
    kids.className = 'kids';
    kids.style.display = 'none';

    var wrap = document.createElement('div');
    wrap.className = 'node-wrap';
    wrap.appendChild(row);
    wrap.appendChild(kids);

    row.addEventListener('click', function (e) {
      e.stopPropagation();
      select(node);
      if (node.hasKid) toggle(node);
    });

    // 双击展开/收起子级但不改选中
    row.addEventListener('dblclick', function (e) {
      e.stopPropagation();
      if (node.hasKid) toggle(node);
    });

    return { wrap: wrap, row: row, kids: kids, twisty: twisty };
  }

  function renderLevel(parentId, container, nodes) {
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      if (state[n.id]) continue;
      var ui = makeRow(n);
      state[n.id] = {
        node: n, el: ui.row, kids: ui.kids, twisty: ui.twisty,
        loaded: false, expanded: false, parent: parentId
      };
      container.appendChild(ui.wrap);
      order.push(n.id);
    }
  }

  function toggle(node) {
    var st = state[node.id];
    if (!st) return;

    if (st.expanded) {
      st.expanded = false;
      st.kids.style.display = 'none';
      st.twisty.classList.remove('open');
      return;
    }
    expandNode(st, node);
  }

  // expandNode 展开一个节点（必要时懒加载它的子层），返回 Promise。
  //
  // 单独抽出来是为了深链定位：要把目录树展开到某篇文档，必须逐层等待
  // 子层加载完才能继续往下展开，所以这里必须可 await。
  function expandNode(st, node) {
    st.expanded = true;
    st.kids.style.display = 'block';
    st.twisty.classList.add('open');
    if (st.loaded) return Promise.resolve();

    // 懒加载：只请求这一层
    st.kids.innerHTML = '<div class="tree-loading">加载中…</div>';
    return api('/api/nav', { lib: LIB_ID, parent: node.id }).then(function (d) {
      st.kids.innerHTML = '';
      var items = d.items || [];
      if (!items.length) {
        st.kids.innerHTML = '<div class="tree-empty">（无子目录）</div>';
      } else {
        renderLevel(node.id, st.kids, items);
      }
      st.loaded = true;
      rebuildOrder();
      applyTreeFilter(); // 若筛选框里有词，新加载的层也要过滤
    }).catch(function (e) {
      st.kids.innerHTML = '<div class="tree-empty">加载失败：' + esc(e.message) + '</div>';
    });
  }

  // 重建可见顺序（上/下篇导航只走可见节点）
  function rebuildOrder() {
    order = [];
    (function walk(container) {
      var wraps = container.children;
      for (var i = 0; i < wraps.length; i++) {
        var w = wraps[i];
        var row = w.querySelector(':scope > .node');
        if (!row) continue;
        order.push(row.dataset.id);
        var kids = w.querySelector(':scope > .kids');
        if (kids && kids.style.display !== 'none') walk(kids);
      }
    })(treeEl);
  }

  function select(node) {
    current = node.id;
    document.querySelectorAll('.node.active').forEach(function (el) { el.classList.remove('active'); });
    var st = state[node.id];
    if (st) st.el.classList.add('active');

    if (!node.url) return;
    frame.src = resURL(node);
    updateCrumb(node);
  }

  function updateCrumb(node) {
    var chain = [];
    var id = node.id;
    var guard = 0;
    while (id && guard++ < 64) {
      var st = state[id];
      if (!st) break;
      chain.unshift(st.node);
      id = st.node.parent;
    }
    var html = '<a href="/">文档库</a>';
    for (var i = 0; i < chain.length; i++) {
      var n = chain[i];
      var t = esc(n.title || n.id);
      if (i === chain.length - 1) {
        html += '<span class="sep">/</span><span class="cur">' + t + '</span>';
      } else {
        html += '<span class="sep">/</span><a href="javascript:void(0)" data-goto="' + esc(n.id) + '">' + t + '</a>';
      }
    }
    crumb.innerHTML = html;
    crumb.querySelectorAll('[data-goto]').forEach(function (a) {
      a.addEventListener('click', function () {
        var st = state[a.dataset.goto];
        if (st) { select(st.node); st.el.scrollIntoView({ block: 'center' }); }
      });
    });
  }

  function move(delta) {
    rebuildOrder();
    if (!order.length) return;
    var i = current ? order.indexOf(current) : -1;
    var j = i < 0 ? 0 : Math.max(0, Math.min(order.length - 1, i + delta));
    var st = state[order[j]];
    if (!st) return;
    select(st.node);
    st.el.scrollIntoView({ block: 'center' });
  }

  // ---------- 目录筛选 ----------

  // 按标题子串过滤树。保留命中节点的完整父链，隐藏其余。
  function applyTreeFilter() {
    var q = (document.getElementById('tree-filter').value || '').trim().toLowerCase();
    var ids = Object.keys(state);

    if (!q) {
      for (var i = 0; i < ids.length; i++) {
        state[ids[i]].el.classList.remove('filtered-out');
      }
      return;
    }

    // 命中集合 + 其祖先集合
    var keep = {};
    for (var j = 0; j < ids.length; j++) {
      var st = state[ids[j]];
      var t = ((st.node.title || '') + ' ' + (st.node.url || '')).toLowerCase();
      if (t.indexOf(q) < 0) continue;
      keep[ids[j]] = true;
      var p = st.node.parent;
      var guard = 0;
      while (p && guard++ < 64) {
        if (keep[p]) break;
        keep[p] = true;
        var ps = state[p];
        if (!ps) break;
        p = ps.node.parent;
      }
    }
    for (var k = 0; k < ids.length; k++) {
      state[ids[k]].el.classList.toggle('filtered-out', !keep[ids[k]]);
    }
  }

  document.getElementById('tree-filter').addEventListener('input', applyTreeFilter);

  // ---------- 深链定位 ----------
  //
  // 从首页搜索结果点进来时（/doc/{lib}/?url=…&node=…），目录树得展开到那一篇
  // 并选中，否则用户落在「树没展开、不知道自己在哪」的页面上。
  //
  // 目录树是逐层懒加载的，前端手里只有目标节点的 ID/URL，没有祖先链，所以先
  // 向服务端要「根 -> 目标」的节点链（/api/nav?lib=..&node=.. 或 &url=..），
  // 再按链逐层展开。
  //
  // 返回值表示是否真的打开了某篇正文。为 false 时调用方退化成打开第一篇，
  // 免得深链参数失效导致正文区空白。
  function revealDeep(nodeId, url) {
    var params = { lib: LIB_ID };
    if (nodeId) { params.node = nodeId; } else { params.url = url; }
    return api('/api/nav', params).then(function (d) {
      var path = d.path || [];
      return expandChain(path, 0).then(function () {
        var last = path.length ? path[path.length - 1] : null;
        var st = last ? state[last.id] : null;
        if (st && st.node.url) {
          select(st.node); // 选中并在 iframe 打开正文
          st.el.scrollIntoView({ block: 'center' });
          return true;
        }
        // 该 URL 不在目录树里：正文命中常见这种情况，有些页面没有被
        // navi.xml / .hhc 收录。正文照样打开，只是树里没有可高亮的位置。
        if (url) { frame.src = resURL({ url: url }); return true; }
        return false;
      });
    }).catch(function () {
      // 深链解析失败也不能让正文打不开，退化成直接按 url 打开。
      if (url) { frame.src = resURL({ url: url }); return true; }
      return false;
    });
  }

  // expandChain 沿节点链逐层展开。
  // 必须串行：上一层没加载完，下一层的节点还不存在（懒加载）。
  function expandChain(path, i) {
    if (i >= path.length - 1) return Promise.resolve();
    var st = state[path[i].id];
    if (!st) return Promise.resolve(); // 链断了就停手，不抛错
    return expandNode(st, path[i]).then(function () { return expandChain(path, i + 1); });
  }

  // openFirst 打开目录里的第一篇（没有深链参数、或深链失效时的默认行为）。
  function openFirst(items) {
    var first = items[0];
    select(first);
    if (first.hasKid) toggle(first);
  }

  // ---------- 本文档内搜索 ----------
  //
  // 与首页检索同一套接口、同一套渲染，区别只有一个：带上 lib=<本库>。
  // 「首页搜全库 / 文档页搜本文档」因此不是两种搜法，而是同一搜法换个范围，
  // 用户不需要做任何选择。
  //
  // 结果覆盖在正文之上（不跳走），点某条直接在下面的 iframe 里打开。
  // 目录树与当前位置都保留，关掉面板即可继续读。

  var searchPane = document.getElementById('searchPane');
  var searchStat = document.getElementById('searchStat');
  var searchResults = document.getElementById('searchResults');
  var searchPager = document.getElementById('searchPager');
  var qEl = document.getElementById('q');

  var squery = '';        // 搜索词
  var spage = 1;          // 页码
  var sinflight = null;   // AbortController：取消过期请求
  var sseq = 0;           // 请求序号：防止旧响应覆盖新结果

  function searchURL(p) {
    return '/api/search?q=' + encodeURIComponent(squery) +
      '&lib=' + encodeURIComponent(LIB_ID) + '&page=' + p;
  }

  function openSearchPane(on) {
    searchPane.hidden = !on;
    if (!on) { searchPager.hidden = true; }
  }

  function cancelSearch() {
    if (sinflight) {
      try { sinflight.abort(); } catch (e) { /* 忽略 */ }
      sinflight = null;
    }
    sseq++;
  }

  function runSearch(newPage) {
    if (!squery) { cancelSearch(); openSearchPane(false); return; }
    cancelSearch();
    openSearchPane(true);
    spage = newPage || 1;

    var mySeq = sseq;
    var t0 = Date.now();
    sinflight = (typeof AbortController !== 'undefined') ? new AbortController() : null;
    var opt = sinflight ? { signal: sinflight.signal } : undefined;

    searchStat.textContent = '检索中…';
    searchResults.innerHTML = '<div class="blank">检索中…</div>';
    searchPager.hidden = true;

    fetch(searchURL(spage), opt)
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (mySeq !== sseq) return;   // 已被更新的查询取代
        if (d.error) {
          searchStat.textContent = '';
          searchResults.innerHTML = '<div class="blank">' + esc(d.error) + '</div>';
          return;
        }
        renderStat(d);
        var items = d.items || [];
        if (!items.length) {
          searchResults.innerHTML = '<div class="blank">本文档中没有找到「' +
            esc(squery) + '」。可以换个更短的关键词试试。</div>';
          return;
        }
        renderHits(items);
        renderPager(d);
      })
      .catch(function (e) {
        if (mySeq !== sseq) return;
        if (e && e.name === 'AbortError') return;
        searchStat.textContent = '';
        searchResults.innerHTML = '<div class="blank">检索失败：' +
          esc(e && e.message ? e.message : e) + '</div>';
      });
  }

  function renderStat(d) {
    var c = d.content || {};
    var bits = [];
    var total = d.total || 0;
    // 措辞与首页搜索保持一致（总数在前、用同一个词「结果」），
    // 两页的差异只在范围，不在措辞。语料库覆盖全部包时总数是精确值，
    // 任何页都可以直接说「找到 N 条」。
    var exact = !!(d.corpus && d.corpus.enabled && !c.incomplete);
    if (spage === 1 || exact) {
      bits.push('找到 ' + total + ' 条结果');
      if (spage > 1) { bits.push('第 ' + spage + ' 页'); }
    } else {
      bits.push('到本页已确认 ' + total + ' 条');
      bits.push('第 ' + spage + ' 页');
    }
    bits.push('标题 ' + (d.titleN || 0) + ' 篇');
    if (c.enabled) { bits.push('正文 ' + (c.matched || 0) + ' 篇'); }
    bits.push('耗时 ' + (d.elapsedMs || 0) + ' ms');
    searchStat.textContent = bits.join(' · ');
  }

  // 结果条目与首页同构，但点击行为不同：在下方 iframe 里打开，不跳页。
  function renderHits(items) {
    var frag = document.createDocumentFragment();
    for (var i = 0; i < items.length; i++) {
      var h = items[i];
      var isContent = h.source === 'content';

      var a = document.createElement('a');
      a.className = 'sr' + (isContent ? ' sr-content' : ' sr-title-hit');
      a.href = resURL({ url: h.url });

      var t = document.createElement('div');
      t.className = 'sr-title';
      var bodyHit = (h.count || 0) > 0;
      var tag = document.createElement('span');
      tag.className = 'sr-tag ' + (isContent ? 'sr-tag-content' : 'sr-tag-title');
      tag.textContent = isContent ? '正文' : '标题';
      t.appendChild(tag);
      t.appendChild(document.createTextNode(h.title || h.url));
      a.appendChild(t);

      // 摘要紧跟标题（与首页、与官方阅读器次序一致）：不用点进去就能看到内容。
      if (h.snippet) {
        var sn = document.createElement('div');
        sn.className = 'sr-snippet' + (bodyHit ? '' : ' sr-snippet-lead');
        sn.innerHTML = h.snippet; // 服务端已转义并加 <em>
        a.appendChild(sn);
      }

      var m = document.createElement('div');
      m.className = 'sr-meta';
      var bits = [];
      if (h.breadth) { bits.push(h.breadth); }
      if (bodyHit && h.count > 1) { bits.push('命中 ' + h.count + ' 次'); }
      if (bits.length) { m.textContent = bits.join('  ›  '); a.appendChild(m); }

      // 点击：先关掉结果面板，再把正文打开并把目录树定位过去。
      //
      // 关面板这一步不能省。结果面板是覆盖在正文之上的（.docpane 用 inset:0），
      // 只换 iframe.src 的话正文确实换了，但用户眼前仍然是结果列表，
      // 观感就是「点了没反应、跳不过去」。
      a.addEventListener('click', (function (hit) {
        return function (ev) {
          ev.preventDefault();
          openSearchPane(false);
          // 与从首页进来时同一套深链逻辑：优先按 nodeId 精确定位，
          // 否则按 url 反查目录节点；都不在目录树里时退化为直接打开正文。
          revealDeep(hit.nodeId || '', hit.url);
        };
      })(h));

      frag.appendChild(a);
    }
    searchResults.innerHTML = '';
    searchResults.appendChild(frag);
  }

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
        b.addEventListener('click', function () { runSearch(target); });
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

  document.getElementById('btn-search').addEventListener('click', function () {
    squery = qEl.value.trim();
    runSearch(1);
  });
  document.getElementById('btn-close-search').addEventListener('click', function () {
    cancelSearch();
    openSearchPane(false);
  });

  var stimer = null;
  qEl.addEventListener('input', function () {
    squery = qEl.value.trim();
    clearTimeout(stimer);
    if (!squery) { cancelSearch(); openSearchPane(false); return; }
    // 正文段是扫全库的重操作，统一等用户停手再发请求。
    stimer = setTimeout(function () { runSearch(1); }, 500);
  });
  qEl.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter') return;
    squery = qEl.value.trim();
    runSearch(1);
  });
  qEl.addEventListener('keyup', function (e) {
    if (e.key === 'Escape') {
      qEl.value = ''; squery = '';
      cancelSearch();
      openSearchPane(false);
    }
  });

  // ---------- 顶栏按钮 ----------

  document.getElementById('btn-prev').addEventListener('click', function () { move(-1); });
  document.getElementById('btn-next').addEventListener('click', function () { move(1); });
  document.getElementById('btn-top').addEventListener('click', function () {
    // 正文跑在沙箱 iframe 里，父窗口碰不到它的 DOM（见 viewer.html 的注释），
    // 所以不能用 contentWindow.scrollTo。改用 fragment 导航：#top 是浏览器的
    // 保留锚点，文档里没有同名元素时滚到最顶部，而且只改 fragment 属于
    // 同文档跳转，不会重新加载正文。
    try {
      frame.src = frame.src.split('#')[0] + '#top';
    } catch (e) { /* 忽略 */ }
  });
  document.getElementById('collapse-all').addEventListener('click', function () {
    Object.keys(state).forEach(function (id) {
      var st = state[id];
      st.expanded = false;
      st.kids.style.display = 'none';
      st.twisty.classList.remove('open');
    });
    rebuildOrder();
  });
  document.getElementById('toggle-side').addEventListener('click', function () {
    sidebar.classList.toggle('hidden');
  });

  // 键盘导航
  document.addEventListener('keydown', function (e) {
    var tag = (e.target.tagName || '').toUpperCase();
    if (tag === 'INPUT' || tag === 'TEXTAREA') return;
    if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); move(1); }
    if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); move(-1); }
  });

  // ---------- 分栏拖动 ----------

  (function () {
    var splitter = document.getElementById('splitter');
    var dragging = false;
    splitter.addEventListener('mousedown', function (e) {
      dragging = true;
      splitter.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
      e.preventDefault();
    });
    document.addEventListener('mousemove', function (e) {
      if (!dragging) return;
      var w = Math.max(180, Math.min(720, e.clientX));
      document.documentElement.style.setProperty('--side-w', w + 'px');
    });
    document.addEventListener('mouseup', function () {
      if (!dragging) return;
      dragging = false;
      splitter.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
    });
  })();

  // ---------- 内存指标 ----------

  WebICSMem.attach(document.getElementById('mem'));

  // ---------- 初始加载：顶层目录 ----------

  api('/api/nav', { lib: LIB_ID, parent: '' }).then(function (d) {
    treeEl.innerHTML = '';
    var items = d.items || [];
    if (!items.length) {
      treeEl.innerHTML = '<div class="tree-empty">该文档库没有目录</div>';
      return;
    }
    renderLevel('', treeEl, items);
    rebuildOrder();
    // 带深链参数（从搜索结果点进来）就定位到那一篇，否则打开第一篇。
    // 深链失效时也退化成第一篇，不留白屏。
    if (!DEEP.node && !DEEP.url) {
      openFirst(items);
      return;
    }
    revealDeep(DEEP.node, DEEP.url).then(function (opened) {
      if (!opened) openFirst(items);
    });
  }).catch(function (e) {
    treeEl.innerHTML = '<div class="tree-empty">加载目录失败：' + esc(e.message) + '</div>';
  });
})();
