package corpus

import (
	"bytes"
	"io"
	"sync"
)

// 扫描参数。分块是为了让峰值内存与包大小无关（最大包的正文可达 70 MB）。
//
// scanChunk / ctxKeep 是变量而不是常量：调小它们就能用很小的语料覆盖
// 「命中压在块边界」「命中离块尾很远」这类情形。
//
// 块大小取 1 MB 而不是 4 MB：一次查询要扫几十个分片，每片一个读缓冲，
// 4 MB 会让「每查询 N × 4 MB」的分配量把 RSS 顶到内存软限附近。
var (
	scanChunk = 1 << 20
	ctxKeep   = 512 // 块间保留的上下文，够抽摘要用（摘要取 ±60 字节）
)

// scanScratch 是每次扫描的工作缓冲，池化复用。
//
// 必须池化的原因：一次查询要扫几十个分片，每片都要一个读缓冲（scanChunk）
// 和逐篇计数表（counts，docN 个 int32）。不池化时每次查询都会产生大量
// 短命对象，GC 来不及回收就把 RSS 推高。
type scanScratch struct {
	buf    []byte
	counts []int32
}

var scratchPool = sync.Pool{New: func() any { return &scanScratch{} }}

// ScanInto 全量扫描本分片的正文，把命中直接追加到 out 上。
//
// 返回的切片按文档序号升序（只含命中的篇），Cnt 是该篇命中次数。
//
// 直接追加而不是先产出中间结构再转换：一次查询要扫几十个分片，常见词在
// 每个包里能命中几万篇，中间那层切片会变成每查询几十 MB 的垃圾。
// 直接写进调用方的紧凑切片（RawHit，12 字节）就没有这层开销。
//
// 刻意不做「够用就停」：全库扫一遍不到一秒，而停手会让「命中总数」变成
// 近似值，用户要的正是「一次就给出全部文档的结果」。
func (s *Shard) ScanInto(q string, lib uint16, out []RawHit) ([]RawHit, error) {
	if q == "" || s.DocN == 0 || s.TextLen <= 0 {
		return out, nil
	}
	needle := []byte(q)

	sc := scratchPool.Get().(*scanScratch)
	defer func() {
		if cap(sc.buf) <= 4*scanChunk { // 异常大的缓冲不留在池里
			scratchPool.Put(sc)
		}
	}()
	if cap(sc.counts) < s.DocN {
		sc.counts = make([]int32, s.DocN)
	}
	counts := sc.counts[:s.DocN]
	for i := range counts {
		counts[i] = 0
	}

	buf := sc.buf[:0]
	var (
		readOff   = s.textOff
		remaining = s.TextLen
		bufStart  int64 // buf[0] 在 textBlob 内的偏移
		nextScan  int   // buf 内的下一个扫描起点
	)
	for remaining > 0 {
		n := int64(scanChunk)
		if n > remaining {
			n = remaining
		}
		old := len(buf)
		if cap(buf) < old+int(n) {
			nb := make([]byte, old, old+int(n)+ctxKeep+len(needle))
			copy(nb, buf)
			buf = nb
		}
		buf = buf[:old+int(n)]
		if _, err := s.f.ReadAt(buf[old:], readOff); err != nil && err != io.EOF {
			sc.buf = buf[:0]
			return out, err
		}
		readOff += n
		remaining -= n

		// 起点超过 limit 的命中可能跨到下一块，留给下一轮
		limit := len(buf) - (len(needle) - 1)
		if limit < nextScan {
			limit = nextScan
		}
		for {
			i := indexFold(buf, needle, nextScan)
			if i < 0 || i >= limit {
				break
			}
			pos := uint32(bufStart + int64(i))
			di := s.docOf(pos)
			if di >= 0 && di < s.DocN {
				counts[di]++
			}
			nextScan = i + len(needle) // 不重叠计数，与摘要逻辑一致
		}

		// 只保留尾部 ctxKeep 字节作为下一块的上下文前缀
		keepFrom := limit - ctxKeep
		if keepFrom < 0 {
			keepFrom = 0
		}
		if keepFrom > 0 {
			copy(buf, buf[keepFrom:])
			buf = buf[:len(buf)-keepFrom]
			bufStart += int64(keepFrom)
			nextScan -= keepFrom
			// 必须夹到 0：若本块的最后一个命中离块尾超过 ctxKeep
			// （命中在块开头、块还有几 MB 时很常见），nextScan 会变成负数，
			// 下一轮 indexFold 切片就越界 panic。夹到 0 的语义是对的：
			// 被丢弃的前缀已经扫过，保留区的开头就是下一个待扫位置。
			if nextScan < 0 {
				nextScan = 0
			}
		}
	}
	sc.buf = buf[:0]

	for i := 0; i < s.DocN; i++ {
		if counts[i] > 0 {
			out = append(out, RawHit{Lib: lib, Doc: uint32(i), Cnt: uint32(counts[i])})
		}
	}
	return out, nil
}

// indexFold 在 buf 里从 from 起找 needle，ASCII 字母大小写不敏感。
//
// 为什么不直接 ToLower 整段再找：对 222 MB 文本做 strings.ToLower 要 1.4 s，
// 外推全库 1.6 GB 就是 9 s，比扫描本身慢一个数量级。
//
// 做法：
//   - 检索词里没有 ASCII 字母（纯中文）就直接用 bytes.Index
//     （SIMD，1.7~6 GB/s）；
//   - 有 ASCII 字母时挑第一个 ASCII 字母当锚点，用 IndexByte 找它的大小写
//     两种形式，命中后再逐字节折叠校验整个检索词。锚点选 ASCII 字母很关键：
//     中文正文里 ASCII 字母稀疏，候选极少；锚点落在中文字节（0xE6 这类）上，
//     候选会密集到退化。
func indexFold(buf, needle []byte, from int) int {
	if from < 0 {
		from = 0 // 防御：调用方算错偏移也不该 panic
	}
	if from >= len(buf) {
		return -1
	}
	if !hasASCIILetter(needle) {
		i := bytes.Index(buf[from:], needle)
		if i < 0 {
			return -1
		}
		return from + i
	}
	anchor := firstASCIILetter(needle)
	c0 := needle[anchor]
	c1 := c0 ^ 0x20 // ASCII 字母大小写互换
	for start := from; start < len(buf); {
		j := bytes.IndexByte(buf[start:], c0)
		k := bytes.IndexByte(buf[start:], c1)
		var p int
		switch {
		case j < 0 && k < 0:
			return -1
		case j < 0:
			p = k
		case k < 0:
			p = j
		case j < k:
			p = j
		default:
			p = k
		}
		at := start + p
		beg := at - anchor
		if beg >= from && beg+len(needle) <= len(buf) &&
			equalFoldBytes(buf[beg:beg+len(needle)], needle) {
			return beg
		}
		start = at + 1
	}
	return -1
}

func hasASCIILetter(b []byte) bool {
	for _, c := range b {
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return true
		}
	}
	return false
}

func firstASCIILetter(b []byte) int {
	for i, c := range b {
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return i
		}
	}
	return 0
}

func equalFoldBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
