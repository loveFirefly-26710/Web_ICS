// Package doclib 负责发现并索引文档库目录下的 .hdx / .hwics 包。
//
// 设计约束（1G 内存上限）：
//   - 启动时只读每个包的 profile.xml（几百字节到 10KB），不解析 navi.xml
//   - 不缓存包内容，每次按需打开 zip
package doclib

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LibMeta 是文档库的元数据，来源于包内 profile.xml。
// 字段覆盖不完整时（部分 hwics 没有 profile.xml）回退到文件名推断。
type LibMeta struct {
	LibID          string `json:"libId"`          // 库标识，优先取 GUID，否则用文件名
	LibName        string `json:"libName"`        // 展示名
	ProductType    string `json:"productType"`    // 产品类型
	ProductVersion string `json:"productVersion"` // 版本
	LibVersion     string `json:"libVersion"`     // 文档版本序号（profile.xml 的 libVersion，如 "11"）
	Language       string `json:"language"`
	IssueDate      string `json:"issueDate"`
	TopicNumber    int    `json:"topicNumber"`
	Guid           string `json:"guid"`
	FilePath       string `json:"-"` // 包在磁盘上的绝对路径
	FileName       string `json:"fileName"`
	SizeBytes      int64  `json:"sizeBytes"`
	HasNaviXML     bool   `json:"hasNaviXml"` // 是否可用 navi.xml（否则用 .hhc）
}

// profileXML 同时声明两套标签名，因为文档包有两种布局。
//
// 一种是包根目录的 profile.xml，驼峰标签（libId / libVersion / libName /
// productType / productVersion / issueDate）。.hdx 与一部分 .hwics 用这套。
//
// 另一种只有 resources/infocenter_service/profile.xml，标签全小写而且名字不同
// （libid / resourcelibversion / resourcelibname / productname / productversion /
// issuedate）。HedEx 2.0 导出的 .hwics 用这套，而且 resourcelibversion 常常是空的。
//
// 两套都列出来，encoding/xml 按标签名匹配，互不干扰。取值时驼峰优先、小写兜底。
// language 与 guid 两套同名同大小写，一个字段就够。
type profileXML struct {
	LibID          string `xml:"libId"`
	LibName        string `xml:"libName"`
	ProductType    string `xml:"productType"`
	ProductVersion string `xml:"productVersion"`
	LibVersion     string `xml:"libVersion"`
	IssueDate      string `xml:"issueDate"`
	TopicNumber    int    `xml:"topicNumber"`

	// 全小写那套。resourcelibname 故意不取：它比文件名还差
	// （例如 "V800R021C00 产品文档_new"，丢了产品名、还带个 _new 后缀），
	// 那种包用文件名当展示名更好。
	LibIDLower          string `xml:"libid"`
	ProductTypeLower    string `xml:"productname"`
	ProductVersionLower string `xml:"productversion"`
	LibVersionLower     string `xml:"resourcelibversion"`
	IssueDateLower      string `xml:"issuedate"`

	Language string `xml:"language"`
	Guid     string `xml:"guid"`
}

// pick 取第一个非空值。
func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Index 是文档库索引。只保存元数据，不保存任何内容。
type Index struct {
	mu   sync.RWMutex
	root string
	libs []LibMeta
	byID map[string]*LibMeta
}

// Empty 返回一个不含任何文档包的索引。
//
// 给「文档库目录不存在或读不了」用：那种情况下起服务并如实报告 0 个包，
// 比直接退出好。双击启动的场景下退出会让控制台一闪而过，用户看不到原因。
func Empty(root string) *Index {
	return &Index{root: root, byID: make(map[string]*LibMeta)}
}

// Scan 扫描 root 下的所有 .hdx / .hwics 包并建立索引。
//
// 只扫描直接子文件，不递归。内存上每包约 1KB 元数据，59 个包总计不到 100KB。
func Scan(root string) (*Index, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取文档库目录失败: %w", err)
	}

	idx := &Index{root: root, byID: make(map[string]*LibMeta)}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".hdx" && ext != ".hwics" {
			continue
		}
		abs := filepath.Join(root, e.Name())
		meta, err := readMeta(abs, e.Name())
		if err != nil {
			// 单包元数据读取失败不应中断整个索引：记录一个仅含文件名的条目
			meta = &LibMeta{
				LibID:    libIDFromName(e.Name()),
				LibName:  strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
				FilePath: abs,
				FileName: e.Name(),
			}
		}
		if fi, err := os.Stat(abs); err == nil {
			meta.SizeBytes = fi.Size()
		}
		idx.libs = append(idx.libs, *meta)
	}

	sort.Slice(idx.libs, func(i, j int) bool {
		return idx.libs[i].LibName < idx.libs[j].LibName
	})
	for i := range idx.libs {
		idx.byID[idx.libs[i].LibID] = &idx.libs[i]
	}
	return idx, nil
}

