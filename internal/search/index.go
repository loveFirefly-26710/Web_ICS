// Package search 提供两类搜索能力：
//
//   - 标题索引（TitleIndex）：扫描全部包的 navi.xml，把几十万条目录标题建成
//     常驻内存索引，查询是对标题列的全量子串扫描。
//   - 正文检索（Grep）：按需流式扫描 zip 内的 HTML，剥标签后匹配关键词。
//     不建常驻索引（全库 90 万篇 HTML 建索引要 1~3 GB，超出 1G 预算），
//     改用「单包流式扫 + 收集上限 + 超时」的控制方式。
//
// 内存预算：83 万条标题合计约 20 MB 文本，加上结构体与索引约 290 MB 常驻。
// 为不突破 1G 上限，索引可由内存守卫在压力下释放（见 Release）。
package search

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"web_ics/internal/doclib"
)

// 构建等待的重试间隔。构建一次要数秒，用 runtime.Gosched() 空转会
// 把 CPU 烧在一个纯粹等待的循环上，因此改成定时重试。
const buildWaitInterval = 5 * time.Millisecond

// buildGrace 是「释放时等一次在途构建完成」的上限。
//
// 必须有这个宽限，而不是立刻作废：一次全库标题索引构建要 4.5 秒左右，
// 若 Release 一进来就把在途构建当场作废，就会出现「构建烧掉几秒，出锁时
// 发现被作废、结果被丢弃，然后立刻从头再来」的循环。查询线程一直构建、
// 一直被打断，永远填不上 cols，用户看到的只是一直「检索中…」。
//
// 宽限窗口把「作废在途构建」的节奏降下来，一次正常发布的构建因此都能落地；
// 而那些与构建叠得太紧的 Release 会重试（见 Release），不会丢掉释放意图。
//
// 这个值必须大于一次真实构建的耗时，否则宽限期一到就作废在途构建，等于每轮都白干。
const buildGrace = 10 * time.Second

// titleCols 是列式存储的标题索引。
//
// 用列式而不是 []struct：每行 6 个 string 的结构体每行 96 B，扫描时要跨
// 几十万行做指针追逐，CPU 缓存命中率极低。列式把 titles 单独连续存放
// （约 20 MB），扫描只触碰这一份紧凑数组。
//
// 附带好处：Go 的 GC 需要扫描对象里的每个指针，[]struct 有几十万个对象
// 乘以 6 个指针；列式只有 8 个切片对象，GC 扫描量降几个数量级。
type titleCols struct {
	n       int
	titles  []string // 标题
	libIDs  []uint16 // 指向 libList 的下标（库数量有限，uint16 足够）
	urls    []string // 相对 resources/ 的 URL
	nodeID  []string
	parID   []string
	breadth []string
	libList []string // 库 ID 字典
}

// titleRow 只用于构建阶段的临时载体。
type titleRow struct {
	libID    string
	nodeID   string
	title    string
	url      string
	parentID string
	breadth  string
}

// stringPool 是字符串去重池，让重复字符串共享同一份底层内存。
//
// 几十万行里 libID 只有几十种、breadth（祖先链）重复率也极高，
// 不去重时这些字符串会被复制几十万份。
type stringPool struct {
	m map[string]string
}

func newStringPool(n int) *stringPool { return &stringPool{m: make(map[string]string, n)} }

// intern 返回 s 的共享副本。空串直接返回，避免占用 map 项。
func (p *stringPool) intern(s string) string {
	if s == "" {
		return ""
	}
	if v, ok := p.m[s]; ok {
		return v
	}
	p.m[s] = s
	return s
}

// toCols 把构建期的行切片转成列式存储。
func toCols(rows []titleRow) *titleCols {
	c := &titleCols{
		n:       len(rows),
		titles:  make([]string, len(rows)),
		libIDs:  make([]uint16, len(rows)),
		urls:    make([]string, len(rows)),
		nodeID:  make([]string, len(rows)),
		parID:   make([]string, len(rows)),
		breadth: make([]string, len(rows)),
	}
	idx := make(map[string]uint16, 64)
	for i := range rows {
		r := &rows[i]
		k, ok := idx[r.libID]
		if !ok {
			k = uint16(len(c.libList))
			idx[r.libID] = k
			c.libList = append(c.libList, r.libID)
		}
		c.titles[i] = r.title
		c.libIDs[i] = k
		c.urls[i] = r.url
		c.nodeID[i] = r.nodeID
		c.parID[i] = r.parentID
		c.breadth[i] = r.breadth
	}
	return c
}

