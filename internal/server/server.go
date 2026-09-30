// Package server 提供 HTTP 服务：库列表、目录树（按层）、正文资源流式直出。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"web_ics/internal/corpus"
	"web_ics/internal/doclib"
	"web_ics/internal/memguard"
	"web_ics/internal/nav"
	"web_ics/internal/search"
	"web_ics/web"
)

// Config 是服务配置。
type Config struct {
	Addr           string
	MaxConcurrency int   // 最大在途请求数
	MaxBigFile     int   // 同时进行的大文件请求数
	BigFileBytes   int64 // 超过此大小视为大文件

	DebugHealth bool    // 是否暴露 /debug/healthz（详细内存与缓存指标）
	IPRate      float64 // 每 IP 每秒允许的请求数，<=0 关闭限流
	IPBurst     int     // 每 IP 的突发容量
	TrustProxy  bool    // 是否信任 X-Forwarded-For（配了反代才开）
}

// DefaultConfig 返回面向 1G 机器的默认配置。
func DefaultConfig() Config {
	return Config{
		Addr:           ":8080",
		MaxConcurrency: 10,
		MaxBigFile:     2,
		BigFileBytes:   4 << 20,
		// 每 IP 每秒 5 次检索、突发 20，只作用于 /api/search 与 /api/grep
		// 这两个重操作，正常浏览（含深链展开几十层目录）碰不到。
		IPRate:  5,
		IPBurst: 20,
	}
}

// Server 持有索引、缓存与内存守卫。
type Server struct {
	cfg    Config
	index  *doclib.Index
	trees  *nav.Cache
	guard  *memguard.Guard
	titles *search.TitleIndex
	corpus *corpus.Store

	// 并发闸门
	sem    chan struct{}
	bigSem chan struct{}

	// 每 IP 限流。全局闸门挡不住「单个 IP 占满全部槽位」。
	ipLim *ipLimiter
}

// New 构造服务器。index 必须已扫描完成。
func New(cfg Config, index *doclib.Index, trees *nav.Cache, guard *memguard.Guard) *Server {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 10
	}
	if cfg.MaxBigFile <= 0 {
		cfg.MaxBigFile = 2
	}
	s := &Server{
		cfg:    cfg,
		index:  index,
		trees:  trees,
		guard:  guard,
		sem:    make(chan struct{}, cfg.MaxConcurrency),
		bigSem: make(chan struct{}, cfg.MaxBigFile),
		ipLim:  newIPLimiter(cfg.IPRate, cfg.IPBurst),
	}
	// 标题索引懒构建：首次搜索时才扫描全部 navi.xml。内存守卫在软限时
	// 会调用 titles.Release()，让这块内存可以被回收。
	s.titles = search.NewTitleIndex(search.Options{
		Libs:        index.List,
		OpenPackage: doclib.OpenPackage,
		CategoryOf:  func(m doclib.LibMeta) string { return doclib.CategoryOf(m).Key },
	})
	return s
}

// SearchIndex 暴露标题索引，供 selftest 与内存守卫使用。
func (s *Server) SearchIndex() *search.TitleIndex { return s.titles }

// SetGuard 注入内存守卫。
//
// 拆成两步（先 New 再 SetGuard）是因为守卫的 onSoft 回调要引用 Server 的
// 搜索索引，而 Server 的构造又要拿到守卫，直接写会形成循环依赖。
func (s *Server) SetGuard(g *memguard.Guard) { s.guard = g }

// SetCorpus 注入正文语料库。未注入时正文段退回逐包流式扫描。
func (s *Server) SetCorpus(c *corpus.Store) { s.corpus = c }

// Handler 返回配置好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/debug/healthz", s.handleDebugHealth)
	mux.HandleFunc("/api/libs", s.handleLibs)
	mux.HandleFunc("/api/categories", s.handleCategories)
	mux.HandleFunc("/api/nav", s.handleNav)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/grep", s.handleGrep)
	mux.HandleFunc("/doc/", s.handleDoc)
	mux.HandleFunc("/doc", s.handleDoc) // /doc 与 /doc/{libId} 无斜杠形式
	mux.HandleFunc("/", s.handleStatic)
	// 顺序有讲究：安全头放最外层，这样限流的 429、闸门的 503、方法拒绝的
	// 405 也带上头；方法判断在限流之前，非 GET 请求不消耗限流配额。
	return withSecurityHeaders(withMethodGuard(s.withRateLimit(s.withGate(mux))))
}