// 包内已知路径。
const (
	profileAtRoot  = "profile.xml"
	profileAtHedex = "resources/infocenter_service/profile.xml"
	naviPath       = "resources/navi.xml"
)

// readMeta 打开 zip 只读 profile.xml 与探测 navi.xml 是否存在。
func readMeta(path, fileName string) (*LibMeta, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	meta := &LibMeta{
		FilePath: path,
		FileName: fileName,
		LibName:  strings.TrimSuffix(fileName, filepath.Ext(fileName)),
	}

	// 一个包里可能有多份 profile.xml（书签映射目录里也有同名的），所以先按
	// 「包根 > HedEx 2.0 位置 > 其它（路径短的优先）」排序，再取第一份能解析
	// 出内容的。只用第一份，避免把嵌套的那份的字段混进来。
	var profiles []*zip.File
	for _, f := range zr.File {
		switch {
		case strings.EqualFold(filepath.Base(f.Name), "profile.xml"):
			profiles = append(profiles, f)
		case f.Name == naviPath:
			meta.HasNaviXML = true
		}
	}
	sort.SliceStable(profiles, func(i, j int) bool {
		ri, rj := profileRank(profiles[i].Name), profileRank(profiles[j].Name)
		if ri != rj {
			return ri < rj
		}
		return len(profiles[i].Name) < len(profiles[j].Name)
	})

	for _, f := range profiles {
		data, err := readProfile(f)
		if err != nil {
			continue
		}
		var p profileXML
		if xml.Unmarshal(data, &p) != nil {
			continue
		}
		if p.LibID == "" && p.LibIDLower == "" && p.LibName == "" &&
			p.ProductVersion == "" && p.ProductVersionLower == "" {
			continue // 这份不是我们要的
		}
		meta.LibID = pick(p.LibID, p.LibIDLower)
		meta.LibName = p.LibName // 小写那套的 resourcelibname 不如文件名，不取
		meta.ProductType = pick(p.ProductType, p.ProductTypeLower)
		meta.ProductVersion = pick(p.ProductVersion, p.ProductVersionLower)
		meta.LibVersion = pick(p.LibVersion, p.LibVersionLower)
		// .hdx 的 issueDate 是纯日期（2024-11-10），.hwics 的 issuedate 带时间
		// （2022-04-13 03:05:52）。界面上那一列叫「日期」，统一只留日期部分。
		meta.IssueDate = firstField(pick(p.IssueDate, p.IssueDateLower))
		meta.Language = p.Language
		meta.TopicNumber = p.TopicNumber
		meta.Guid = p.Guid
		break
	}

	if meta.LibID == "" {
		meta.LibID = libIDFromName(fileName)
	}
	if meta.LibName == "" {
		meta.LibName = strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	return meta, nil
}

// firstField 取第一个空白之前的部分。
func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// profileRank 越小越优先。
func profileRank(name string) int {
	switch name {
	case profileAtRoot:
		return 0
	case profileAtHedex:
		return 1
	}
	return 2
}

// readProfile 读一份 profile.xml。它很小（几百字节到 10KB），一次性读完是安全的。
func readProfile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 1<<20))
}

// libIDFromName 在没有 GUID/profile.xml 时，从文件名推导一个稳定的 ID。
func libIDFromName(fileName string) string {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	// 只保留字母数字与 CJK，其余折叠为 '-'，避免 URL 里出现奇怪字符
	var b strings.Builder
	prevDash := false
	for _, r := range base {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r > 0x7f
		if ok {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// List 返回全部库的元数据副本。
func (ix *Index) List() []LibMeta {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]LibMeta, len(ix.libs))
	copy(out, ix.libs)
	return out
}

// Get 按 libID 取库元数据，第二个返回值表示是否存在。
func (ix *Index) Get(libID string) (*LibMeta, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	m, ok := ix.byID[libID]
	if !ok {
		return nil, false
	}
	cp := *m
	return &cp, true
}