// TitleIndex 是标题索引。
type TitleIndex struct {
	build func() *titleCols // 由 server 注入的加载函数，避免本包依赖 doclib 扫描逻辑
	cols  *titleCols
	libs  map[string]string // libID -> libName

	// 分类维度：libID -> categoryKey，让搜索页也能按领域过滤
	catOf map[string]string

	// buildMu 串行化「构建 / 释放」这对互斥动作，与查询路径的 mu 分开。
	//
	// 不能只用 mu：查询路径要在持 mu.RLock 的状态下扫全量标题（数十毫秒），
	// 而内存守卫会在另一个 goroutine 调 Release。若 Release 用 mu.Lock，
	// 就会形成「写者等读锁释放 / 读者等写者」的环。
	//
	// 锁序固定为 buildMu -> mu，且绝不反向获取。
	buildMu sync.Mutex

	// refreshMu 串行化「构建」这一个动作。
	//
	// 为什么要和 buildMu 分开：构建是长操作。若构建期间一直持 buildMu，
	// 则内存守卫的 Release 会被卡住整整一次构建时长，而守卫是每 5 秒一次，
	// 结果是守卫排队、内存也救不下来。分开后 Release 只在挂载/卸载索引的
	// 瞬间拿 buildMu（微秒级），构建过程本身由 refreshMu 串行。
	//
	// 锁序固定为 refreshMu -> buildMu -> mu。
	refreshMu sync.Mutex

	// refresh 是索引刷新状态机（refreshIdle/Running/Done/Dead）。
	//
	// Release 与构建是并发的，必须能回答两个问题：有没有人在构建（决定
	// Release 要不要把索引作废），以及构建完成后它的结果还算不算数
	// （被 Release 打断的就永不挂载）。只用锁回答不了后者，构建者可能
	// 已经出锁了才发现自己白干。
	refresh atomic.Int32

	mu        sync.RWMutex
	built     bool
	buildMs   int64
	nodeCount int
}

// 索引刷新状态。
const (
	refreshIdle int32 = iota
	refreshRunning
	refreshDone // 构建结束，但结果尚未/已被 Release 丢弃
	refreshDead // 被释放打断，本次构建已取消，其索引永不挂载
)

// Options 是构造标题索引所需的依赖。
type Options struct {
	// Libs 返回全部库的元数据。
	Libs func() []doclib.LibMeta
	// OpenPackage 打开一个包用于读取 navi.xml。
	OpenPackage func(path string) (*doclib.Reader, error)
	// CategoryOf 给出库的产品领域键。
	CategoryOf func(doclib.LibMeta) string
}

// NewTitleIndex 构造标题索引（不立即构建，首次查询或显式 Build 时构建）。
func NewTitleIndex(opt Options) *TitleIndex {
	ti := &TitleIndex{libs: map[string]string{}, catOf: map[string]string{}}
	ti.build = func() *titleCols { return loadTitles(opt, ti) }
	return ti
}

// Build 显式构建索引，返回耗时（毫秒）与节点数。
func (t *TitleIndex) Build() (int64, int) {
	t.ensure()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.buildMs, t.nodeCount
}

// maxBuildAttempts 限制「构建被守卫作废后重试几次」。
//
// 不设上限的话，内存压力持续时这里会无限重试，每个查询都卡住。
const maxBuildAttempts = 3

