// Package htmltext 把文档包的正文 HTML 抽成纯文本。
//
// 单独一个包是因为正文检索（internal/search）和语料库（internal/corpus）
// 都要用同一套抽取逻辑，两边各写一份必然漂移。抽取里最关键的一步是
// GBK 前置解码，一旦漏掉，中文就永远搜不到。抽成独立包两边 import 它，
// 也就不会出现循环依赖。
package htmltext

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"web_ics/internal/textfold"
)

// ExtractRevision 是抽取逻辑的版本号。
//
// 任何会改变抽取输出的改动（改标签处理、改实体还原、改噪声过滤等）都必须
// 把它加一。离线语料库判断「新不新」只有两个依据：包文件的 mtime 和分片的
// 格式版本。改了抽取逻辑却不改这两个，已建好的语料会被判定为就绪而不重建，
// 症状是「明明改了代码、结果一点没变」，而且不报任何错。
const ExtractRevision = 2

// ExtractText 把 HTML 剥成纯文本（UTF-8）。
//
// 正文的字节编码是 GBK（<meta charset=gb2312>），而检索词是 UTF-8。直接在
// 原始字节里找 UTF-8 的「接口」永远找不到，必须先把 GBK 解码成 UTF-8 再匹配。
//
// 处理范围（够用即可，不做完整 HTML 解析）：
//
//	去掉 <script>...</script>、<style>...</style>、各类 <...> 标签，
//	把 &nbsp; &lt; &gt; &amp; &quot; 等实体还原，
//	折叠连续空白。
func ExtractText(raw []byte) string {
	decoded := DecodeToUTF8(raw)
	s := string(decoded)
	var b strings.Builder
	b.Grow(len(s) / 2)

	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		if c == '<' {
			// 跳过 script/style 整体：它们的代码里含大量符号，且不是正文
			if hasTagFold(s, i, "script") {
				i = skipTo(s, i, "</script>")
				continue
			}
			if hasTagFold(s, i, "style") {
				i = skipTo(s, i, "</style>")
				continue
			}
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				break
			}
			// 块级标签换成空格，避免 "端口地址" 这类粘连
			b.WriteByte(' ')
			i += j + 1
			continue
		}
		if c == '&' {
			if semi := strings.IndexByte(s[i:min(i+12, n)], ';'); semi > 0 {
				ent := s[i : i+semi+1]
				b.WriteString(unescapeEntity(ent))
				i += semi + 1
				continue
			}
		}
		if c == '\n' || c == '\r' || c == '\t' {
			b.WriteByte(' ')
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	// 文档模板的顶部导航条渲染出来就是一行「< Home」（模板里是
	// `&lt;</span><a> Home </a>`），它会被当成正文开头混进摘要，
	// 让每条结果都以「< Home」开头。顺带消掉「搜 Home 命中所有文档」
	// 这种假命中。
	out = strings.ReplaceAll(out, "< Home ", " ")
	return strings.TrimPrefix(out, "< Home")
}

// ExtractTitle 取正文标题。
//
// 优先 <title>，其次 class="topicTitle-h1" 的 h1。同样必须先做 GBK 到
// UTF-8 的转码，否则标题里的中文是乱码。
func ExtractTitle(raw []byte) string {
	s := string(DecodeToUTF8(raw))
	if t := betweenTag(s, "<title>", "</title>"); t != "" {
		return t
	}
	if t := betweenAttrText(s, "topicTitle-h1"); t != "" {
		return t
	}
	return ""
}

// IsNoisePath 判断是否是无需检索的导航类页面。入参应为小写的包内路径。
func IsNoisePath(lower string) bool {
	switch {
	case strings.Contains(lower, "/index.html"),
		strings.Contains(lower, "hedex-homepage"),
		strings.Contains(lower, "copyright"),
		strings.Contains(lower, "glossary"),
		strings.Contains(lower, "search.html"):
		return true
	}
	return false
}

// DecodeToUTF8 把文档字节解码成 UTF-8。
//
// 先判是否已是合法 UTF-8；不是才走 GBK 转码，转码失败退回原字节
// （部分包本来就是 UTF-8）。
func DecodeToUTF8(raw []byte) []byte {
	if utf8.Valid(raw) {
		return raw
	}
	out, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw)
	if err != nil || len(out) == 0 {
		return raw
	}
	return out
}

// betweenTag 取 open 与 close 之间的文本（忽略大小写），入参须已是 UTF-8。
func betweenTag(s, open, close string) string {
	i := indexFold(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	j := indexFold(s, close)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

// betweenAttrText 取含 marker 的那个标签的文本内容，入参须已是 UTF-8。
func betweenAttrText(s, marker string) string {
	i := indexFold(s, marker)
	if i < 0 {
		return ""
	}
	s = s[i:]
	gt := strings.Index(s, ">")
	if gt < 0 {
		return ""
	}
	s = s[gt+1:]
	lt := strings.Index(s, "<")
	if lt < 0 {
		return ""
	}
	return strings.TrimSpace(s[:lt])
}

func indexFold(s, sub string) int {
	// 返回值必须对原串 s 同样有效：调用方会拿它去切 s。
	// 所以不能用 strings.ToLower，非法 UTF-8 字节会让它变长。
	return textfold.IndexFold(s, sub)
}

// hasTagFold 判断从 i 开始是否是一个指定名称的开始标签。
func hasTagFold(s string, i int, name string) bool {
	if i+1+len(name) > len(s) {
		return false
	}
	if !strings.EqualFold(s[i+1:i+1+len(name)], name) {
		return false
	}
	next := i + 1 + len(name)
	return next < len(s) && (s[next] == '>' || s[next] == ' ' || s[next] == '\t' || s[next] == '\n')
}

// skipTo 返回 close 之后的位置；找不到则返回 len(s)。
func skipTo(s string, i int, close string) int {
	j := strings.Index(textfold.ASCIILower(s[i:]), close)
	if j < 0 {
		return len(s)
	}
	return i + j + len(close)
}

func unescapeEntity(e string) string {
	switch e {
	case "&nbsp;":
		return " "
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	case "&amp;":
		return "&"
	case "&quot;":
		return `"`
	case "&#39;", "&apos;":
		return "'"
	case "&copy;":
		return "(c)"
	}
	if n := len(e); n > 3 && e[1] == '#' {
		// 数字实体，仅处理 BMP 内常见值
		v := 0
		for _, ch := range e[2 : n-1] {
			if ch < '0' || ch > '9' {
				return e
			}
			v = v*10 + int(ch-'0')
			if v > 0xFFFF {
				return e
			}
		}
		if v > 0 {
			return string(rune(v))
		}
	}
	return e
}
