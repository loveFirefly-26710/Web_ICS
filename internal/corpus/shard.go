// Package corpus 为正文全文检索提供磁盘语料库。
//
// 为什么要做这一层，先看数字。原来的做法是每次查询逐包流式扫描 zip：
// 解压 HTML、剥标签、GBK 解码、再匹配。吞吐约 5000 篇/s，全库 82.8 万篇
// 要几分钟，所以一次请求只能推进一个包。用户看到的是「正文已扫描 1/59 库」，
// 结果也只来自那一个包。
//
// 把抽取提前做掉之后，剩下的扫描其实很快：
//
//	全库正文纯文本 ≈ 1.6 GB
//	strings.Index 吞吐 1.7~6 GB/s（Go 的 SIMD IndexByte）
//	→ 全量扫一遍只要 0.25~0.9 s，两核并行 ~0.15~0.5 s
//
// 所以不需要倒排索引。把纯文本落盘（每个包一个分片），每次查询把全部
// 文档包从头扫一遍，结果天然精确、天然覆盖全库、天然确定性分页，
// 代码量还比建索引少一个数量级。
//
// 代价只有磁盘：语料是未压缩的纯文本，全库约 1.8 GB。不压缩是刻意的，
// 压缩后随机读一篇要解压整块，而扫描时 1.8 GB 顺序读在任何磁盘上都不慢，
// 解压反而会变成新瓶颈。
//
// 分片格式：每包一个文件，便于增量重建（包没变就不重建）
//
//	0   魔数 "WICS" + 格式版本 + 抽取版本（htmltext.ExtractRevision）
//	8   docCount u32
//	12  metaBlobLen u32
//	16  textBlobLen u64
//	24  textOff u64 / offsetsOff u64 / metaOffsetsOff u64 / metaBlobOff u64
//	56  urlIdxOff u64
//	64  textBlob：每篇正文 + '\n'（分隔符防止跨篇误匹配）
//	    offsets：docCount × u32，正文起点（升序）
//	    metaOffsets：docCount × u32，指向 metaBlob
//	    metaBlob：每篇 [u16 urlLen][url][u16 titleLen][title][u16 breadthLen][breadth]
//	              breadth 是目录里的层级（「命令参考>安全>PKI配置命令」），
//	              建索引时从 navi.xml/.hhc 里取。正文命中的搜索结果没有节点
//	              信息，只能靠这里带上，否则结果卡片少了官方那行路径
//	    urlIdx：docCount × [u64 urlHash][u32 docIdx]，按 hash 升序
//
// urlIdx 是给「标题命中也要配正文摘要」用的：标题索引给出的是 (libID, url)，
// 而正文在语料里按文档序号存，必须能按 URL 反查（二分加 ReadAt）。
// hash 用归一化后的 URL（小写、去锚点、去 resources/ 前缀）。目录里的 URL
// 大小写常与包内文件名不一致，不归一化就查不到。
//
// 只有 offsets 常驻内存（3.6 万篇 = 144 KB），其余按需 ReadAt。
package corpus

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"web_ics/internal/doclib"
	"web_ics/internal/htmltext"
	"web_ics/internal/nav"
)

const (
	magic      = "WICS"
	formatVer  = 2 // v2 增加了 urlIdx（URL -> 文档序号），见文件头注释
	headerSize = 64
)

// Doc 是一篇文档的元信息（按需从 metaBlob 读出，不常驻）。
type Doc struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Breadth string `json:"breadth,omitempty"` // 目录里的层级（如「命令参考>安全>PKI配置命令」）
}

// Shard 是某个文档包的语料分片。
type Shard struct {
	LibID   string
	Path    string
	DocN    int
	TextLen int64

	f             *os.File
	offsets       []uint32
	textOff       int64
	urlIdxOff     int64 // 排序后的 (urlHash, docIdx) 数组，用于按 URL 反查
	metaOffsetsAt int64
	metaBlobAt    int64
	metaBlobLen   int64
}