// ---------------------------------------------------------------- middleware

// withGate 实现内存守卫与并发闸门。
func (s *Server) withGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 降级状态下拒绝新请求：快速失败好过雪崩
		if s.guard != nil && s.guard.Degraded() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "服务繁忙：内存接近上限，请稍后重试", http.StatusServiceUnavailable)
			return
		}
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "2")
			http.Error(w, "服务繁忙：请求过多", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- handlers

// handleHealth 是对外的健康检查，只回答「活着吗、在降级吗、内存到哪一档了」。
//
// 内存只给粗粒度值（50 MB 取整加档位），精确 RSS、缓存命中、在途请求、
// 语料错误这些挪到 handleDebugHealth。原因：匿名暴露实时内存与负载，等于
// 给攻击者一把尺子，让他能精确压到降级阈值。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	degraded := false
	memMB, memLevel := 0, "normal"
	if s.guard != nil {
		_, degraded, _, _ = s.guard.Stats()
		memMB, memLevel = s.guard.PublicStats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       !degraded,
		"degraded": degraded,
		"memMB":    memMB,
		"memLevel": memLevel,
	})
}

// handleDebugHealth 暴露完整运行指标，默认关闭。
//
// 打开方式：环境变量 WEB_ICS_DEBUG_HEALTH=1（或 selftest 里把 cfg.DebugHealth
// 置真）。生产环境应只在内网可达，或干脆不打开。
func (s *Server) handleDebugHealth(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DebugHealth {
		http.NotFound(w, r)
		return
	}
	rss, degraded, softHits, hardHits := uint64(0), false, int64(0), int64(0)
	if s.guard != nil {
		rss, degraded, softHits, hardHits = s.guard.Stats()
	}
	libID, nodes, hits, misses := "", 0, int64(0), int64(0)
	if s.trees != nil {
		libID, nodes, hits, misses = s.trees.Stats()
	}
	corpusReady, corpusTotal, corpusBuilding, corpusErr := 0, 0, false, ""
	if s.corpus != nil {
		corpusReady = s.corpus.ReadyCount()
		corpusTotal = s.corpus.Total()
		corpusBuilding = s.corpus.Building()
		corpusErr = s.corpus.LastErr()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              !degraded,
		"rssBytes":        rss,
		"rssMB":           rss >> 20,
		"degraded":        degraded,
		"softLimitHits":   softHits,
		"hardLimitHits":   hardHits,
		"cachedLib":       libID,
		"cachedNodes":     nodes,
		"treeCacheHits":   hits,
		"treeCacheMisses": misses,
		"inflight":        len(s.sem),
		"maxConcurrency":  s.cfg.MaxConcurrency,
		"libCount":        len(s.index.List()),
		"corpusReady":     corpusReady,
		"corpusTotal":     corpusTotal,
		"corpusBuilding":  corpusBuilding,
		"corpusErr":       corpusErr,
	})
}

func (s *Server) handleLibs(w http.ResponseWriter, r *http.Request) {
	libs := s.index.List()
	// 精简：列表页不需要 profile 的全部字段
	type item struct {
		LibID          string `json:"libId"`
		LibName        string `json:"libName"`
		ProductType    string `json:"productType"`
		ProductVersion string `json:"productVersion"`
		LibVersion     string `json:"libVersion"`
		IssueDate      string `json:"issueDate"`
		TopicNumber    int    `json:"topicNumber"`
		SizeMB         int64  `json:"sizeMB"`
		CategoryKey    string `json:"categoryKey"`
		CategoryName   string `json:"categoryName"`
	}
	out := make([]item, 0, len(libs))
	for _, l := range libs {
		cat := doclib.CategoryOf(l)
		out = append(out, item{
			LibID:          l.LibID,
			LibName:        l.LibName,
			ProductType:    l.ProductType,
			ProductVersion: l.ProductVersion,
			LibVersion:     l.LibVersion,
			IssueDate:      l.IssueDate,
			TopicNumber:    l.TopicNumber,
			SizeMB:         l.SizeBytes >> 20,
			CategoryKey:    cat.Key,
			CategoryName:   cat.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "items": out})
}

// handleCategories 返回产品领域分类及其库数量，供首页左侧导航渲染。
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	cats := s.index.Categories()
	writeJSON(w, http.StatusOK, map[string]any{"total": len(cats), "items": cats})
}

