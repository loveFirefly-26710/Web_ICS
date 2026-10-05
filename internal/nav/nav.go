// Package nav 解析文档包的目录树。
//
// 内存约束（1G）决定了三个设计点：
//   - navi.xml 走流式 XML 解析，不 Unmarshal 整棵树；
//   - 只缓存当前包的目录树（容量 1），切包即丢弃；
//   - 大包 3.6 万节点常驻约 43MB，可以接受；59 个包全缓存要 867MB，放不下。
package nav

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"sync"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"web_ics/internal/doclib"
)

// maxMetaBytes 是包内元数据文件（navi.xml / .hhc / FileList.xml）的读取上限。
//
// 实际最大的几个：navi.xml 8.8 MB、.hhc 10.54 MB、FileList.xml 5.14 MB。
// 解析出来的树要常驻，每节点约 160 字节，上限给太大等于给常驻内存留了口子。
// 32 MB 相对实际需求仍有 3 倍余量。
const maxMetaBytes = 32 << 20

// Node 是目录树的一个节点。
type Node struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	HasKid bool   `json:"hasKid"` // 是否有子节点，前端据此决定是否显示展开箭头
}

// Tree 是某个包的完整目录树（扁平存储，便于按 parent 取子节点）。
type Tree struct {
	Nodes    []Node
	Children map[string][]int // parentID -> 子节点在 Nodes 中的下标
	Roots    []int            // 顶层节点下标
	byID     map[string]int
}

// nodeCount 返回树规模，供内存评估。
func (t *Tree) nodeCount() int { return len(t.Nodes) }

// build 根据扁平节点列表构建索引。
func build(nodes []Node) *Tree {
	t := &Tree{
		Nodes:    nodes,
		Children: make(map[string][]int, len(nodes)/4+1),
		Roots:    make([]int, 0, 64),
		byID:     make(map[string]int, len(nodes)),
	}
	for i := range nodes {
		n := &nodes[i]
		if _, dup := t.byID[n.ID]; !dup {
			t.byID[n.ID] = i
		}
		if n.Parent == "" {
			t.Roots = append(t.Roots, i)
		} else {
			t.Children[n.Parent] = append(t.Children[n.Parent], i)
		}
	}
	// 标记哪些节点有子节点，避免前端无意义地请求空层
	for pid := range t.Children {
		if i, ok := t.byID[pid]; ok {
			t.Nodes[i].HasKid = true
		}
	}
	return t
}

// ChildrenOf 返回某个节点的直接子节点。parent 为空字符串时返回顶层节点。
func (t *Tree) ChildrenOf(parent string) []Node {
	var idxs []int
	if parent == "" {
		idxs = t.Roots
	} else {
		idxs = t.Children[parent]
	}
	out := make([]Node, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, t.Nodes[i])
	}
	return out
}

// Path 返回从根到 id 的节点链（含 id 本身）。id 不存在时返回 nil。
//
// 深链定位要用：搜索结果点进文档时前端只有节点 ID，而目录树是逐层懒加载
// 的，必须知道「要先展开哪几层」。这个方法是深链定位的服务端一半，另一半
// 是前端按这条链逐层展开。
func (t *Tree) Path(id string) []Node {
	if t == nil || id == "" {
		return nil
	}
	i, ok := t.byID[id]
	if !ok {
		return nil
	}
	return t.pathOf(i)
}

// PathByURL 按包内相对 URL 反查节点链。
//
// 正文命中的搜索结果只有 URL、没有节点 ID（正文里不含目录信息），因此需要
// 按 URL 反查。树是扁平的，线性扫一遍即可，只在深链请求时调用，不必额外
// 建索引常驻内存。
func (t *Tree) PathByURL(url string) []Node {
	if t == nil || url == "" {
		return nil
	}
	want := normalizeNodeURL(url)
	for i := range t.Nodes {
		if normalizeNodeURL(t.Nodes[i].URL) == want {
			return t.pathOf(i)
		}
	}
	return nil
}

// pathOf 把扁平下标 i 顺着 Parent 链走回根，再反转成「根 -> 目标」。
func (t *Tree) pathOf(i int) []Node {
	chain := make([]Node, 0, 8)
	for guard := 0; i >= 0 && guard < 4096; guard++ {
		n := t.Nodes[i]
		chain = append(chain, n)
		if n.Parent == "" {
			break
		}
		j, ok := t.byID[n.Parent]
		if !ok {
			break
		}
		i = j
	}
	for a, b := 0, len(chain)-1; a < b; a, b = a+1, b-1 {
		chain[a], chain[b] = chain[b], chain[a]
	}
	return chain
}

// normalizeNodeURL 只做最小的归一化：目录里的 URL 与搜索结果里的 URL 都来自
// 同一份 navi.xml / .hhc，正常情况下逐字节相同；这里兜住前导 "./" 与空白，
// 避免因为写法差异导致反查落空。
func normalizeNodeURL(u string) string {
	u = strings.TrimSpace(u)
	for strings.HasPrefix(u, "./") {
		u = u[2:]
	}
	return u
}

// ---------------------------------------------------------------- cache

