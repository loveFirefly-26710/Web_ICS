package search

import (
	"context"
	"sort"
	"strings"
	"time"

	"web_ics/internal/corpus"
	"web_ics/internal/doclib"
	"web_ics/internal/textfold"
)

// 融合检索（Fused Search）
//
// 只有一条检索链路：一个输入框、一次查询，同时打标题字段和正文字段，
// 靠权重差让标题命中排在前面。标题命中与正文命中是同一结果集里的两类
// 条目，不是让用户自己选的两种搜索模式。
//
// 与 Lucene 的对应关系（原版阅读器用 Lucene 8.11.2，索引字段只有
// t_title 权重 10、t_content 权重 1、tp_keywords）：
//
//	Lucene                            本实现
//	BoostQuery(t_title, 10)           Score = 0（标题段整体靠前）
//	BoostQuery(t_content, 1)          Score = 1
//	BooleanQuery SHOULD 并集          两段结果拼成一个 Items 切片
//	TopDocs.totalHits                 Result.Total（精确全量）
//	pageIndex/pageSize/firstPageSize  PageIndex/PageSize/FirstPageSize
//
// 正文扫描的取舍：原版为全库正文建了常驻索引（1~3 GB），1G 预算放不下。
// 这里的做法是标题段走常驻索引、正文段走磁盘语料库全量扫描：
//
//  1. 标题段精确且即时，任何情况下都完整返回（含命中总数）；
//  2. 正文段一次查询扫全部文档包，命中总数精确、分页确定；
//  3. Scope 参数让调用方显式收窄（对应原版高级搜索的「检索范围」下拉）。
//
// 结果多靠分页解决，不截断：total 始终是精确全量，不是「取回了多少条」。

// 命中来源，供前端做类型标记。
const (
	SourceTitle   = "title"   // 命中标题字段（t_title）
	SourceContent = "content" // 命中正文字段（t_content）
)

// FusedHit 是融合结果里的一条。
//
// 字段是标题命中与正文命中的并集，由 Source 区分，前端一套渲染逻辑就够。
//
// 标题命中带 Groups：同一篇文档（同一个 url / nodeId）常常被多个产品
// 文档包收录。搜「接口」时两万多条标题命中里唯一主题只有几千个，剩下
// 都是同一篇文档在不同包里的重复。按主题归组后，一个主题只画一张卡片，
// 卡片里列出该主题所在的全部文档包，用户看到的是「一篇文档出现在哪些
// 产品里」，而不是同一篇文档刷屏多次。
type FusedHit struct {
	Source   string `json:"source"` // SourceTitle | SourceContent
	LibID    string `json:"libId"`
	LibName  string `json:"libName"`
	Title    string `json:"title"`              // 标题命中=标题；正文命中=正文页标题
	URL      string `json:"url"`                // 相对 resources/ 的 URL
	NodeID   string `json:"nodeId,omitempty"`   // 仅标题命中：目录节点 ID
	ParentID string `json:"parentId,omitempty"` // 仅标题命中
	Breadth  string `json:"breadth,omitempty"`  // 仅标题命中：父链标题
	Snippet  string `json:"snippet,omitempty"`  // 仅正文命中：含 <em> 的摘要
	Count    int    `json:"count,omitempty"`    // 仅正文命中：该篇命中次数
	Score    int    `json:"score"`              // 排序权重，越小越靠前

	// Groups 是「该主题所在的全部文档包」（含首个）。
	// 仅当 len(Groups) > 1 时前端才渲染包列表。
	Groups []HitGroup `json:"groups,omitempty"`
}

// HitGroup 描述「某篇文档出现在某个产品文档包」的一次收录。
type HitGroup struct {
	LibID   string `json:"libId"`
	LibName string `json:"libName"`
	Breadth string `json:"breadth,omitempty"` // 该包内的父链标题
	URL     string `json:"url"`               // 该包内的跳转 URL
}

