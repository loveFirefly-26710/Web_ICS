// Package textfold 提供「字节长度严格不变」的大小写折叠。
//
// 为什么不用 strings.ToLower：它遇到非法 UTF-8 字节时，会把每个非法字节
// 替换成 U+FFFD（EF BF BD，3 字节），于是输出比输入长。代码里常见的写法是
//
//	lower := strings.ToLower(s)
//	idx := strings.Index(lower, needle)
//	rest := s[idx:]          // 越界
//
// 「在折叠串上算出来的下标」拿去切原串就会 slice bounds out of range。
// 本仓库原先有四处这样的写法（nav/hhc.go 的 paramValue、htmltext/extract.go
// 的 betweenTag 与 skipTo、search/grep.go 的 snippets），都已改用本包。
//
// 这里只折叠 ASCII 字母，返回值与输入的字节长度严格相等，下标两边通用。
// 代价是非 ASCII 的大小写折叠没有了，而折叠本来只用于匹配 ASCII 的标签名、
// 参数名、字段名与查询词，中文不受影响。
package textfold

import "strings"

// ASCIILower 返回 s 的副本，只把 A-Z 转成 a-z。
//
// 返回值的字节长度与输入严格相等。没有大写字母时直接返回原串，不做分配。
func ASCIILower(s string) string {
	up := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			up = true
			break
		}
	}
	if !up {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// IndexFold 返回 sub 在 s 中首次出现的位置（只折叠 ASCII），没有则返回 -1。
//
// 与 strings.Index(strings.ToLower(s), sub) 的区别：返回的下标对 s 本身同样有效。
func IndexFold(s, sub string) int {
	return strings.Index(ASCIILower(s), ASCIILower(sub))
}
