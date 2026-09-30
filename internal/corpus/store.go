package corpus

import (
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"web_ics/internal/doclib"
	"web_ics/internal/htmltext"
)

// Store 管理全部文档包的语料分片：建、查、报状态。
//
// 生命周期：启动时 Refresh() 找出缺失或过期的分片，后台逐个 BuildOne()；
// 查询时 Query() 用已经建好的分片做全量扫描。建好之前该包的正文不参与
// 检索（响应里如实报告「语料 X/59」），而不是退回「只扫一个包」那种
// 会让用户误判的半成品结果。
//
// 分片按文档包索引，不是按 libID。文档库里有多组同名同 libID 的不同发行版，
// 用 libID 当文件名的话后建的会覆盖先建的，59 个包只落成 55 个分片，
// 而且 Refresh 还认为它们都就绪，静默漏掉 4 个包的正文。
type Store struct {
	dir  string
	libs []doclib.LibMeta

	paths []string // 与 libs 同长，每个包一个分片路径

	mu      sync.RWMutex
	fresh   []bool
	lastErr string

	buildMu  sync.Mutex
	building bool
}

// NewStore 创建语料库。dir 为空时用 <doc-root>/.web_ics-corpus。
func NewStore(dir string, libs []doclib.LibMeta) *Store {
	if strings.TrimSpace(dir) == "" && len(libs) > 0 {
		dir = filepath.Join(filepath.Dir(libs[0].FilePath), ".web_ics-corpus")
	}
	s := &Store{
		dir:   dir,
		libs:  libs,
		paths: make([]string, len(libs)),
		fresh: make([]bool, len(libs)),
	}
	for i, l := range libs {
		// 文件名 = libID 前缀加包文件名的 CRC32：稳定、唯一、可读。
		// 用包文件名（而不是完整路径）是为了「把文档库整体搬到别的目录」时
		// 语料依然有效，不必重建。
		s.paths[i] = filepath.Join(dir, fmt.Sprintf("%s-%08x.icsc",
			safeName(l.LibID), crc32.ChecksumIEEE([]byte(l.FileName))))
	}
	return s
}

// Dir 返回语料目录。
func (s *Store) Dir() string { return s.dir }

// Refresh 检查每个包的分片是否存在且比包本身新，返回需要重建的包。
func (s *Store) Refresh() []doclib.LibMeta {
	var missing []doclib.LibMeta
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.libs {
		ok := shardFresh(s.paths[i], l.FilePath)
		s.fresh[i] = ok
		if !ok {
			missing = append(missing, l)
		}
	}
	return missing
}

// shardFresh 分片比包新且格式版本、抽取版本都匹配，才算可用。
//
// 必须校验版本，不能只看 mtime：
//   - 格式升级后旧分片仍然「比包新」，会被当成就绪，而 OpenShard 又会因
//     版本不符而失败，那些包的正文静默从检索里消失；
//   - 抽取逻辑升级更隐蔽：不改这两个版本号的话，分片会被判定为就绪、
//     根本不重建，症状是「改了代码但结果一点没变」，同样不报错。
func shardFresh(shard, pkg string) bool {
	si, err := os.Stat(shard)
	if err != nil {
		return false
	}
	pi, err := os.Stat(pkg)
	if err != nil {
		return false
	}
	if si.ModTime().Before(pi.ModTime()) {
		return false
	}
	f, err := os.Open(shard)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return string(hdr[:4]) == magic &&
		hdr[4] == formatVer &&
		hdr[5] == htmltext.ExtractRevision
}

// ReadyCount 返回可用分片数。
func (s *Store) ReadyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, ok := range s.fresh {
		if ok {
			n++
		}
	}
	return n
}

// Total 返回文档包总数。
func (s *Store) Total() int { return len(s.libs) }

// Building 报告后台是否正在建。
func (s *Store) Building() bool {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	return s.building
}

// SetBuilding 由后台构建流程标记状态。
func (s *Store) SetBuilding(v bool) {
	s.buildMu.Lock()
	s.building = v
	s.buildMu.Unlock()
}

// LastErr 返回最近一次构建错误（供 /debug/healthz 诊断）。
func (s *Store) LastErr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// BuildOne 重建一个包的分片。成功后立刻标记为可用。
func (s *Store) BuildOne(i int, l doclib.LibMeta) (ShardStat, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return ShardStat{}, err
	}
	st, err := BuildShard(l, s.paths[i])
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
		return st, err
	}
	s.fresh[i] = true
	return st, nil
}

// BuildAll 依次重建缺失/过期的分片（后台调用）。
//
// 串行而不是并行：建分片是「解压 + 剥标签 + 写盘」的 IO/CPU 混合负载，
// 而服务同时还要响应查询。串行把对线上请求的影响压到最小。
func (s *Store) BuildAll(missing []doclib.LibMeta, logf func(string, ...any)) {
	if len(missing) == 0 {
		return
	}
	// 待建列表按包文件路径定位回下标，libID 有重复，不能用它认包。
	want := make(map[string]bool, len(missing))
	for _, m := range missing {
		want[m.FilePath] = true
	}
	type job struct {
		idx int
		lib doclib.LibMeta
	}
	var jobs []job
	for i, l := range s.libs {
		if want[l.FilePath] {
			jobs = append(jobs, job{i, l})
		}
	}

	s.SetBuilding(true)
	defer s.SetBuilding(false)
	t0 := time.Now()
	var docs int
	var textBytes int64
	for k, j := range jobs {
		st, err := s.BuildOne(j.idx, j.lib)
		if err != nil {
			logf("[corpus] %d/%d %s 建索引失败: %v", k+1, len(jobs), j.lib.LibName, err)
			continue
		}
		docs += st.Docs
		textBytes += st.TextBytes
		logf("[corpus] %d/%d %s: %d 篇 / %.1f MB", k+1, len(jobs), j.lib.LibName,
			st.Docs, float64(st.TextBytes)/(1<<20))
	}
	logf("[corpus] 完成：%d 篇 / %.1f MB 正文，耗时 %v",
		docs, float64(textBytes)/(1<<20), time.Since(t0).Round(time.Second))
}