// BuildShard 从一个文档包抽取正文，写出语料分片。
//
// 流式写：逐篇抽取，直接写进 textBlob，不在内存里攒整包的文本。
// 最大的包正文可达 70 MB，攒着会顶到内存软限。
func BuildShard(lib doclib.LibMeta, outPath string) (ShardStat, error) {
	var st ShardStat
	rd, err := doclib.OpenPackage(lib.FilePath)
	if err != nil {
		return st, err
	}
	defer rd.Close()

	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return st, err
	}
	defer func() {
		f.Close()
		os.Remove(tmp) // 成功后已 rename，这里只清理失败残留
	}()

	if _, err := f.Write(make([]byte, headerSize)); err != nil { // 头部占位，最后回填
		return st, err
	}
	w := bufio.NewWriterSize(f, 1<<20)

	// 目录层级表：url -> 「父 / 祖 / …」。
	//
	// 正文命中的搜索结果只有文档本身，没有节点信息，拿不到章节路径。
	// 建索引时顺手从目录树里取出来存进 meta，查询时就不用再解析目录树了
	// （目录树缓存容量只有 1 个包，几十个包逐个解析要十几秒）。
	breadthOf := buildBreadthMap(rd)

	var (
		offsets     []uint32
		metaOffsets []uint32
		metaBlob    bytes.Buffer
		urlHashes   []uint64 // 与文档同序；最后排序成 urlIdx
		textPos     int64
	)
	for _, zf := range rd.Files() {
		name := zf.Name
		if zf.UncompressedSize64 > maxDocBytes {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".html") ||
			!strings.HasPrefix(strings.ToLower(name), "resources/") {
			continue
		}
		if htmltext.IsNoisePath(strings.ToLower(name)) {
			continue
		}
		raw, err := rd.ReadAllBounded(name, maxDocBytes)
		if err != nil {
			continue
		}
		text := htmltext.ExtractText(raw)
		if text == "" {
			continue
		}
		offsets = append(offsets, uint32(textPos))
		metaOffsets = append(metaOffsets, uint32(metaBlob.Len()))
		url := strings.TrimPrefix(name, "resources/")
		title := htmltext.ExtractTitle(raw)
		key := normalizeURLKey(url)
		writeMeta(&metaBlob, url, title, breadthOf[key])
		urlHashes = append(urlHashes, urlHash(key))

		if _, err := w.WriteString(text); err != nil {
			return st, err
		}
		if err := w.WriteByte('\n'); err != nil { // 分隔符：防止跨篇误匹配
			return st, err
		}
		textPos += int64(len(text)) + 1
		st.Docs++
		st.TextBytes += int64(len(text))
	}
	if err := w.Flush(); err != nil {
		return st, err
	}

	offsetsAt := headerSize + textPos
	metaOffsetsAt := offsetsAt + int64(len(offsets))*4
	metaBlobAt := metaOffsetsAt + int64(len(metaOffsets))*4
	urlIdxAt := metaBlobAt + int64(metaBlob.Len())

	if err := writeU32Slice(w, offsets); err != nil {
		return st, err
	}
	if err := writeU32Slice(w, metaOffsets); err != nil {
		return st, err
	}
	if _, err := w.Write(metaBlob.Bytes()); err != nil {
		return st, err
	}
	if err := writeURLIdx(w, urlHashes); err != nil {
		return st, err
	}
	if err := w.Flush(); err != nil {
		return st, err
	}

	hdr := make([]byte, headerSize)
	copy(hdr, magic)
	hdr[4] = formatVer
	hdr[5] = htmltext.ExtractRevision // 抽取逻辑版本：改了就作废重建
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(offsets)))
	binary.LittleEndian.PutUint32(hdr[12:], uint32(metaBlob.Len()))
	binary.LittleEndian.PutUint64(hdr[16:], uint64(textPos))
	binary.LittleEndian.PutUint64(hdr[24:], uint64(headerSize))
	binary.LittleEndian.PutUint64(hdr[32:], uint64(offsetsAt))
	binary.LittleEndian.PutUint64(hdr[40:], uint64(metaOffsetsAt))
	binary.LittleEndian.PutUint64(hdr[48:], uint64(metaBlobAt))
	binary.LittleEndian.PutUint64(hdr[56:], uint64(urlIdxAt))
	if _, err := f.WriteAt(hdr, 0); err != nil {
		return st, err
	}
	if err := f.Sync(); err != nil {
		return st, err
	}
	if err := f.Close(); err != nil {
		return st, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return st, err
	}
	return st, nil
}

// ShardStat 是一次建分片的统计。
type ShardStat struct {
	Docs      int
	TextBytes int64
}

const maxDocBytes = 1 << 20

func writeMeta(b *bytes.Buffer, url, title, breadth string) {
	var tmp [2]byte
	for _, s := range []string{url, title, breadth} {
		if len(s) > 0xFFFF {
			s = s[:0xFFFF]
		}
		binary.LittleEndian.PutUint16(tmp[:], uint16(len(s)))
		b.Write(tmp[:])
		b.WriteString(s)
	}
}

