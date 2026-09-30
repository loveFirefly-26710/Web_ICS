package search

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"web_ics/internal/doclib"
	"web_ics/internal/htmltext"
	"web_ics/internal/textfold"
)

// GrepOptions 控制一次正文检索的资源消耗。
//
// 正文检索要打开 zip、解压 HTML、剥标签、逐篇匹配。全库 90 万篇 HTML
// 解压后约 12 GB，不加限制会把 1G 机器打爆。
//
// 资源保护和丢弃结果是两件事。这里的做法是：分页负责「结果多」，
// 预算与超时只负责保护内存与耗时。
//
//   - MaxPerDoc / BudgetByte / Timeout 保护单次调用的内存与耗时；
//   - SkipHits / CollectLimit 支撑确定性分页（见下）。
//
// 确定性分页：同一查询词、同一顺序扫描，第 k 次调用取到的前 N 条必然一致。
// 所以翻页等价于「以同一顺序扫描，跳过前 (page-1)*pageSize 条命中，再取
// pageSize 条」。不需要缓存全量结果，也就不需要额外内存。
type GrepOptions struct {
	LibID      string // 只搜这个库；空则搜全部
	Query      string
	MaxPerDoc  int   // 单篇最多返回几条，默认 3
	BudgetByte int64 // 总解压字节预算，默认 256 MB
	Timeout    time.Duration
	Ctx        context.Context

	// LimitLibs 非空时，只扫描这些库（用于把大扫描切成多个小请求）。
	LimitLibs []string
	// SkipHits 跳过前 N 条命中（确定性分页用）。
	SkipHits int
	// CollectLimit 最多收集多少条（在 SkipHits 之后计数）；<=0 表示不限。
	CollectLimit int
	// WantsExactCount 为 true 时，收集满后仍继续扫完，让 Matched 成为
	// 精确命中篇数。代价是把整包解压一遍（首包约 3 秒）。
	//
	// 只有需要「全库精确总数」的调用方该打开它。融合检索不需要：它只要
	// 够填满本页的命中，关闭时 Matched 的含义是「扫到停手为止已确认的
	// 命中篇数」。
	WantsExactCount bool
}

// GrepHit 是一条正文命中。
type GrepHit struct {
	LibID   string `json:"libId"`
	LibName string `json:"libName"`
	DocID   string `json:"docId"`   // 正文条目在包内的路径
	URL     string `json:"url"`     // 相对 resources/ 的 URL，用于拼正文地址
	Title   string `json:"title"`   // 正文标题（取自 <title> 或 h1）
	Breadth string `json:"breadth"` // 目录里的层级（若能在标题索引里找到）
	Snippet string `json:"snippet"` // 命中上下文，含 <em> 高亮
	Count   int    `json:"count"`   // 该篇命中的总次数
}

// GrepResult 是一次检索的汇总。
//
// Truncated 的含义是「本次扫描未覆盖全部范围（受时间或内存保护所限），
// 可以继续扫描」，不是「结果被丢弃」。已有的命中一条都不会丢。
type GrepResult struct {
	Hits       []GrepHit `json:"hits"`
	Scanned    int       `json:"scanned"`    // 已扫描的 HTML 篇数
	ScannedLib int       `json:"scannedLib"` // 已扫描的库数
	TotalLibs  int       `json:"totalLibs"`  // 范围内的库总数
	Matched    int       `json:"matched"`    // 命中的篇数
	Truncated  bool      `json:"truncated"`  // 是否因预算/超时提前结束扫描
	Timeout    bool      `json:"timeout"`    // 是否超时退出
	ElapsedMs  int64     `json:"elapsedMs"`
}