// ensure 保证索引可用。所有查询路径都必须走它。
//
// 不用 sync.Once：Once.Do 在回调返回前会阻塞其他所有调用者，而这里的回调
// （构建全库标题索引）要数秒，中间还夹着内存守卫的 Release。「进入后不许
// 他人插队」的语义，跟「内存压力下随时要能放弃」是冲突的。
//
// 也不用「一把锁加一个标志」：构建期间一直持锁的话，守卫的 Release 要等满
// 一次构建；中途出锁又会出现多个查询各自构建一份、结果集互不相同。
// 所以用显式状态机：未构建时，抢到 refreshRunning 的人负责构建，其余人等
// 它的结果；Release 把状态打成 refreshDead，正在构建的那份结果就再也不会挂载。
func (t *TitleIndex) ensure() {
	attempts := 0
	for {
		if t.loadCols() != nil {
			return
		}

		switch t.refresh.Load() {
		case refreshRunning:
			// 已经有人在构建。等它出结果，不要自己再建一份，
			// 否则 N 个并发查询会各建一份索引，内存直接翻 N 倍。
			time.Sleep(buildWaitInterval)
			continue
		case refreshDead:
			// 上一次构建被 Release 打断了，需要重建。先把标记复位，
			// 让某一方抢到构建权（CAS 只对一方成功，其余人重试）。
			//
			// CAS 失败后必须走下一步去抢构建权，不能 continue：重来一轮
			// 只会看到 refreshIdle，于是所有人都在「把 idle CAS 成
			// running，有人成功，其余人 CAS 失败」之间打转，
			// 表现为 goroutine 一直活着却什么都没做。
			if t.refresh.CompareAndSwap(refreshDead, refreshIdle) {
				continue // 复位成功，下一轮去抢构建权
			}
			fallthrough
		case refreshDone, refreshIdle:
			// 轮到我构建。
			if !t.refresh.CompareAndSwap(t.refresh.Load(), refreshRunning) {
				continue // 有人抢到了，回到循环等它出结果
			}
			if t.refreshIndex() {
				return
			}
			// 构建被 Release 作废了（守卫在构建期间又清了一次）。
			//
			// 这里不能直接返回：调用方拿到 nil 索引会静默返回 0 条标题
			// 命中，用户看到的是「没搜到」，实际是索引压根没建起来。
			attempts++
			if attempts >= maxBuildAttempts {
				return
			}
			continue
		}
	}
}

// refreshIndex 构建并挂载一次索引。调用前必须已把 refresh 置为 refreshRunning。
//
// 构建过程只持 refreshMu，不持 buildMu，这样内存守卫的 Release 不会被一次
// 构建卡住（守卫是救命的，不能排队）。
// 返回 true 表示索引已挂载可用；false 表示构建期间被 Release 作废。
func (t *TitleIndex) refreshIndex() bool {
	t.refreshMu.Lock()
	start := time.Now()
	cols := t.build()
	elapsed := time.Since(start).Milliseconds()
	t.refreshMu.Unlock()

	// 挂载。若构建期间被 Release 打断过（状态被改成 refreshDead），
	// 这份索引就作废，否则内存守卫刚省下的内存会立刻被我们放回去。
	if t.refresh.CompareAndSwap(refreshRunning, refreshDone) {
		t.mu.Lock()
		t.cols = cols
		t.built = true
		if cols != nil {
			t.nodeCount = cols.n
		}
		t.buildMs = elapsed
		t.mu.Unlock()
		return true
	}
	t.mu.Lock()
	t.built = false
	t.mu.Unlock()
	return false
}

// loadCols 取当前索引（可能为 nil）。
func (t *TitleIndex) loadCols() *titleCols {
	t.mu.RLock()
	cols := t.cols
	t.mu.RUnlock()
	return cols
}