// buildBreadthMap 从目录树建「url -> 祖先标题链」。
//
// 分隔符与标题索引保持一致（" / "），这样标题命中与正文命中显示出来的
// 章节路径是同一套写法。
func buildBreadthMap(rd *doclib.Reader) map[string]string {
	tree, err := nav.Parse(rd)
	if err != nil || tree == nil || len(tree.Nodes) == 0 {
		return nil
	}
	titleOf := make(map[string]string, len(tree.Nodes))
	parentOf := make(map[string]string, len(tree.Nodes))
	for i := range tree.Nodes {
		n := tree.Nodes[i]
		if n.Title != "" {
			titleOf[n.ID] = n.Title
		}
		parentOf[n.ID] = n.Parent
	}
	out := make(map[string]string, len(tree.Nodes)/2+1)
	for i := range tree.Nodes {
		n := tree.Nodes[i]
		if n.URL == "" {
			continue
		}
		key := normalizeURLKey(n.URL)
		if _, dup := out[key]; dup {
			continue // 同一文件被多个节点引用：保留先出现的
		}
		var chain []string
		id := n.Parent
		for guard := 0; id != "" && guard < 64; guard++ {
			if t := titleOf[id]; t != "" {
				chain = append(chain, t)
			}
			id = parentOf[id]
		}
		if len(chain) == 0 {
			continue
		}
		// 反转成「外层 -> 内层」，与标题索引一致
		for a, b := 0, len(chain)-1; a < b; a, b = a+1, b-1 {
			chain[a], chain[b] = chain[b], chain[a]
		}
		out[key] = strings.Join(chain, " / ")
	}
	return out
}

// writeURLIdx 写「按 URL hash 升序」的 (hash, docIdx) 数组。
//
// 排序而不是哈希表：这样查一个 URL 只要二分加十几次 ReadAt，
// 不需要把整个索引读进内存（几十个分片全常驻要几十 MB）。
func writeURLIdx(w *bufio.Writer, hashes []uint64) error {
	order := make([]uint32, len(hashes))
	for i := range order {
		order[i] = uint32(i)
	}
	sort.Slice(order, func(a, b int) bool { return hashes[order[a]] < hashes[order[b]] })
	var buf [12]byte
	for _, idx := range order {
		binary.LittleEndian.PutUint64(buf[:8], hashes[idx])
		binary.LittleEndian.PutUint32(buf[8:], idx)
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}
	}
	return nil
}

// urlHash 是 FNV-1a 64。
func urlHash(normalized string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(normalized); i++ {
		h ^= uint64(normalized[i])
		h *= prime
	}
	return h
}

// normalizeURLKey 把 URL 归一成可比较的形式。
//
// 必须归一化：目录（navi.xml）里的 URL 大小写常与包内实际文件名不一致，
// 还有的带 #锚点。归一化 = 去空白、去锚点、去 "./" 与 "resources/" 前缀、
// 转小写。
func normalizeURLKey(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	for strings.HasPrefix(u, "./") {
		u = u[2:]
	}
	u = strings.TrimPrefix(u, "resources/")
	return strings.ToLower(u)
}

func writeU32Slice(w *bufio.Writer, v []uint32) error {
	var tmp [4]byte
	for _, x := range v {
		binary.LittleEndian.PutUint32(tmp[:], x)
		if _, err := w.Write(tmp[:]); err != nil {
			return err
		}
	}
	return nil
}