// handleNav 按层返回目录子节点。
//
// GET /api/nav?lib=<libId>&parent=<nodeId>
// GET /api/nav?lib=<libId>&node=<nodeId>   深链：返回「根 -> 目标」的节点链
// GET /api/nav?lib=<libId>&url=<相对URL>   深链：按 URL 反查节点链
//
// parent 为空时返回顶层节点。按层下发而不是整棵树，是为了避免把几万个
// 节点一次性推给前端。
//
// node= / url= 形式供「从搜索结果直接进入某篇文档」时定位目录树用：前端
// 只有节点 ID 或 URL，没有祖先链，而树是逐层懒加载的，必须先知道要展开
// 哪几层。标题命中带 nodeId，正文命中只有 URL，所以两种都要支持。
func (s *Server) handleNav(w http.ResponseWriter, r *http.Request) {
	libID := r.URL.Query().Get("lib")
	parent := r.URL.Query().Get("parent")
	if libID == "" {
		writeErr(w, http.StatusBadRequest, "缺少参数 lib")
		return
	}
	meta, ok := s.index.Get(libID)
	if !ok {
		writeErr(w, http.StatusNotFound, "库不存在")
		return
	}

	tree, err := s.trees.Get(libID, func() (*nav.Tree, error) {
		rd, err := doclib.OpenPackage(meta.FilePath)
		if err != nil {
			return nil, err
		}
		defer rd.Close()
		return nav.Parse(rd)
	})
	if err != nil {
		// 不回显 err：解析失败的信息里带包在磁盘上的绝对路径，
		// 匿名访客不该拿到服务端的目录结构。细节只进日志。
		log.Printf("[nav] 解析目录失败 lib=%s: %v", libID, err)
		writeErr(w, http.StatusInternalServerError, "解析目录失败")
		return
	}

	if node := strings.TrimSpace(r.URL.Query().Get("node")); node != "" ||
		strings.TrimSpace(r.URL.Query().Get("url")) != "" {
		path := tree.Path(node)
		if path == nil {
			path = tree.PathByURL(strings.TrimSpace(r.URL.Query().Get("url")))
		}
		if path == nil {
			path = []nav.Node{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"lib":  libID,
			"node": node,
			"path": path, // 空数组表示该节点/URL 不在目录树里（正文命中常见）
		})
		return
	}

	children := tree.ChildrenOf(parent)
	writeJSON(w, http.StatusOK, map[string]any{
		"lib":    libID,
		"parent": parent,
		"items":  children,
	})
}

// handleSearch 是统一的检索入口：一次查询同时命中标题与正文。
//
// GET /api/search?q=<词>&scope=all|title&page=1&cat=<领域键>&limit=<每页条数>
//
// 一个输入框、一次查询，同时打标题字段与正文字段，靠权重差让标题命中排在
// 前面。不做「标题搜索 / 正文检索」的模式切换，用户不需要猜该用哪个。
//
// total 是精确的全量命中数，结果多靠分页解决，不截断。正文扫描受内存与
// 时间保护（1G 机器放不下全库正文索引），表现为「正文覆盖 X/Y 个包」的
// 进度信息，不是结果被丢弃。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"query": "", "total": 0, "titleN": 0, "items": []any{},
		})
		return
	}
	if queryTooLong(q) {
		writeErr(w, http.StatusBadRequest, "检索词过长，最多 256 个字符")
		return
	}

	// 分页参数。默认首页 100 条、后续每页 20 条。
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	if page > maxPageIndex {
		page = maxPageIndex
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	if limit > 200 {
		limit = 200
	}
	firstLimit := atoiDefault(r.URL.Query().Get("firstLimit"), 100)
	if firstLimit > 500 {
		firstLimit = 500
	}

	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope != "title" && scope != "content" {
		scope = "all"
	}

	// lib= 限定在单个文档包内检索（文档页搜索用）。为空即搜全库（首页用）。
	// 范围由页面上下文决定，与「范围下拉」无关。
	libScope := strings.TrimSpace(r.URL.Query().Get("lib"))

	res := s.titles.Fused(search.Options{
		Libs:        s.index.List,
		OpenPackage: doclib.OpenPackage,
		CategoryOf:  func(m doclib.LibMeta) string { return doclib.CategoryOf(m).Key },
	}, &search.ContentQuerier{
		Libs:        s.index.List(),
		OpenPackage: doclib.OpenPackage,
		Corpus:      s.corpus, // 非空则正文段扫全库语料（覆盖全部文档包）
	}, search.FusedOptions{
		Query:         q,
		Cat:           strings.TrimSpace(r.URL.Query().Get("cat")),
		Scope:         scope,
		LibScope:      libScope,
		PageIndex:     page,
		PageSize:      limit,
		FirstPageSize: firstLimit,
		Ctx:           r.Context(),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"query":     res.Query,
		"libScope":  libScope,         // 空=搜全库；非空=限制在该文档包内
		"total":     res.Total,        // 全量命中数（精确）
		"titleN":    res.TitleN,       // 标题命中总数
		"items":     res.Items,        // 本页结果
		"page":      res.Page,         // 分页信息
		"content":   res.Content,      // 正文扫描进度
		"corpus":    s.corpusDigest(), // 语料库就绪情况（覆盖了哪些包）
		"indexNode": res.IndexNode,    // 标题索引节点数
		"indexMs":   res.IndexMs,
		"elapsedMs": res.ElapsedMs,
	})
}