// Release 释放索引占用的内存（内存守卫在软阈值时调用）。
//
// 这里必须用 buildMu 而不是 mu：调用方（内存守卫）是在其它 goroutine 正在
// 查询时被打断进来调用的，而查询路径持有 mu.RLock。用 mu.Lock 会形成
// 「写者等读锁释放、读者又要等写者」的死锁。
func (t *TitleIndex) Release() {
	t.buildMu.Lock()
	defer t.buildMu.Unlock()

	// 有构建在途时，先给它一个宽限窗口，不要当场作废（原因见 buildGrace）。
	// 等一次当前构建落地，换来的是「索引持续可用」，而 Release 的下一次
	// 调用（守卫每 5 秒一次）会真正把它清掉。
	if t.refresh.Load() == refreshRunning {
		deadline := time.Now().Add(buildGrace)
		for time.Now().Before(deadline) && t.refresh.Load() == refreshRunning {
			time.Sleep(buildWaitInterval)
		}
		if t.refresh.Load() == refreshRunning {
			// 超了宽限还没建完：作废它，避免守卫被无限拖住。
			t.refresh.CompareAndSwap(refreshRunning, refreshDead)
		}
	}

	if t.cols == nil {
		// 已经没东西可放。注意不能在这里提前返回前把 refresh 复位：
		// 若上一次构建被作废（refreshDead），必须让它回到 idle，
		// 否则后续查询会一直看到 refreshDead 而反复重建。
		t.refresh.CompareAndSwap(refreshDead, refreshIdle)
		return
	}
	// 在 buildMu 下清空，保证没有查询会拿到「半释放」的状态。
	t.mu.Lock()
	t.cols = nil
	t.built = false
	t.mu.Unlock()
}

// hasASCIILetter 判断查询词里是否有 ASCII 字母（有才需要大小写处理）。
func hasASCIILetter(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return true
		}
	}
	return false
}

// caseInsensitiveAt 检查以 foldQ（小写）命中的位置，在原串里是否
// 忽略大小写地等于 foldQ。
//
// 用于「先用向量化的区分大小写 Index 粗筛，再对少量命中做精确校验」
// 这条路径：粗筛命中不代表真的匹配（例如查 "sap" 会命中 "SAPid"），
// 必须校验，否则会把不相关标题放进结果。
func caseInsensitiveAt(s, foldQ string) bool {
	i := strings.Index(s, foldQ)
	if i < 0 {
		return false
	}
	return strings.EqualFold(s[i:i+len(foldQ)], foldQ)
}

// scoreOfLower 给出匹配质量分，越小越好。
//
// 调用方必须保证 lower 已是小写形式。热路径要对几十万行调用它，
// 每次都多做一次 ToLower 是实打实的分配开销，所以这里不做转换。
func scoreOfLower(lower, lq string, idx int) int {
	switch {
	case lower == lq:
		return 0
	case idx == 0:
		return 1
	case isWordBoundary(lower[idx-1]):
		return 2
	default:
		return 3
	}
}

// isWordBoundary 判断该字节是否算词边界（用于给"词首命中"加分）。
func isWordBoundary(b byte) bool {
	switch b {
	case ' ', '-', '_', '/', '.', '(', '[', ':', ',', '+':
		return true
	}
	return b < 0x80 && !unicode.IsLetter(rune(b)) && !unicode.IsDigit(rune(b))
}

// loadTitles 扫描所有包的 navi.xml 与（回退）FileList.xml，抽出标题并转列式。
func loadTitles(opt Options, t *TitleIndex) *titleCols {
	libs := opt.Libs()
	libName := make(map[string]string, len(libs))
	catOf := make(map[string]string, len(libs))
	for _, l := range libs {
		libName[l.LibID] = l.LibName
		if opt.CategoryOf != nil {
			catOf[l.LibID] = opt.CategoryOf(l)
		}
	}

	// 预估总量，避免 append 反复扩容
	all := make([]titleRow, 0, 64*1024)
	pool := newStringPool(1 << 20)
	for _, l := range libs {
		all = append(all, loadOneLib(opt, l, pool)...)
	}

	// 统一回填给查询路径用的映射（构建期间的写入，对外不可见）
	t.mu.Lock()
	t.libs = libName
	t.catOf = catOf
	t.mu.Unlock()

	return toCols(all)
}

// loadOneLib 解析单个包的目录标题。
func loadOneLib(opt Options, meta doclib.LibMeta, pool *stringPool) []titleRow {
	rd, err := opt.OpenPackage(meta.FilePath)
	if err != nil {
		return nil
	}
	defer rd.Close()

	raw, err := rd.ReadAllBounded("resources/navi.xml", maxMetaBytes)
	if err == nil && len(raw) > 0 {
		if rows := parseNavTitles(meta.LibID, raw, pool); len(rows) > 0 {
			return rows
		}
	}
	// 回退：navi.xml 缺失（部分 hwics）或无有效节点，改用 FileList.xml 平铺
	return parseFileListTitles(rd, meta.LibID, pool)
}