// Grep 流式扫描包内 HTML，返回命中的正文片段。
//
// 逐篇处理，不保留任何正文。每篇读完立刻剥标签、匹配、抽摘要，然后丢弃
// 原始字节。因此峰值内存与库大小无关，只与单篇大小有关。
func Grep(opt Options, go_ GrepOptions) GrepResult {
	start := time.Now()
	res := GrepResult{}

	q := strings.TrimSpace(go_.Query)
	if q == "" {
		res.ElapsedMs = time.Since(start).Milliseconds()
		return res
	}
	if go_.MaxPerDoc <= 0 {
		go_.MaxPerDoc = 3
	}
	if go_.BudgetByte <= 0 {
		go_.BudgetByte = 256 << 20
	}
	if go_.Timeout <= 0 {
		go_.Timeout = 20 * time.Second
	}
	ctx := go_.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, go_.Timeout)
	defer cancel()

	lq := strings.ToLower(q)
	libNameOf := map[string]string{}

	// LimitLibs 允许把「扫描全部库」拆成多次小请求。
	// 为空时不加限制，与「一次搜全部索引」等价。
	limit := map[string]bool{}
	for _, id := range go_.LimitLibs {
		limit[id] = true
	}

	var budget int64
	skipped := 0
	for _, l := range opt.Libs() {
		if go_.LibID != "" && l.LibID != go_.LibID {
			continue
		}
		if len(limit) > 0 && !limit[l.LibID] {
			continue
		}
		libNameOf[l.LibID] = l.LibName

		select {
		case <-ctx.Done():
			res.Timeout = true
			res.ElapsedMs = time.Since(start).Milliseconds()
			return res
		default:
		}

		res.ScannedLib++
		rd, err := opt.OpenPackage(l.FilePath)
		if err != nil {
			continue
		}
		scanLibContent(rd, l, lq, &go_, &res, &skipped, &budget, ctx)
		rd.Close()

		// 收集够了就直接停。让 scanLibContent 在收集满后继续「只计数不
		// 收集」地扫完整个包，能把 Matched 变成精确值，代价是每个包都要
		// 完整解压一遍（首包约 7800 篇 HTML、3.5 秒），而本页要的结果
		// 早就拿到了。只有调用方明确要精确计数时才继续扫。
		if budget >= go_.BudgetByte {
			res.Truncated = true
			break
		}
		if go_.CollectLimit > 0 && len(res.Hits) >= go_.CollectLimit && !go_.WantsExactCount {
			break
		}
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res
}

// scanLibContent 扫描单个包里的所有 HTML。
//
// skip 记录已跳过的命中数，用于确定性分页：翻到第 2 页时前 N 条仍然会被
// 匹配到，只是不再收集，所以第 2 页的内容与前一次扫描的前 N 条完全一致，
// 不串页也不漏。
//
// 收集上限不等于停止计数。已收集条数达到 CollectLimit 后，本函数是否继续
// 扫完本库由 WantsExactCount 决定（见该字段的注释）。
func scanLibContent(rd *doclib.Reader, l doclib.LibMeta, lq string, go_ *GrepOptions,
	res *GrepResult, skip *int, budget *int64, ctx context.Context) {

	for _, f := range rd.Files() {
		if *budget >= go_.BudgetByte {
			return
		}
		if (res.Scanned & 0x3f) == 0 {
			select {
			case <-ctx.Done():
				res.Timeout = true
				return
			default:
			}
		}

		lower := strings.ToLower(f.Name)
		if !strings.HasSuffix(lower, ".html") || !strings.HasPrefix(lower, "resources/") {
			continue
		}
		// 跳过明显的导航/模板碎片，只搜正文
		if isNoisePath(lower) {
			continue
		}
		// 单篇解压上限：正常正文几十 KB，超过 1MB 的多是索引/打包文件
		const maxDoc = 1 << 20
		if f.UncompressedSize64 > maxDoc {
			continue
		}

		raw, err := rd.ReadAllBounded(f.Name, maxDoc)
		if err != nil {
			continue
		}
		res.Scanned++
		*budget += int64(len(raw))

		text := htmltext.ExtractText(raw)
		if text == "" {
			continue
		}
		if !containsFold(text, lq) {
			continue
		}

		// matched 统计「命中篇数」，与是否被 skip、是否已达收集上限都无关，
		// 因此首页也能报出准确的总命中篇数。
		res.Matched++

		if *skip < go_.SkipHits { // 本页之前的部分，跳过不收集
			*skip++
			continue
		}
		if go_.CollectLimit > 0 && len(res.Hits) >= go_.CollectLimit {
			if !go_.WantsExactCount {
				return
			}
			continue
		}

		rel := strings.TrimPrefix(f.Name, "resources/")
		hit := GrepHit{
			LibID:   l.LibID,
			LibName: l.LibName,
			DocID:   f.Name,
			URL:     rel,
			Title:   htmltext.ExtractTitle(raw),
		}
		hit.Snippet, hit.Count = snippets(text, go_.Query, go_.MaxPerDoc)
		if hit.Snippet == "" {
			continue
		}
		res.Hits = append(res.Hits, hit)
	}
}