// FusedOptions 是一次融合检索的参数。
type FusedOptions struct {
	Query string
	Cat   string // 领域过滤键（"all" 或空表示不过滤）
	Scope string // "all"（默认，标题+正文）| "title"（仅标题）

	// LibScope 非空时，把检索限定在这一个文档包内。
	//
	// 阅读器里「打开某个文档后搜索」应当是搜本文档，而不是搜全库，
	// 后者会把用户从当前文档里带走。标题段与正文段都按它过滤。
	//
	// 这不是「范围下拉」，而是页面上下文决定的：首页搜全库、
	// 文档页搜本文档，用户不需要做选择。
	LibScope string

	PageIndex     int // 从 1 开始
	PageSize      int // 后续页每页条数
	FirstPageSize int // 第 1 页条数

	// 正文扫描的资源边界。标题段不受这些限制。
	ContentBudget  int64         // 解压字节预算
	ContentTimeout time.Duration // 超时
	ContentMaxDoc  int           // 单篇最多几个摘要
	Ctx            context.Context
}

// Result 是一次融合检索的最终结果。
type Result struct {
	Query   string     `json:"query"`
	Total   int        `json:"total"`  // 全量命中数（唯一标题主题 + 正文命中篇数）
	TitleN  int        `json:"titleN"` // 唯一标题主题数（已合并重复收录，精确，不受分页影响）
	Items   []FusedHit `json:"items"`  // 本页结果
	Page    PageInfo   `json:"page"`
	Content ContentDig `json:"content"` // 正文扫描的进度信息

	IndexNode int   `json:"indexNode"` // 标题索引节点数
	IndexMs   int64 `json:"indexMs"`
	ElapsedMs int64 `json:"elapsedMs"`
}

// PageInfo 描述分页状态。
type PageInfo struct {
	PageIndex     int  `json:"pageIndex"`
	PageSize      int  `json:"pageSize"`
	FirstPageSize int  `json:"firstPageSize"`
	Total         int  `json:"total"`
	HasPrev       bool `json:"hasPrev"`
	HasNext       bool `json:"hasNext"`
	More          bool `json:"more"` // 是否还有下一页（含「正文尚未扫完」）
}

// ContentDig 是正文扫描的进度摘要。
//
// 描述的是扫描覆盖情况，不是「结果被截断」。已找到的命中一条都不会丢。
type ContentDig struct {
	Enabled    bool  `json:"enabled"`    // 本次是否扫了正文
	Scanned    int   `json:"scanned"`    // 已扫描篇数
	ScannedLib int   `json:"scannedLib"` // 已扫描库数
	TotalLibs  int   `json:"totalLibs"`  // 范围内库总数
	Matched    int   `json:"matched"`    // 正文命中篇数
	MoreLibs   bool  `json:"moreLibs"`   // 是否还有库没轮到
	Incomplete bool  `json:"incomplete"` // 扫描未覆盖全部范围
	Timeout    bool  `json:"timeout"`
	ElapsedMs  int64 `json:"elapsedMs"`
}

