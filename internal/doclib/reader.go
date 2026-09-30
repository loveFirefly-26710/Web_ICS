package doclib

import (
	"archive/zip"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
)

// ErrNotFound 表示包内不存在该资源。
var ErrNotFound = errors.New("资源不存在")

// Open 打开一个包用于按需读取。
//
// 调用方必须 Close。zip.OpenReader 会缓存中央目录（几百 KB 到几 MB），
// 因此对同一包的高频读取应复用 Reader 而不是反复打开。
type Reader struct {
	zr *zip.ReadCloser

	// 条目索引：首次查询时惰性构建。
	// exact 是 zip 内路径 -> *zip.File；folded 是小写路径 -> *zip.File。
	// 构建一次约 10~30ms（3.6 万条目），之后查询是 O(1)。
	// 不用线性扫描：大包每次请求扫 3.6 万条会明显拖慢正文响应。
	idxOnce sync.Once
	exact   map[string]*zip.File
	folded  map[string]*zip.File
}

// OpenPackage 打开指定路径的文档包。
func OpenPackage(path string) (*Reader, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	return &Reader{zr: zr}, nil
}

// Close 关闭底层 zip。
func (r *Reader) Close() error {
	if r.zr == nil {
		return nil
	}
	return r.zr.Close()
}

// Lookup 按 zip 内路径查找条目，路径分隔符统一为 '/'。
//
// 文档包里的 URL 大小写和包内实际文件名经常对不上。全库 74 万个目录 URL
// 里有 26.4% 只能靠忽略大小写命中，正文内部相对引用（CSS/JS/图片）这个
// 比例更高，35.5%。例如正文写 tabSection.js / customQuery.js，包内却是
// tabsection.js / customquery.js，精确匹配会让 tab 切换和样式全部 404。
//
// 策略是先精确命中，没中再走小写折叠索引回退。这条回退是复现文档自身
// 引用行为的必要条件，不是可有可无的容错。
func (r *Reader) Lookup(name string) (*zip.File, bool) {
	name = normalizeZipPath(name)
	if name == "" {
		return nil, false
	}
	r.buildIndex()
	if f, ok := r.exact[name]; ok {
		return f, true
	}
	if f, ok := r.folded[strings.ToLower(name)]; ok {
		return f, true
	}
	return nil, false
}

// buildIndex 惰性构建条目索引。只执行一次。
func (r *Reader) buildIndex() {
	r.idxOnce.Do(func() {
		files := r.zr.File
		// 预分配：避免大包反复扩容
		r.exact = make(map[string]*zip.File, len(files))
		r.folded = make(map[string]*zip.File, len(files))
		for _, f := range files {
			if _, dup := r.exact[f.Name]; !dup {
				r.exact[f.Name] = f
			}
			fl := strings.ToLower(f.Name)
			if _, dup := r.folded[fl]; dup {
				// 同一路径仅大小写不同的条目极少；保留先出现的
				continue
			}
			r.folded[fl] = f
		}
	})
}

// ReadAllBounded 读取一个条目的全部内容，但限制最大字节数。
//
// 仅用于小文件（元数据、navi.xml 的分层片段）。正文与图片一律走 Stream。
func (r *Reader) ReadAllBounded(name string, limit int64) ([]byte, error) {
	f, ok := r.Lookup(name)
	if !ok {
		return nil, ErrNotFound
	}
	if int64(f.UncompressedSize64) > limit {
		return nil, errors.New("文件超过允许的大小上限")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// Stream 以流式方式读取条目并写入 w，全程不把内容读入内存。
//
// 这是正文 HTML / 图片 / PDF 的唯一读取路径，保证固定内存占用。
func (r *Reader) Stream(name string, w io.Writer) (int64, error) {
	f, ok := r.Lookup(name)
	if !ok {
		return 0, ErrNotFound
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	// io.Copy 使用 32KB 内部缓冲，内存占用与文件大小无关
	return io.Copy(w, rc)
}

// Files 返回包内全部条目（供导航解析时定位 .hhc 等）。
func (r *Reader) Files() []*zip.File { return r.zr.File }

// normalizeZipPath 清理路径，防止 ../ 穿越。
func normalizeZipPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "/")
	cleaned := path.Clean(p)
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}