// OpenShard 打开一个语料分片。只把 offsets 读进内存。
func OpenShard(libID, path string) (*Shard, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		f.Close()
		return nil, err
	}
	if string(hdr[:4]) != magic {
		f.Close()
		return nil, fmt.Errorf("不是语料分片: %s", path)
	}
	if hdr[4] != formatVer {
		f.Close()
		return nil, fmt.Errorf("语料分片格式版本不支持: %d", hdr[4])
	}
	if hdr[5] != htmltext.ExtractRevision {
		f.Close()
		return nil, fmt.Errorf("语料分片抽取版本过期: %d != %d", hdr[5], htmltext.ExtractRevision)
	}
	s := &Shard{
		LibID:         libID,
		Path:          path,
		DocN:          int(binary.LittleEndian.Uint32(hdr[8:])),
		metaBlobLen:   int64(binary.LittleEndian.Uint32(hdr[12:])),
		TextLen:       int64(binary.LittleEndian.Uint64(hdr[16:])),
		textOff:       int64(binary.LittleEndian.Uint64(hdr[24:])),
		metaOffsetsAt: int64(binary.LittleEndian.Uint64(hdr[40:])),
		metaBlobAt:    int64(binary.LittleEndian.Uint64(hdr[48:])),
		urlIdxOff:     int64(binary.LittleEndian.Uint64(hdr[56:])),
		f:             f,
	}
	offsetsAt := int64(binary.LittleEndian.Uint64(hdr[32:]))
	buf := make([]byte, s.DocN*4)
	if _, err := f.ReadAt(buf, offsetsAt); err != nil && err != io.EOF {
		f.Close()
		return nil, err
	}
	s.offsets = make([]uint32, s.DocN)
	for i := 0; i < s.DocN; i++ {
		s.offsets[i] = binary.LittleEndian.Uint32(buf[i*4:])
	}
	return s, nil
}

// Close 关闭分片。
func (s *Shard) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// DocMeta 读出第 i 篇的 url / 标题。
func (s *Shard) DocMeta(i int) (Doc, error) {
	var d Doc
	if i < 0 || i >= s.DocN {
		return d, fmt.Errorf("文档序号越界: %d", i)
	}
	var offBuf [4]byte
	if _, err := s.f.ReadAt(offBuf[:], s.metaOffsetsAt+int64(i)*4); err != nil {
		return d, err
	}
	at := s.metaBlobAt + int64(binary.LittleEndian.Uint32(offBuf[:]))
	// 一条 meta 记录（url + 标题 + 章节路径）通常几百字节；读 2 KB 一次
	// 搞定，路径过长时按「能读多少算多少」截断。url 与标题在前，
	// 不会被截断影响。
	buf := make([]byte, 2048)
	n, err := s.f.ReadAt(buf, at)
	if n < 6 {
		return d, fmt.Errorf("meta 读取失败: %v", err)
	}
	buf = buf[:n]
	rest := buf
	out := []*string{&d.URL, &d.Title, &d.Breadth}
	for k := range out {
		if len(rest) < 2 {
			break
		}
		l := int(binary.LittleEndian.Uint16(rest[:2]))
		rest = rest[2:]
		if l > len(rest) {
			l = len(rest) // 被截断：能取多少取多少
		}
		*out[k] = string(rest[:l])
		rest = rest[l:]
	}
	if d.URL == "" {
		return d, fmt.Errorf("meta 越界")
	}
	return d, nil
}

// DocText 读出第 i 篇的正文纯文本。
func (s *Shard) DocText(i int) (string, error) {
	if i < 0 || i >= s.DocN {
		return "", fmt.Errorf("文档序号越界: %d", i)
	}
	beg := int64(s.offsets[i])
	end := s.TextLen
	if i+1 < s.DocN {
		end = int64(s.offsets[i+1])
	}
	end-- // 去掉尾部分隔符 '\n'
	if end <= beg {
		return "", nil
	}
	buf := make([]byte, end-beg)
	if _, err := s.f.ReadAt(buf, s.textOff+beg); err != nil && err != io.EOF {
		return "", err
	}
	return string(buf), nil
}

// DocByURL 按 URL 反查文档序号。
//
// 用途：标题索引给的是 (libID, url)，而正文在语料里按文档序号存，
// 「标题命中也要配一段正文摘要」就必须能按 URL 反查。
// 索引按 hash 升序排好，二分加 ReadAt 即可，不占常驻内存。
func (s *Shard) DocByURL(url string) (int, bool) {
	if s.f == nil || s.urlIdxOff <= 0 || s.DocN == 0 {
		return 0, false
	}
	h := urlHash(normalizeURLKey(url))
	lo, hi := 0, s.DocN-1
	var buf [12]byte
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		if _, err := s.f.ReadAt(buf[:], s.urlIdxOff+int64(mid)*12); err != nil {
			return 0, false
		}
		hh := binary.LittleEndian.Uint64(buf[:8])
		switch {
		case hh < h:
			lo = mid + 1
		case hh > h:
			hi = mid - 1
		default:
			return int(binary.LittleEndian.Uint32(buf[8:])), true
		}
	}
	return 0, false
}

// docOf 用二分找出偏移 pos 属于哪一篇。
func (s *Shard) docOf(pos uint32) int {
	i := sort.Search(s.DocN, func(k int) bool { return s.offsets[k] > pos })
	return i - 1
}
