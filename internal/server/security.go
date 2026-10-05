package server

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------- 安全响应头

// 两套 CSP 策略。
//
// 正文路由必须放宽：文档包里的正文 HTML 普遍自带内联 <script>（标签页切换
// 这类原生交互），严格策略会让这些交互失效。
//
// nosniff 与 X-Frame-Options 对正文同样有效，不需要放宽：前者防 MIME 嗅探
// （正文的 Content-Type 故意不带 charset），后者防点击劫持。
//
// frame-ancestors 单独写：它不参与 default-src 的回退，漏写就等于没防。
// object-src 'none' 单独写：主站不加载任何插件内容，关掉它省一类攻击面。
const (
	cspStrict = "default-src 'self'; object-src 'none'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'"
	cspDoc    = "default-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob:; frame-ancestors 'self'"
)

// withSecurityHeaders 给所有响应加安全头。
//
// 放在最外层，这样限流的 429、闸门的 503、方法拒绝的 405 也带上头。
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "no-referrer")
		if isDocResource(r.URL.Path) {
			h.Set("Content-Security-Policy", cspDoc)
		} else {
			h.Set("Content-Security-Policy", cspStrict)
		}
		next.ServeHTTP(w, r)
	})
}

// withMethodGuard 只放行 GET 与 HEAD。
//
// 服务是只读的，没有任何写接口，其余方法都没有意义。显式拒绝而不是静默
// 按 GET 处理，可以避免请求体被中间层当成有效载荷缓存或转发。
func withMethodGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		}
	})
}

// isDocResource 判断请求是不是文档包内的资源（/doc/{libId}/res/...）。
func isDocResource(p string) bool {
	return strings.HasPrefix(p, "/doc/") && strings.Contains(p, "/res/")
}

// ---------------------------------------------------------------- 参数边界

const (
	// maxQueryRunes 是检索词的长度上限。按 rune 计，中英文体感一致。
	//
	// 没有上限时，超长查询会先撞上 MaxHeaderBytes（256KB）变成 431，
	// 那个报错对调用方毫无信息量。256 个字符足够表达任何真实检索需求。
	maxQueryRunes = 256

	// maxPageIndex / maxSkipHits 是分页参数上限。
	//
	// 超大偏移本身没有资源代价（page 只是拿去算切片位置，越界得到空结果），
	// 加上限是为了错误语义清晰，不是为了防 DoS。
	maxPageIndex = 10000
	maxSkipHits  = 100000
)

func queryTooLong(q string) bool {
	return utf8.RuneCountInString(q) > maxQueryRunes
}

// ---------------------------------------------------------------- 文案清理

// sanitizeMsg 去掉控制字符。
//
// 错误文案里会带上用户可控的内容（比如请求路径）。换行能借此进入 JSON
// 响应与日志，在日志里伪造出完整的一行假记录。
func sanitizeMsg(s string) string {
	if !strings.ContainsFunc(s, isControlRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isControlRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f
}