// maxMetaBytes 是包内元数据文件的读取上限，与 internal/nav 保持一致。
// 实际最大的 .hhc 是 10.54 MB，这里留 3 倍余量。
const maxMetaBytes = 32 << 20

// 单包的解析上限。三条一起用，把「畸形文档包」的最坏内存占用钉死。
//
// 章节路径是把祖先标题用 " / " 连起来，深度 N 的线性链会产生 O(N²) 的
// 字符串总量，而且全部进只增不删的 stringPool。一个 36 MB / 100 万层的
// 畸形 navi.xml 能让单次搜索把 RSS 从 1.3 GB 推到 7.7 GB。
//
// 正常文档包的规模：最大 navi.xml 8.8 MB、36111 个节点、最深路径十几级。
// 下面三个值都留了足够余量。
const (
	maxPathDepth   = 64       // 章节路径最多 64 级
	maxPathBytes   = 16 << 20 // 单包所有章节路径加起来不超过 16 MB
	maxNodesPerLib = 200000   // 单包最多解析 20 万个节点
)

// parseNavTitles 用流式解码抽取 <topic txt url id> 的层级信息。
//
// 最大的 navi.xml 是 8.8 MB，直接 Unmarshal 成 DOM 不划算。
// 三条上限都是为了掐掉内存放大，见各自的注释。
func parseNavTitles(libID string, raw []byte, pool *stringPool) []titleRow {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false

	var (
		rows      []titleRow
		stack     []string // 祖先标题栈
		nodes     int
		pathBytes int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 文档里有未转义字符，宽容处理：返回已解析部分
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local != "topic" {
				continue
			}
			var txt, url, id string
			for _, a := range el.Attr {
				switch a.Name.Local {
				case "txt":
					txt = a.Value
				case "url":
					url = a.Value
				case "id":
					id = a.Value
				}
			}
			if txt == "" {
				txt = url // 无标题时用 URL 兜底，至少可搜到
			}
			parent := ""
			if n := len(stack); n > 0 {
				parent = stack[n-1]
			}

			// 章节路径只取最近 maxPathDepth 级。再往上是全库通用的前缀，
			// 对用户没有信息量，却是 O(N²) 的来源。
			lo := 0
			if n := len(stack); n > maxPathDepth {
				lo = n - maxPathDepth
			}
			breadth := ""
			if pathBytes < maxPathBytes {
				breadth = strings.Join(stack[lo:], " / ")
				pathBytes += len(breadth)
			}

			rows = append(rows, titleRow{
				libID:    pool.intern(libID),
				nodeID:   pool.intern(id),
				title:    pool.intern(txt),
				url:      pool.intern(url),
				parentID: pool.intern(parent),
				breadth:  pool.intern(breadth),
			})
			nodes++
			if nodes >= maxNodesPerLib {
				break
			}
			if len(stack) < maxPathDepth {
				stack = append(stack, txt)
			}
		case xml.EndElement:
			if el.Name.Local == "topic" && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return rows
}

// parseFileListTitles 兜底：从 FileList.xml 读出平铺条目。
func parseFileListTitles(rd *doclib.Reader, libID string, pool *stringPool) []titleRow {
	for _, f := range rd.Files() {
		if !strings.HasSuffix(strings.ToLower(f.Name), "filelist.xml") {
			continue
		}
		raw, err := rd.ReadAllBounded(f.Name, maxMetaBytes)
		if err != nil {
			return nil
		}
		dec := xml.NewDecoder(bytes.NewReader(raw))
		dec.Strict = false
		var rows []titleRow
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			se, ok := tok.(xml.StartElement)
			if !ok || se.Name.Local != "file" {
				continue
			}
			var url, title string
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "url":
					url = a.Value
				case "title", "name", "txt":
					title = a.Value
				}
			}
			if url == "" {
				continue
			}
			if title == "" {
				title = url
				if i := strings.LastIndex(url, "/"); i >= 0 {
					title = url[i+1:]
				}
			}
			rows = append(rows, titleRow{
				libID: pool.intern(libID),
				title: pool.intern(title),
				url:   pool.intern(url),
			})
		}
		return rows
	}
	return nil
}