// corpusDigest 汇总正文语料库状态，供 /api/search 与 /healthz 复用。
//
// ready < total 时说明还有包没建好语料，此时正文段只覆盖了 ready 个包，
// 必须如实告诉调用方，而不是让它以为搜遍了全部文档。
func (s *Server) corpusDigest() map[string]any {
	total := len(s.index.List())
	if s.corpus == nil {
		return map[string]any{"enabled": false, "ready": 0, "total": total, "building": false}
	}
	return map[string]any{
		"enabled":  true,
		"ready":    s.corpus.ReadyCount(),
		"total":    s.corpus.Total(),
		"building": s.corpus.Building(),
	}
}

// catKeys 返回 libID -> categoryKey 映射，用于搜索结果按领域过滤。
func (s *Server) catKeys() map[string]string {
	out := map[string]string{}
	for _, l := range s.index.List() {
		out[l.LibID] = doclib.CategoryOf(l).Key
	}
	return out
}

// handleGrep 在正文里做全文检索。
//
// GET /api/grep?q=<词>&lib=<libId>&limit=50&skip=<跳过条数>
//
// 这是重操作：要打开 zip、解压 HTML、剥标签，全库扫描要数秒。
//
// 界面走融合入口 /api/search，本接口保留为「单独扫正文」的低层通道，供
// 脚本与排障直接调用，语义与融合检索一致：
//
//   - limit 是本页最多取回多少条，不是结果上限；
//   - total 是已扫描范围内的命中篇数，不是取回了多少条；
//   - incomplete 表示扫描未覆盖全部范围，不是结果被丢弃。
func (s *Server) handleGrep(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"query": "", "total": 0, "items": []any{}})
		return
	}
	if len([]rune(q)) < 2 {
		writeErr(w, http.StatusBadRequest, "检索词至少 2 个字符")
		return
	}
	if queryTooLong(q) {
		writeErr(w, http.StatusBadRequest, "检索词过长，最多 256 个字符")
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	if limit > 200 {
		limit = 200
	}
	skip := atoiDefault(r.URL.Query().Get("skip"), 0)
	if skip < 0 {
		skip = 0
	}
	if skip > maxSkipHits {
		skip = maxSkipHits
	}

	res := search.Grep(search.Options{
		Libs:        s.index.List,
		OpenPackage: doclib.OpenPackage,
	}, search.GrepOptions{
		LibID:        r.URL.Query().Get("lib"),
		Query:        q,
		CollectLimit: limit + 20, // 内存收集上限，非结果上限
		SkipHits:     skip,
		Ctx:          r.Context(),
	})
	// 领域过滤：在命中结果里筛。正文扫描本身是全库的，
	// 但若指定了 lib 则已天然限定，无需再筛。
	if cat := strings.TrimSpace(r.URL.Query().Get("cat")); cat != "" && res.Hits != nil {
		keys := s.catKeys()
		kept := res.Hits[:0]
		for _, h := range res.Hits {
			if keys[h.LibID] == cat {
				kept = append(kept, h)
			}
		}
		res.Hits = kept
	}
	more := len(res.Hits) > limit
	if more {
		res.Hits = res.Hits[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":      q,
		"total":      res.Matched, // 已扫描范围内的命中篇数（精确）
		"matched":    res.Matched,
		"scanned":    res.Scanned,
		"scannedLib": res.ScannedLib,
		"totalLibs":  res.TotalLibs,
		"skip":       skip,
		"more":       more,                         // 本页之外还有命中
		"incomplete": res.Truncated || res.Timeout, // 扫描未覆盖全部范围
		"timeout":    res.Timeout,
		"elapsedMs":  res.ElapsedMs,
		"items":      res.Hits,
	})
}