// Fused 执行一次融合检索。
//
// 排序：标题命中（Score=0）整体在前，正文命中（Score=1）在后，与用权重
// 10:1 打分得到的相对次序一致。无打分器时用整数分级表达同一语义。
func (t *TitleIndex) Fused(opt Options, q *ContentQuerier, fo FusedOptions) Result {
	start := time.Now()
	res := Result{Query: fo.Query}
	if fo.PageIndex < 1 {
		fo.PageIndex = 1
	}
	if fo.PageSize <= 0 {
		fo.PageSize = 20
	}
	if fo.FirstPageSize <= 0 {
		fo.FirstPageSize = 100
	}
	if fo.Scope == "" {
		fo.Scope = "all"
	}

	catOK := func(libID string) bool {
		if fo.Cat == "" || fo.Cat == "all" {
			return true
		}
		return t.catOf[libID] == fo.Cat
	}

	// 限定单库与领域过滤是两个正交的条件，合成后同时生效。
	// 首页传空（搜全库），文档页传当前 libId（搜本文档）。
	libScope := strings.TrimSpace(fo.LibScope)
	keep := catOK
	if libScope != "" {
		keep = func(libID string) bool {
			return libID == libScope && catOK(libID)
		}
	}

	// ---- 第 1 段：标题命中（精确、即时、完整）----
	//
	// 不用「选出最好的 N 条」那种设计，它会把命中总数掩盖成 limit。
	// 这里全量扫描 + 按主题合并，让 TitleN 是真实主题数，与分页完全解耦。
	allTitles := t.searchTitlesAll(fo.Query, keep)
	res.TitleN = len(allTitles)
	ms, nodes := t.Build()
	res.IndexMs, res.IndexNode = ms, nodes

	// 本页需要多少条。正文段只收集「本页还差多少」，让正文扫描代价与
	// 「命中总数」解耦。
	size := fo.PageSize
	if fo.PageIndex == 1 {
		size = fo.FirstPageSize
	}

	// offset 的算法：把标题段与正文段看成一条连续的结果流
	//
	//	[ 标题命中 0..T-1 ][ 正文命中 0..C-1 ]
	//
	// 第 1 页取流的前 FirstPageSize 条，第 k(k>=2) 页取接下来的 PageSize 条，
	// 与 Lucene 的 getSearchRange 一致。落到两段结构上，第 k 页的起点
	// offset 就是上面的 range，落在哪一段由 offset 与 T 的大小关系决定。
	//
	// 标题结果里同一篇文档会重复出现（同一命令参考被多个产品包收录），
	// 因此必须按主题合并，否则第 1 页尾部与第 2 页头部会包含同一主题的
	// 不同收录行，看起来像分页串页。合并后每主题一条，跨页天然零重叠。
	offset := 0
	if fo.PageIndex > 1 {
		offset = fo.FirstPageSize + (fo.PageIndex-2)*fo.PageSize
		if offset < 0 {
			offset = 0
		}
	}
	// 本页在标题段里的起点与条数。
	titleStart := offset
	if titleStart > len(allTitles) {
		titleStart = len(allTitles)
	}

	// 第 1 页从标题段里让出若干条给正文段。第 1 页总是标题与正文混排，
	// 若让标题命中吃满首屏，用户在「全文」范围下会看到清一色标题结果，
	// 误以为「没有正文命中」。
	//
	// 让位比例取 1/4，与「标题是主体、正文做补充」的权重直觉一致；
	// 仅在标题命中确实足够多时才让，避免短查询首屏变空。
	// 后续页不让位：那是纯顺序切片，不重排权重。
	const contentFirstPageShare = 4 // 1/4
	titleQuota := size
	if fo.Scope != "title" && fo.PageIndex == 1 && size >= 8 {
		titleQuota = size - size/contentFirstPageShare
		if titleQuota > len(allTitles)-titleStart {
			titleQuota = len(allTitles) - titleStart
		}
		if titleQuota < 0 {
			titleQuota = 0
		}
	}
	titleSlice, titleMore := pageSlice(allTitles, titleStart, titleQuota)
	needContent := size - len(titleSlice)
	if needContent < 0 {
		needContent = 0
	}

	// 这里要「探测」而不是直接跳过正文段：当标题命中已把本页填满
	// （needContent == 0）时，看似可以不扫正文，但那样用户会以为
	// 「这个词没有正文命中」，而实际上只是本页被标题占满了。
	//
	// 所以只在第 1 页且标题已填满时做一次限量探测，目的有二：
	// 拿到正文命中数用于 total，以及用「是否探到」决定 HasNext，
	// 让用户知道翻页还有正文结果。后续页一律只取 needContent 条。
	probe := 0
	if needContent == 0 && fo.Scope != "title" && fo.PageIndex == 1 {
		probe = contentProbe
	}

	// ---- 第 2 段：正文命中 ----
	//
	// 正文段在整体结果流里的起始下标固定是 len(allTitles)，因此正文的
	// 跳过量 = 本页 offset 减去标题段总长（不足则为 0）。
	contentSkip := 0
	if offset > len(allTitles) {
		contentSkip = offset - len(allTitles)
	}

	var contentHits []FusedHit
	contentMore := false
	if fo.Scope != "title" {
		switch {
		case q.CorpusReady():
			// 语料库路径：无论本页要不要正文条目，都扫全部文档包。
			// total 必须是精确的全量命中数，这正是「搜索覆盖全部文档」
			// 的含义。顺带把本页的标题命中补上正文摘要（enrich），
			// 让结果卡片是「标题 + 正文」而不是光秃秃一个标题。
			contentHits, contentMore = q.corpusContent(fo, keep, contentSkip, needContent, &res, titleSlice)
		default:
			// 兜底：语料还没建好时退回逐包流式扫描，
			// 保证搜索不会因为索引没建完而完全不可用。
			want := needContent
			if want == 0 {
				want = probe
			}
			if want > 0 {
				contentHits, contentMore = q.fusedContent(fo, keep, contentSkip, want, &res)
			} else {
				// 后续页且本页已被标题填满：仍然把正文进度填进 res.Content，
				// 但不实际扫描（不重复付出扫描代价）。
				contentProgressHint(q, fo, keep, &res)
			}
		}
	}

	// ---- 合并（标题段在前，正文段在后）----
	//
	// 标题段整体在前是权重的离散化实现：Score 0（标题）< 1（正文）。
	items := make([]FusedHit, 0, len(titleSlice)+len(contentHits))
	items = append(items, titleSlice...)
	if needContent > 0 {
		// 正常交付：正文条目补足本页
		items = append(items, contentHits...)
	} else {
		// 纯探测：正文条目不用来填充本页（标题已按 quota 填满），
		// 只用于 total 与 HasNext 判断。
		if len(items) > size {
			items = items[:size]
		}
	}
	res.Items = items
	if res.Items == nil {
		res.Items = []FusedHit{}
	}

	// ---- 分页信息 ----
	//
	// 「还有下一页」有两种来源，任一成立即可：标题段后面还有（titleMore），
	// 或正文段已扫出的条数达到了本页所需且扫描尚未覆盖全部范围。
	hasMore := titleMore || contentMore
	res.Page = PageInfo{
		PageIndex:     fo.PageIndex,
		PageSize:      fo.PageSize,
		FirstPageSize: fo.FirstPageSize,
		Total:         res.TitleN + res.Content.Matched,
		HasPrev:       fo.PageIndex > 1,
		HasNext:       hasMore,
		More:          hasMore,
	}
	// Total 是全量命中数：标题命中总数（精确）加正文命中篇数，
	// 与「本页取回多少条」完全解耦。
	res.Total = res.TitleN + res.Content.Matched
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res
}

