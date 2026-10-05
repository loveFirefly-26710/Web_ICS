package doclib

import (
	"regexp"
	"sort"
	"strings"
)

// Category 是文档库的产品领域分类，对应用户在首页左侧看到的导航项。
//
// 不直接用 profile.xml 的 productType：它的粒度太细（一台设备一个产品名，
// 59 个包能出 40 多种），而用户实际是按「无线 / 数据通信 / 计算」这种
// 产品领域找文档的。
type Category struct {
	Key   string `json:"key"`   // 稳定的英文键，用于前端 URL 与筛选
	Name  string `json:"name"`  // 展示名
	Order int    `json:"order"` // 显示顺序
	Count int    `json:"count"` // 库数量，由 Index 填充
}

// categoryRules 是分类规则表。
//
// 顺序敏感：按切片顺序匹配，先中者胜，所以更具体的规则必须排在前面。
// 例：AR 路由器既含 "AR" 又像无线设备，必须由「数据通信」先命中；
// SD-WAN 属于解决方案而非具体产品，排在数据通信之前。
var categoryRules = []struct {
	Key   string
	Name  string
	Order int
	Pat   *regexp.Regexp
}{
	{"wireless", "无线", 10, regexp.MustCompile(`(?i)(FAT AP|云AP|无线接入控制器|eNodeB|天线|WLAN)`)},
	{"dc-solution", "数据通信解决方案", 20, regexp.MustCompile(`(?i)(SD-WAN|敏捷园区|智简园区|云园区)`)},
	{"datacom", "数据通信", 30, regexp.MustCompile(
		`(?i)(^AR\d|^AR路由器|^AR100|^AR120|^AR500|^NetEngine|^HUAWEI NE|^NE\d+` +
			`|CloudEngine|^S\d|^HUAWEI USG|^USG|^HiSecEngine|^FutureMatrix|^IP_V` +
			`|HedEx|华为S系列|S\d{4})`)},
	{"network-mgmt", "网络管理", 40, regexp.MustCompile(`(?i)(iMaster|^NCE|eSight|Agile Controller)`)},
	{"storage", "数据存储", 50, regexp.MustCompile(`(?i)(FusionStorage|OceanStor|存储)`)},
	{"computing", "计算", 60, regexp.MustCompile(`(?i)(FusionSphere|FusionAccess|Taishan|桌面云|虚拟化)`)},
	{"cloud", "华为云", 70, regexp.MustCompile(`(?i)(华为云Stack|Huawei Cloud|ManageOne)`)},
	{"optical-transport", "光传送", 80, regexp.MustCompile(`(?i)(OSN|OptiX|光传送|WDM)`)},
	{"optical-access", "光接入", 90, regexp.MustCompile(`(?i)(光接入|OLT|MA5)`)},
	{"digital-power", "数字能源", 100, regexp.MustCompile(`(?i)(数字能源|UPS|电源)`)},
}

// fallbackCategory 未命中任何规则时的兜底分类。
var fallbackCategory = Category{Key: "other", Name: "其他", Order: 999}

// CategoryOf 根据库元数据判定其产品领域。
//
// 判定顺序：LibName 优先，其次 FileName。不匹配 productType：部分包的
// productType 是版本串或空值。
func CategoryOf(m LibMeta) Category {
	for _, r := range categoryRules {
		if r.Pat.MatchString(m.LibName) {
			return Category{Key: r.Key, Name: r.Name, Order: r.Order}
		}
	}
	for _, r := range categoryRules {
		if r.Pat.MatchString(m.FileName) {
			return Category{Key: r.Key, Name: r.Name, Order: r.Order}
		}
	}
	return fallbackCategory
}

// Categories 返回当前索引里实际出现过的分类，按 Order 排序，并填好 Count。
//
// 只返回非空分类：界面上不该出现一个点进去什么都没有的导航项。
func (ix *Index) Categories() []Category {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	counts := make(map[string]*Category)
	for _, l := range ix.libs {
		c := CategoryOf(l)
		if got, ok := counts[c.Key]; ok {
			got.Count++
			continue
		}
		cc := c
		cc.Count = 1
		counts[c.Key] = &cc
	}

	out := make([]Category, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return strings.Compare(out[i].Key, out[j].Key) < 0
	})
	return out
}
