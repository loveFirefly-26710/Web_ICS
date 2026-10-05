package search

import "web_ics/internal/corpus"

// 正文段：语料库全量检索，取代逐包流式扫描。
//
// 旧路径每次查询都要现解压 zip、剥标签、GBK 解码，约 5000 篇/s，
// 全库 82.8 万篇要几分钟，所以一次请求只能推进一个包，用户看到的是
// 「正文已扫描 1/59 库」，结果也只来自那一个包。
//
// 新路径把抽取提前到建索引阶段（每包一个语料分片），查询时只剩扫一遍
// 纯文本：strings.Index 的实测吞吐是 1.7~6 GB/s，全库 1.6 GB 只要
// 0.25~0.9 s。于是可以每次查询都扫全部文档包，拿到精确命中数和确定性
// 分页，不需要截断、进度、跳过这类近似机制。
//
// 与标题段的关系没变：标题命中 Score=0 在前，正文命中 Score=1 在后，
// 仍然是一次查询同时打标题与正文。

// corpusContent 用语料库跑正文段。
//
// skip/need 是「在正文结果流里的偏移与条数」，与标题段的坐标系拼在一起
// （见 Fused 里 offset 的算法）。注意：无论 need 是多少都会扫全库，
// 因为 total 必须精确。
func (q *ContentQuerier) corpusContent(fo FusedOptions, keep func(string) bool,
	skip, need int, res *Result, enrich []FusedHit) ([]FusedHit, bool) {

	// 范围内的包总数（含语料还没建好的），用于如实报告覆盖情况。
	total := 0
	for _, l := range q.Libs {
		if keep(l.LibID) {
			total++
		}
	}
	res.Content.TotalLibs = total

	cq, err := q.Corpus.Query(keep, fo.Query)
	if err != nil {
		return nil, false
	}
	defer cq.Close()

	res.Content.Enabled = true
	res.Content.ScannedLib = len(cq.Entries) // 实际扫到的包数
	res.Content.Scanned = cq.Docs
	res.Content.Matched = len(cq.Hits) // 精确命中篇数（全库，不截断）
	res.Content.ElapsedMs += cq.ElapsedMs
	// 「未完成」现在只意味着有包还没建好语料，与扫描进度无关。
	res.Content.Incomplete = len(cq.Entries) < total
	res.Content.MoreLibs = res.Content.Incomplete

	// 给本页的标题命中也配上正文摘要。放在 need 判断之前：
	// 标题把本页占满（need<=0）时同样需要摘要。
	q.enrichTitleHits(cq, fo, enrich)

	if need <= 0 {
		return nil, false
	}
	from := skip
	if from > len(cq.Hits) {
		from = len(cq.Hits)
	}
	to := from + need
	if to > len(cq.Hits) {
		to = len(cq.Hits)
	}

	out := make([]FusedHit, 0, to-from)
	// ContentMaxDoc 默认是 0，而 snippets(text, q, 0) 的循环一次都不执行，
	// 摘要会为空、条目被丢掉，结果是「正文命中数算对了，但本页一条正文都
	// 看不到」。所以这里兜一个默认值。
	maxSnip := fo.ContentMaxDoc
	if maxSnip <= 0 {
		maxSnip = 3
	}
	for i := from; i < to; i++ {
		doc, err := cq.Meta(i)
		if err != nil {
			continue
		}
		text, err := cq.Text(i)
		if err != nil {
			continue
		}
		// 摘要与逐包扫描路径共用同一套逻辑（含 <em> 高亮、rune 边界对齐）
		snip, _ := snippets(text, fo.Query, maxSnip)
		if snip == "" {
			continue
		}
		out = append(out, FusedHit{
			Source:  SourceContent,
			LibID:   cq.LibID(i),
			LibName: cq.LibName(i),
			Title:   doc.Title,
			URL:     doc.URL,
			Breadth: doc.Breadth, // 章节路径（建语料时从目录树带进来的）
			Snippet: snip,
			Count:   int(cq.Hits[i].Cnt),
			Score:   1,
		})
	}
	return out, to < len(cq.Hits)
}

// 标题命中配摘要时取多少段、兜底开头取多少字。
//
// 标题命中只取 2 段（正文命中取 3 段）：它的标题本身就是锚点，
// 摘要只是「不用点进去也能看出这篇讲什么」，太长反而把列表撑散。
const (
	titleExcerptSegments = 2
	titleExcerptRunes    = 110
)

// enrichTitleHits 给标题命中补一段正文摘要。
//
// 标题命中原本只有标题加章节路径，等于把内容藏在后面，用户要逐条点进去
// 才知道讲什么。摘要来源是按 (libID, url) 去语料里反查这篇的正文：
//
//   - 正文里有检索词 → 高亮摘要（并带回命中次数）
//   - 正文里没有     → 给文档开头一段，至少能看出主题
//   - 语料里没有这篇（噪声路径 / 没被目录收录）→ 保持原样，不硬凑
func (q *ContentQuerier) enrichTitleHits(cq *corpus.Query, fo FusedOptions, hits []FusedHit) {
	for i := range hits {
		h := &hits[i]
		if h.URL == "" {
			continue
		}
		text, ok := cq.DocTextByURL(h.LibID, h.URL)
		if !ok || text == "" {
			continue
		}
		snip, cnt := snippets(text, fo.Query, titleExcerptSegments)
		if snip == "" {
			snip = leadingExcerpt(text, titleExcerptRunes)
		}
		h.Snippet = snip
		h.Count = cnt
	}
}

// leadingExcerpt 取正文开头一段（正文里没有检索词可高亮时的兜底）。
func leadingExcerpt(text string, maxRunes int) string {
	r := []rune(text)
	if len(r) == 0 {
		return ""
	}
	if len(r) <= maxRunes {
		return escapeHTML(text)
	}
	return escapeHTML(string(r[:maxRunes])) + "…"
}

// CorpusReady 报告语料库是否可用（至少有一个包建好了）。
func (q *ContentQuerier) CorpusReady() bool {
	return q != nil && q.Corpus != nil && q.Corpus.ReadyCount() > 0
}