// atoiDefault 解析整数，失败或非正数时返回默认值。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return def
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return def
		}
	}
	if n <= 0 {
		return def
	}
	return n
}

// handleDoc 处理正文资源请求：/doc/{libId}/res/{zip内路径}
//
// 资源一律流式直出，不整文件读入内存。
func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	// 同时兼容 /doc、/doc/、/doc/{libId}、/doc/{libId}/、/doc/{libId}/res/{path}
	if r.URL.Path == "/doc" || r.URL.Path == "/doc/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/doc/"), "/doc")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// 拆分：{libId}[/{res/...}]
	libID, sub := rest, ""
	if slash := strings.Index(rest, "/"); slash >= 0 {
		libID, sub = rest[:slash], rest[slash+1:]
	}

	meta, ok := s.index.Get(libID)
	if !ok {
		writeErr(w, http.StatusNotFound, "库不存在")
		return
	}

	// /doc/{libId}/ 或 /doc/{libId} 本身是阅读页外壳，不是资源
	if sub == "" {
		s.serveWebAsset(w, r, "viewer.html")
		return
	}

	// 只接受 res/ 前缀的资源请求
	if !strings.HasPrefix(sub, "res/") {
		writeErr(w, http.StatusNotFound, "未知资源路径")
		return
	}
	zipPath := "resources/" + strings.TrimPrefix(sub, "res/")

	rd, err := doclib.OpenPackage(meta.FilePath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打开文档包失败")
		return
	}
	defer rd.Close()

	f, ok := rd.Lookup(zipPath)
	if !ok {
		// 不回显内部 zip 路径：它会把包内目录结构告诉匿名访客。
		// 排障信息只进日志。
		log.Printf("[doc] 资源不存在 lib=%s path=%s", libID, sanitizeMsg(zipPath))
		writeErr(w, http.StatusNotFound, "资源不存在")
		return
	}

	// 大文件单独限流，防止多个 PDF 同时占用带宽与缓冲
	isBig := int64(f.UncompressedSize64) >= s.cfg.BigFileBytes
	if isBig {
		select {
		case s.bigSem <- struct{}{}:
			defer func() { <-s.bigSem }()
		default:
			w.Header().Set("Retry-After", "3")
			http.Error(w, "大文件并发已达上限，请稍后重试", http.StatusServiceUnavailable)
			return
		}
	}

	setContentType(w, zipPath)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", f.UncompressedSize64))

	// 流式写出：内存占用与文件大小无关
	if _, err := rd.Stream(zipPath, w); err != nil {
		// 响应已开始，只能记录
		log.Printf("[doc] 流式输出中断 lib=%s path=%s: %v", libID, sanitizeMsg(zipPath), err)
	}
}

// handleStatic 提供前端页面。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		// HTML 外壳绝不能带缓存：它引用的 style.css / home.js 是按名字引用的，
		// 一旦外壳被缓存，用户可能在很长一段时间里拿不到新版资源，
		// 表现为「改了 CSS 但页面还是旧样子」。
		noStore(w)
		s.serveWebAsset(w, r, "index.html")
		return
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// /doc/{libId}/ 与 /doc/{libId} 都返回阅读页外壳
	if strings.HasPrefix(r.URL.Path, "/doc/") {
		noStore(w)
		s.serveWebAsset(w, r, "viewer.html")
		return
	}
	// 前端静态资源：/static/{path}
	// 走条件请求（Last-Modified / ETag 到 304），而不是无脑 max-age。
	// 本地工具场景下「改了立刻看到」比省流量重要得多。
	// 这条只在读磁盘时成立，内嵌资源没有修改时间，见 serveEmbeddedAsset。
	if strings.HasPrefix(r.URL.Path, "/static/") {
		clean := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		s.serveWebAsset(w, r, clean)
		return
	}
	http.NotFound(w, r)
}