// RawHit 是扫描命中的紧凑表示（全库命中可能几十万条，必须省内存）。
type RawHit struct {
	Lib uint16 // 在 Query.Entries 里的下标
	Doc uint32 // 分片内文档序号
	Cnt uint32 // 该篇命中次数
}

// Query 是一次全库正文检索的结果。
//
// 命中列表是全量的（不截断），所以 total 精确、分页确定。
// 摘要按需生成：只有当前页那几十条才会去读正文，避免为几万条命中白读盘。
type Query struct {
	Query     string
	Entries   []doclib.LibMeta // 本次实际扫描到的文档包
	Hits      []RawHit
	Docs      int // 本次实际扫描的文档篇数
	ElapsedMs int64

	shards []*Shard // 与 Entries 同长，nil 表示打开失败
}

// Close 释放本次查询持有的分片句柄。
func (q *Query) Close() {
	for _, sh := range q.shards {
		if sh != nil {
			sh.Close()
		}
	}
	q.shards = nil
}

// Meta 取第 i 条命中的 url 与标题。
func (q *Query) Meta(i int) (Doc, error) {
	h := q.Hits[i]
	sh := q.shards[h.Lib]
	if sh == nil {
		return Doc{}, os.ErrNotExist
	}
	return sh.DocMeta(int(h.Doc))
}

// Text 取第 i 条命中的正文纯文本（用于生成摘要）。
func (q *Query) Text(i int) (string, error) {
	h := q.Hits[i]
	sh := q.shards[h.Lib]
	if sh == nil {
		return "", os.ErrNotExist
	}
	return sh.DocText(int(h.Doc))
}

// shardOf 返回某个 libID 对应的分片（同名 libID 有多个包时取第一个可用的）。
func (q *Query) shardOf(libID string) *Shard {
	for i := range q.Entries {
		if q.Entries[i].LibID == libID && q.shards[i] != nil {
			return q.shards[i]
		}
	}
	return nil
}

// DocTextByURL 按 (libID, url) 取一篇文档的正文纯文本。
//
// 用途：标题命中要配一段正文摘要，标题索引给的是 (libID, url)，而正文在
// 语料里按文档序号存，所以先按 URL 反查序号再取文本。找不到返回 false
// （例如该页没被目录收录、或属于噪声路径没进语料）。
func (q *Query) DocTextByURL(libID, url string) (string, bool) {
	sh := q.shardOf(libID)
	if sh == nil {
		return "", false
	}
	idx, ok := sh.DocByURL(url)
	if !ok {
		return "", false
	}
	text, err := sh.DocText(idx)
	if err != nil {
		return "", false
	}
	return text, true
}

// LibID / LibName 返回第 i 条命中所属的包。
func (q *Query) LibID(i int) string   { return q.Entries[q.Hits[i].Lib].LibID }
func (q *Query) LibName(i int) string { return q.Entries[q.Hits[i].Lib].LibName }

// Query 对满足 keep 的文档包做一次全库正文扫描。
//
// keep 与标题段用的是同一个「这个库要不要算」判断（分类加单库范围），
// 因此不会出现「标题只本文档、正文却来自全库」的串档。
// keep 为 nil 表示全部。
func (s *Store) Query(keep func(string) bool, q string) (*Query, error) {
	s.mu.RLock()
	type target struct {
		entry doclib.LibMeta
		path  string
	}
	var targets []target
	for i, l := range s.libs {
		if !s.fresh[i] {
			continue // 语料还没建好：不参与检索，也不假装搜过
		}
		if keep != nil && !keep(l.LibID) {
			continue
		}
		targets = append(targets, target{l, s.paths[i]})
	}
	s.mu.RUnlock()

	res := &Query{Query: q}
	if len(targets) == 0 || q == "" {
		return res, nil
	}
	res.Entries = make([]doclib.LibMeta, len(targets))
	res.shards = make([]*Shard, len(targets))
	for i, t := range targets {
		res.Entries[i] = t.entry
	}

	t0 := time.Now()
	defer func() { res.ElapsedMs = time.Since(t0).Milliseconds() }()

	jobs := make(chan int)
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []RawHit
	)
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4 // 两核机器上再多也没用，反而争 IO
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// local 在这个 worker 内跨分片复用：避免每片一次中间切片分配
			var local []RawHit
			for i := range jobs {
				sh, err := OpenShard(targets[i].entry.LibID, targets[i].path)
				if err != nil {
					continue
				}
				local, err = sh.ScanInto(q, uint16(i), local)
				if err != nil {
					sh.Close()
					continue
				}
				mu.Lock()
				// 分片保持打开：本页摘要要读它的正文
				res.shards[i] = sh
				res.Docs += sh.DocN
				mu.Unlock()
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// 排序：命中次数多的在前，其次按包、按文档序号，完全确定，
	// 因此翻页不会串页，同一个查询的多次请求结果逐条一致。
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Cnt != b.Cnt {
			return a.Cnt > b.Cnt
		}
		if a.Lib != b.Lib {
			return a.Lib < b.Lib
		}
		return a.Doc < b.Doc
	})
	res.Hits = all
	return res, nil
}

// safeName 把库 ID 变成安全的文件名片段。
func safeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "lib"
	}
	return b.String()
}
