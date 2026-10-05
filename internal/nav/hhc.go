package nav

import (
	"strings"

	"web_ics/internal/textfold"
)

// parseHHCScan 手工扫描 .hhc（CHM sitemap）文本，构建带层级的节点列表。
//
// .hhc 的结构（<LI> 与 </LI> 严格配对）：
//
//	<UL>
//	  <LI><OBJECT ...><param name="Name" ...><param name="Local" ...></OBJECT>
//	      <UL>                        ← 有子节点时，<UL> 在 <LI> 内部
//	        <LI><OBJECT ...></OBJECT></LI>   ← 叶子：直接 </LI> 收尾
//	      </UL>
//	  </LI>                           ← 父节点由这个 </LI> 收尾
//	</UL>
//
// 所以栈的推入在遇到 sitemap 节点时、弹出在遇到 </LI> 时。不能用 </UL>
// 收尾：叶子节点的 </LI> 和父节点的 </UL> 数量不对等（全库样本里 55664 个
// <LI> 只有 6724 个 <UL>），误用 </UL> 会让整棵树塌成单根。
//
// 性能上注意别在大文件上反复对整个剩余串做 ToLower，那是 O(n²)，10MB 的
// .hhc 会跑到分钟级。这里只做一次小写副本，之后一律基于游标做 Index。
func parseHHCScan(text string) []Node {
	var (
		nodes []Node
		stack []string // 祖先 ID 栈；栈顶是当前 <UL> 的父节点
		seq   int
	)

	// 用 ASCIILower 而不是 strings.ToLower：后者遇到非法 UTF-8 字节会变长，
	// 下面用 lower 的下标去切 text 就会越界（见 textfold 包注释）。
	lower := textfold.ASCIILower(text)
	i := 0
	for i < len(text) {
		objIdx := strings.Index(lower[i:], "<object")
		liEndIdx := strings.Index(lower[i:], "</li")
		ulIdx := strings.Index(lower[i:], "<ul")

		next, kind := -1, ""
		for _, c := range []struct {
			off  int
			kind string
		}{{objIdx, "obj"}, {liEndIdx, "liend"}, {ulIdx, "ul"}} {
			if c.off >= 0 && (next < 0 || c.off < next) {
				next, kind = c.off, c.kind
			}
		}
		if next < 0 {
			break
		}
		abs := i + next

		switch kind {
		case "obj":
			end := strings.Index(lower[abs:], "</object")
			var block string
			if end < 0 {
				block = text[abs:]
				i = len(text)
			} else {
				block = text[abs : abs+end]
				i = abs + end + len("</object")
			}
			name := paramValue(block, "Name")
			local := paramValue(block, "Local")
			if name == "" && local == "" {
				continue // 站点属性块，跳过
			}
			seq++
			id := local
			if id == "" {
				id = "hhc-" + itoa(seq)
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			nodes = append(nodes, Node{ID: id, Parent: parent, Title: name, URL: local})
			// 该节点入栈：随后的 <UL> 里的节点以它为父
			stack = append(stack, id)
		case "ul":
			// 进入子列表，无需动作（父节点已在栈顶）
			i = abs + len("<ul")
		case "liend":
			// 当前节点的作用域结束
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i = abs + len("</li")
		}
	}
	return nodes
}

// paramValue 从 <param name="X" value="Y"> 中取出 Y。
func paramValue(block, key string) string {
	lower := textfold.ASCIILower(block)
	needle := `name="` + textfold.ASCIILower(key) + `"`
	idx := strings.Index(lower, needle)
	if idx < 0 {
		needle = "name=" + textfold.ASCIILower(key)
		idx = strings.Index(lower, needle)
		if idx < 0 {
			return ""
		}
	}
	rest := block[idx:]
	vidx := strings.Index(textfold.ASCIILower(rest), "value")
	if vidx < 0 {
		return ""
	}
	rest = rest[vidx+len("value"):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	rest = strings.TrimPrefix(rest, "=")
	rest = strings.TrimLeft(rest, " \t\r\n")
	if rest == "" {
		return ""
	}
	if rest[0] == '"' {
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			return unescapeHTML(rest[1 : 1+end])
		}
		return unescapeHTML(rest[1:])
	}
	end := strings.IndexAny(rest, " \t\r\n>")
	if end < 0 {
		return unescapeHTML(rest)
	}
	return unescapeHTML(rest[:end])
}

// unescapeHTML 处理标题里的常见实体。
func unescapeHTML(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&nbsp;", " ",
		"&#39;", "'",
	)
	return r.Replace(s)
}