// noStore 让响应不被任何层级缓存，用于 HTML 外壳这类
// 「内容随版本变化、且引用其它资源」的入口文件。
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// ---------------------------------------------------------------- helpers

// serveWebAsset 输出一个前端资源文件，name 用正斜杠，例如 "static/style.css"。
//
// 选择顺序：
//  1. WEB_ICS_WEB_ROOT 指定的目录（设了就只用它，方便部署时外置覆盖）
//  2. 当前工作目录下的 web/（开发时的默认，改前端不用重编译）
//  3. 编译进二进制的那份（单文件分发，见 web 包）
//
// 前两步都要先 stat 到文件才用，所以目录不存在时会自然落到内嵌资源，
// 不会出现「配了路径但文件不在就 404」。
func (s *Server) serveWebAsset(w http.ResponseWriter, r *http.Request, name string) {
	if root := os.Getenv("WEB_ICS_WEB_ROOT"); root != "" {
		if p := filepath.Join(root, name); fileExists(p) {
			http.ServeFile(w, r, p)
			return
		}
	} else if p := filepath.Join("web", name); fileExists(p) {
		http.ServeFile(w, r, p)
		return
	}
	serveEmbeddedAsset(w, r, name)
}

// serveEmbeddedAsset 从内嵌资源里输出文件。
//
// 内嵌文件的 ModTime 是零值，所以这里给不出 Last-Modified，也就没有 304。
// 内嵌模式本来就要重新编译才能换文件，条件请求没有意义，不额外补 ETag。
func serveEmbeddedAsset(w http.ResponseWriter, r *http.Request, name string) {
	// embed.FS 只认正斜杠，路径不能以 / 开头，也不接受 Windows 的反斜杠
	clean := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(name)), "/")
	f, err := web.Assets.Open(clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	// embed 里的常规文件实现了 io.ReadSeeker，ServeContent 靠它做范围请求
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, clean, time.Time{}, rs)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// setContentType 设置响应的 Content-Type。
//
// 文档包的 HTML 内部声明 charset=gb2312，正文字节实际是 GBK，而 Go 的
// mime.TypeByExtension(".html") 返回 "text/html; charset=utf-8"。照搬的话
// HTTP 头的 charset 会覆盖 HTML 里的 <meta>，浏览器按 UTF-8 解码 GBK 字节，
// 全篇乱码。所以这里对文本类型一律不带 charset，让浏览器依据正文的
// <meta charset> 自行判断。
func setContentType(w http.ResponseWriter, name string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".htm", ".css", ".js", ".xml", ".txt":
		// 故意不带 charset：交给正文中的 <meta charset> / 编码声明决定
		switch ext {
		case ".html", ".htm":
			w.Header().Set("Content-Type", "text/html")
		case ".css":
			w.Header().Set("Content-Type", "text/css")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript")
		case ".xml":
			w.Header().Set("Content-Type", "application/xml")
		default:
			w.Header().Set("Content-Type", "text/plain")
		}
		return
	}

	ct := mime.TypeByExtension(ext)
	if ct == "" {
		switch ext {
		case ".svg":
			ct = "image/svg+xml"
		case ".pdf":
			ct = "application/pdf"
		case ".png":
			ct = "image/png"
		case ".jpg", ".jpeg":
			ct = "image/jpeg"
		case ".gif":
			ct = "image/gif"
		case ".bmp":
			ct = "image/bmp"
		case ".ico":
			ct = "image/x-icon"
		case ".woff2":
			ct = "font/woff2"
		case ".woff":
			ct = "font/woff"
		case ".ttf":
			ct = "font/ttf"
		default:
			ct = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ct)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 接口响应一律不进缓存：检索词会出现在 URL 与响应体里，
	// 被中间层缓存住等于把一次查询的结果留给下一个访客。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": sanitizeMsg(msg)})
}

// Shutdown 供主程序优雅退出使用。
func Shutdown(ctx context.Context, srv *http.Server) error {
	return srv.Shutdown(ctx)
}