// isNoisePath 判断是否是无需检索的导航类页面。
// 实现放在 internal/htmltext，语料库（internal/corpus）要用同一份判定，
// 两边各写一份必然漂移。这里只做转发。
func isNoisePath(lower string) bool { return htmltext.IsNoisePath(lower) }

// containsFold 判断 text 是否含 q（忽略大小写）。
// text 已是 UTF-8，故可直接用 strings 处理。
func containsFold(text, lq string) bool {
	if lq == "" {
		return false
	}
	return strings.Contains(textfold.ASCIILower(text), lq)
}

// snippets 从纯文本里抽出至多 max 段命中上下文，命中词用 <em> 包裹。
//
// 返回的片段已经是 HTML 片段，前端直接 innerHTML 即可。
func snippets(text, query string, max int) (string, int) {
	// 两处都必须是字节长度不变的折叠：下面的 at 既用来切 ltext 也用来切 text。
	ltext := textfold.ASCIILower(text)
	lq := textfold.ASCIILower(query)
	if lq == "" {
		return "", 0
	}

	const ctxLen = 60
	var parts []string
	count := 0
	from := 0
	prevEnd := -1 // 上一段摘要在 text 里的结束位置
	for len(parts) < max {
		i := strings.Index(ltext[from:], lq)
		if i < 0 {
			break
		}
		at := from + i
		count++
		if len(parts) < max {
			start := at - ctxLen
			if start < 0 {
				start = 0
			}
			end := at + len(query) + ctxLen
			if end > len(text) {
				end = len(text)
			}
			// 对齐到 rune 边界，避免切断多字节字符
			start = alignRune(text, start)
			end = alignRune(text, end)
			// 与上一段重叠就不再收：短文档里相邻两次命中会产出两段几乎
			// 一模一样的文字，卡片上看起来像重复渲染。跳过不影响 count。
			if start >= prevEnd {
				// 三段各自转义后再拼高亮标签。不要先拼成一个含占位符的
				// 整串再整体转义：正文里恰好出现该占位符时会被替换成
				// <em>，等于让文档内容往结果里注入标记。
				seg := escapeHTML(text[start:at]) +
					"<em>" + escapeHTML(text[at:at+len(query)]) + "</em>" +
					escapeHTML(text[at+len(query):end])
				if start > 0 {
					seg = "…" + seg
				}
				if end < len(text) {
					seg += "…"
				}
				parts = append(parts, seg)
				prevEnd = end
			}
		}
		from = at + len(query)
		// 继续统计同一篇里的其它命中，但避免无限循环
		if count > 500 {
			break
		}
	}
	return strings.Join(parts, " ｜ "), count
}

// alignRune 把下标调整到合法的 UTF-8 rune 起点。
func alignRune(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// escapeHTML 转义 HTML 文本上下文里的三个敏感字符。
//
// 结果会被前端用 innerHTML 插入，因此所有来自文档内容的片段都必须先过这里。
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