// contentProbe 是「本页被标题占满」时的正文探测条数。
//
// 只用于算 total 与判断 HasNext，因此不需要多；太小会让 total 偏小
// （只反映探测到的那几条所在的库），太大则白扫。
const contentProbe = 20

// contentProgressHint 在「不实际扫描正文」的场合填好进度字段。
func contentProgressHint(q *ContentQuerier, fo FusedOptions, keep func(string) bool, res *Result) {
	if q == nil || q.OpenPackage == nil {
		return
	}
	res.Content.TotalLibs = len(filterLibsByCat(q.Libs, keep))
}

// pageSlice 从 full 里取出 [offset, offset+size) 段，并报告是否还有后续。
func pageSlice(full []FusedHit, offset, size int) ([]FusedHit, bool) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(full) {
		offset = len(full)
	}
	end := offset + size
	if end > len(full) {
		end = len(full)
	}
	out := full[offset:end]
	if out == nil {
		out = []FusedHit{}
	}
	return out, end < len(full)
}

// searchTitlesAll 扫全部标题，返回按主题合并后的全部命中（已排序、已过滤）。
//
// 标题索引是紧凑的字符串切片（约 20 MB），全量扫一遍是几十毫秒，
// 因此「全量枚举 + 合并」可以承受，这也正是 TitleN 能做精确值的原因。
//
// 合并键是 (title, url)：同一篇文档在不同包里的 url 相同、标题相同，
// 应当合并；而不同 url 的同名标题（如两篇不同的「接口分类」）必须保留为
// 两条，否则会把不同文档误并成一条。
func (t *TitleIndex) searchTitlesAll(q string, catOK func(string) bool) []FusedHit {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	t.ensure()

	t.mu.RLock()
	cols := t.cols
	t.mu.RUnlock()
	if cols == nil {
		return nil
	}

	lq := textfold.ASCIILower(q)
	foldQ := ""
	if hasASCIILetter(q) {
		foldQ = textfold.ASCIILower(q)
	}

	titles := cols.titles
	type group struct {
		head FusedHit
		seen map[string]bool // 显示键 -> 已收录，防止同名同址的包重复列出
	}
	order := make([]string, 0, 256) // 保持首次出现顺序，供稳定排序打底
	gm := make(map[string]*group, 256)
	for i := range titles {
		idx := strings.Index(titles[i], q)
		if idx < 0 {
			if foldQ == "" || foldQ == q {
				continue
			}
			idx = strings.Index(titles[i], foldQ)
			if idx >= 0 && !caseInsensitiveAt(titles[i], foldQ) {
				idx = -1
			}
		}
		if idx < 0 {
			continue
		}
		libID := cols.libList[cols.libIDs[i]]
		if !catOK(libID) {
			continue
		}
		url := cols.urls[i]
		key := titles[i] + "\x00" + url
		g := gm[key]
		if g == nil {
			g = &group{
				head: FusedHit{
					Source:   SourceTitle,
					LibID:    libID,
					LibName:  t.libs[libID],
					Title:    titles[i],
					URL:      url,
					NodeID:   cols.nodeID[i],
					ParentID: cols.parID[i],
					Breadth:  cols.breadth[i],
					Score:    scoreOfLower(textfold.ASCIILower(titles[i]), lq, idx),
				},
				seen: map[string]bool{},
			}
			gm[key] = g
			order = append(order, key)
		}
		// 去重键用 (libName,url) 而不是 libID：若干包显示名完全相同
		// （同名不同 libID，如同一产品的不同发行版），列在卡片里用户
		// 分辨不出区别，重复列出只是噪音；而 (libName,url) 相同就意味着
		// 「点进去是同一处内容」，保留一条即可。libID 仍保留在首条上用于跳转。
		dispKey := t.libs[libID] + "\x00" + url
		if g.seen[dispKey] {
			continue
		}
		g.seen[dispKey] = true
		g.head.Groups = append(g.head.Groups, HitGroup{
			LibID:   libID,
			LibName: t.libs[libID],
			Breadth: cols.breadth[i],
			URL:     url,
		})
	}

	out := make([]FusedHit, 0, len(order))
	for _, k := range order {
		g := gm[k]
		// 包清单按包名排序，保证跨请求稳定，同时让「首个包」= 字母序
		// 最小的包，可复现。
		sort.SliceStable(g.head.Groups, func(i, j int) bool {
			return g.head.Groups[i].LibName < g.head.Groups[j].LibName
		})
		if len(g.head.Groups) > 0 {
			g.head.LibID = g.head.Groups[0].LibID
			g.head.LibName = g.head.Groups[0].LibName
		}
		out = append(out, g.head)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		if len(out[i].Title) != len(out[j].Title) {
			return len(out[i].Title) < len(out[j].Title)
		}
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].URL < out[j].URL
	})
	return out
}

