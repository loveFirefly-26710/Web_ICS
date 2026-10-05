package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipLimiter 是「每 IP 一个令牌桶」的轻量限流。
//
// 全局信号量是所有访客共享的十个槽位，单个 IP 发十个 /api/grep 就能让
// 其他人全部拿到 503，而一次无匹配的全库 grep 要数秒，这个窗口足够大。
//
// 默认开着是因为项目默认的部署形态就是裸监听 8080，没有反代兜底。
// 配了反代之后可以在反代层做更省资源的限流，那时把 --ip-rate 设 0 关掉即可。
type ipLimiter struct {
	rate    float64
	burst   float64
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	swept   time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newIPLimiter 构造限流器。rate<=0 或 burst<=0 时返回 nil，表示不限流。
func newIPLimiter(rate float64, burst int) *ipLimiter {
	if rate <= 0 || burst <= 0 {
		return nil
	}
	return &ipLimiter{
		rate:    rate,
		burst:   float64(burst),
		buckets: make(map[string]*tokenBucket),
		swept:   time.Now(),
	}
}

// allow 判断该 IP 现在能不能发一个请求。limiter 为 nil 时永远放行。
func (l *ipLimiter) allow(ip string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// 定期扫掉闲置的桶，否则被大量不同源 IP 打的时候这张 map 会一直涨。
	if now.Sub(l.swept) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}

	b := l.buckets[ip]
	if b == nil {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	// 按经过的时间补令牌，上限是桶容量
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// isExpensivePath 判断请求是否值得限流。
//
// 只盯两个重操作：/api/grep 一次无匹配的全库扫描要数秒，/api/search 的
// 正文段要扫全库语料。单个 IP 连发十来个就能占满全局那十个槽位。
//
// 不限 /api/nav 与 /static：它们便宜（目录树有缓存、静态资源就是读文件），
// 而且深链展开一棵几十层的目录树本来就要连发几十个 /api/nav。
// 给所有路径统一限流会把正常浏览也挡掉，阅读页的搜索会直接失败。
func isExpensivePath(p string) bool {
	return p == "/api/search" || p == "/api/grep"
}

// withRateLimit 按客户端 IP 限流重操作。未配置限流时直接放行。
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	if s.ipLim == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isExpensivePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.ipLim.allow(clientIP(r, s.cfg.TrustProxy), time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "检索请求过于频繁，请稍后重试", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP 取客户端 IP。
//
// 默认只信 RemoteAddr。只有显式开了 TrustProxy 才认 X-Forwarded-For，
// 否则任何人都能自己塞这个头，限流等于没有。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