// Cache 是单包目录树缓存，容量为 1，只保留最近使用的那个包。
//
// 用户同一时间只读一个库。容量 1 时内存上限就是单个最大包的树（约 43MB），
// 全量缓存要 867MB，超出 1G 预算。
type Cache struct {
	mu      sync.Mutex
	everUse bool
	libID   string
	tree    *Tree
	hits    int64
	misses  int64
}

// NewCache 创建一个容量为 1 的目录树缓存。
func NewCache() *Cache { return &Cache{} }

// Get 取某包的目录树，未命中时用 loader 解析并替换当前缓存。
func (c *Cache) Get(libID string, loader func() (*Tree, error)) (*Tree, error) {
	c.mu.Lock()
	if c.everUse && c.libID == libID && c.tree != nil {
		t := c.tree
		c.hits++
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	// 解析期间不持锁：避免阻塞其它库的读取
	tree, err := loader()
	if err != nil {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Lock()
	c.libID = libID
	c.tree = tree
	c.everUse = true
	c.misses++
	c.mu.Unlock()
	return tree, nil
}

// Drop 清空缓存（内存守卫调用）。
func (c *Cache) Drop() {
	c.mu.Lock()
	c.tree = nil
	c.libID = ""
	c.everUse = false
	c.mu.Unlock()
}

// Stats 返回缓存命中情况与当前占用节点数，供 /debug/healthz 暴露。
func (c *Cache) Stats() (libID string, nodes int, hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	if c.tree != nil {
		n = c.tree.nodeCount()
	}
	return c.libID, n, c.hits, c.misses
}

// ---------------------------------------------------------------- parse

// Parse 解析一个文档包的目录树。
//
// 优先 navi.xml，回退 .hhc，最后用 FileList.xml 平铺。必须传入已打开的包 reader。
func Parse(r *doclib.Reader) (*Tree, error) {
	nodes, err := parseNaviXML(r)
	if err == nil && len(nodes) > 0 {
		return build(nodes), nil
	}
	nodes, err = parseHHC(r)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		// 最后兜底：用 FileList.xml 平铺
		nodes = parseFileList(r)
	}
	return build(nodes), nil
}

// parseNaviXML 流式解析 resources/navi.xml。
//
// 结构：<topics><topic txt url id target><topic .../></topic></topics>
// 用 xml.Decoder 逐个 token 处理，避免把 8MB 的 XML 建成 DOM。
func parseNaviXML(r *doclib.Reader) ([]Node, error) {
	raw, err := r.ReadAllBounded("resources/navi.xml", maxMetaBytes)
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false

	var (
		nodes []Node
		stack []string // 祖先链，栈顶是当前父节点 ID
		seq   int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "topic" {
				continue
			}
			var txt, url, id string
			for _, a := range t.Attr {
				switch a.Name.Local {
				case "txt":
					txt = a.Value
				case "url":
					url = a.Value
				case "id":
					id = a.Value
				}
			}
			if id == "" {
				if url != "" {
					id = url
				} else {
					seq++
					id = "auto-" + itoa(seq)
				}
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			nodes = append(nodes, Node{ID: id, Parent: parent, Title: txt, URL: url})
			stack = append(stack, id)
		case xml.EndElement:
			if t.Name.Local == "topic" && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return nodes, nil
}

// parseHHC 解析 CHM sitemap（resources/*.hhc），回退用。
//
// 结构近似：
//
//	<UL>
//	  <LI><OBJECT type="text/sitemap">
//	        <param name="Name" value="标题">
//	        <param name="Local" value="xx/yy.html">
//	      </OBJECT>
//	    <UL> ...子节点... </UL>
//	  </LI>
//	</UL>
//
// 文件声明 gb2312，但实际字节是 GBK，必须转码，否则标题乱码。
func parseHHC(r *doclib.Reader) ([]Node, error) {
	var hhcName string
	for _, f := range r.Files() {
		if strings.HasSuffix(strings.ToLower(f.Name), ".hhc") {
			hhcName = f.Name
			break
		}
	}
	if hhcName == "" {
		return nil, doclib.ErrNotFound
	}
	raw, err := r.ReadAllBounded(hhcName, maxMetaBytes)
	if err != nil {
		return nil, err
	}
	// GBK -> UTF-8
	decoded, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw)
	if err != nil {
		decoded = raw // 转码失败时退回原字节，至少不丢结构
	}
	text := string(decoded)

	// .hhc 是 HTML 片段，用宽松的 tokenizer 手工扫描即可
	return parseHHCScan(text), nil
}

// parseFileList 兜底：FileList.xml 是平铺列表，没有层级。
func parseFileList(r *doclib.Reader) []Node {
	for _, f := range r.Files() {
		if !strings.HasSuffix(strings.ToLower(f.Name), "filelist.xml") {
			continue
		}
		raw, err := r.ReadAllBounded(f.Name, maxMetaBytes)
		if err != nil {
			return nil
		}
		dec := xml.NewDecoder(bytes.NewReader(raw))
		dec.Strict = false
		var nodes []Node
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "file" {
				var url string
				for _, a := range se.Attr {
					if a.Name.Local == "url" {
						url = a.Value
					}
				}
				if url == "" {
					continue
				}
				title := url
				if i := strings.LastIndex(url, "/"); i >= 0 {
					title = url[i+1:]
				}
				nodes = append(nodes, Node{ID: url, Title: title, URL: url})
			}
		}
		return nodes
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