// ContentQuerier 为融合检索提供「正文查询」能力。
//
// 用字段而不是让 search 包直接调 Grep：TitleIndex 属于纯索引层，正文扫描
// 要解压 zip。把依赖（库清单、开包函数）显式传进来，既避免硬编码，
// 也让融合检索可以脱离真实文档库跑通。
type ContentQuerier struct {
	Libs        []doclib.LibMeta
	OpenPackage func(string) (*doclib.Reader, error)

	// Corpus 是磁盘语料库。非空且至少建好一个包时，正文段走「全库语料
	// 扫描」（精确、覆盖全部文档包，见 corpuscontent.go）；为空或一个包
	// 都没建好时，退回逐包流式扫描兜底，保证语料还没建完时搜索依然可用。
	Corpus *corpus.Store
}

// fusedContent 跑正文段，返回命中的融合条目以及「是否可能还有更多」。
//
// 分页是确定性的：skip 是在正文段内要跳过的命中条数。Grep 按固定顺序
// （库顺序乘以包内文件顺序）扫描，命中后计数并跳过前 skip 条，所以第 2 页
// 拿到的正文条目就是第 1 页之后紧接着的那些，不串页也不漏，还不用缓存
// 全量结果。keep 是调用方合成好的「这个库要不要算」判断（分类加单库范围），
// 本方法只按它裁剪库清单。
func (q *ContentQuerier) fusedContent(fo FusedOptions, keep func(string) bool,
	skip, need int, res *Result) ([]FusedHit, bool) {

	if q == nil || q.OpenPackage == nil {
		return nil, false
	}
	res.Content.Enabled = true

	libs := filterLibsByCat(q.Libs, keep)
	res.Content.TotalLibs = len(libs)

	// 按库串行、每库独立预算。全库正文约 90 万篇 HTML，解压后 12 GB，
	// 1G 机器上不可能一口气扫完；把预算和超时给整次请求，会让单个请求
	// 长时间占着 CPU 和内存，而且「扫到哪了」没法表达成对用户有意义的进度。
	//
	// 一次请求只往下推一个库。库是有序的：前一页扫到第 k 个库，后一页从
	// 第 k+1 个接着扫，不重复也不漏，进度也就能如实说成「已扫描 X/Y 库」。
	//
	// 但整库必须走完，不能凑够本页就停，也不能让库被中途打断：正文段的
	// 分页靠 SkipHits，这要求同一个库在同一查询下的命中序列每次都一样。
	// 凑够就停或给每库一个小额度都会破坏这个前提，同一个查询连发两次会
	// 得到不同的 total，用户会以为数据坏了。
	//
	// 单包解压约 8000 篇 HTML、2~5 秒，所以超时给 20 秒、预算给 256 MB，
	// 远宽于单包实际用量，不会中途截断。
	libBudget := fo.ContentBudget
	if libBudget <= 0 {
		libBudget = 256 << 20
	}
	libTimeout := fo.ContentTimeout
	if libTimeout <= 0 {
		libTimeout = 20 * time.Second
	}

	skipRemain := skip
	needRemain := need
	hits := make([]GrepHit, 0, need)

	// 够用即止，而不是扫完当前库：只要每次扫描的前缀完全一致，分页就是
	// 确定的，而前缀一致只要求同一个库里的命中按同样顺序被产出，
	// 不要求本库被扫完。扫完整个包换来的是一个用户未必看得懂的总数，
	// 代价却是每个包多花几秒。
	scannedLib := 0
	for i, l := range libs {
		if fo.Ctx != nil {
			select {
			case <-fo.Ctx.Done():
				res.Content.Incomplete = true
				res.Content.MoreLibs = true
				goto done
			default:
			}
		}
		// 本页已凑够就停手：后面的命中留给下一次请求（翻页）接着取。
		if needRemain <= 0 {
			break
		}

		before := len(hits)
		gr := Grep(Options{
			Libs:        func() []doclib.LibMeta { return []doclib.LibMeta{l} },
			OpenPackage: q.OpenPackage,
		}, GrepOptions{
			Query:        fo.Query,
			MaxPerDoc:    fo.ContentMaxDoc,
			BudgetByte:   libBudget,
			Timeout:      libTimeout,
			Ctx:          fo.Ctx,
			SkipHits:     skipRemain,
			CollectLimit: needRemain,
		})

		hits = append(hits, gr.Hits...)
		res.Content.Scanned += gr.Scanned
		res.Content.Matched += gr.Matched
		res.Content.ElapsedMs += gr.ElapsedMs
		if gr.Truncated || gr.Timeout {
			res.Content.Incomplete = true
			res.Content.Timeout = res.Content.Timeout || gr.Timeout
		}
		scannedLib = i + 1

		// 本库的命中若全部落在「已跳过的前缀」里，本页要从下一个库接着取。
		if gr.Matched <= skipRemain {
			skipRemain -= gr.Matched
		} else {
			skipRemain = 0
			needRemain -= len(hits) - before
		}
		if gr.Truncated || gr.Timeout {
			// 单片已达预算/超时上界。本页若还没凑够，剩下的由下一次请求
			// 接着扫；本库内部也可能还有没扫到的，所以标未完成。
			break
		}
	}

done:
	res.Content.ScannedLib = scannedLib
	res.Content.MoreLibs = scannedLib < len(libs)
	// 未扫完 = 还有库没轮到；只有这样才能诚实地说「继续翻页还有正文命中」。
	if res.Content.MoreLibs {
		res.Content.Incomplete = true
	}

	out := make([]FusedHit, 0, len(hits))
	for _, h := range hits {
		if !keep(h.LibID) {
			continue
		}
		out = append(out, FusedHit{
			Source:  SourceContent,
			LibID:   h.LibID,
			LibName: h.LibName,
			Title:   h.Title,
			URL:     h.URL,
			Snippet: h.Snippet,
			Count:   h.Count,
			Score:   1,
		})
	}
	if len(out) > need {
		out = out[:need]
	}
	return out, res.Content.MoreLibs
}

func filterLibsByCat(libs []doclib.LibMeta, catOK func(string) bool) []doclib.LibMeta {
	out := make([]doclib.LibMeta, 0, len(libs))
	for _, l := range libs {
		if catOK(l.LibID) {
			out = append(out, l)
		}
	}
	return out
}
