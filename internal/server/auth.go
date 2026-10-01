package server

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// ClientAuth 是客户端证书认证的配置与运行时状态。
//
// 配了它就要求客户端证书，不配就是原来的裸 HTTP。零值不可用，
// 用 LoadClientAuth 或 NewClientAuth 构造。设计见 PLAN 的
// 「客户端证书认证设计」一节。
type ClientAuth struct {
	caPool *x509.CertPool

	// denied 是吊销名单：证书 DER 的 SHA-256（小写十六进制）到备注。
	// 标准库的 TLS 栈不做 CRL 与 OCSP 检查，吊销只能自己做。
	denied map[string]string
}

// NewClientAuth 从内存里的 PEM 与名单文本构造，供自检与单元测试使用。
//
// caPEM 为空时不建立信任锚：闸门仍然按「有没有证书」判断，但真实握手一定
// 失败。生产路径走 LoadClientAuth。
func NewClientAuth(caPEM []byte, denyList string) (*ClientAuth, error) {
	pool := x509.NewCertPool()
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA 证书里没有可用的 PEM 内容")
	}
	a := &ClientAuth{caPool: pool, denied: make(map[string]string)}
	if err := a.parseDenyList(denyList, "吊销名单"); err != nil {
		return nil, err
	}
	return a, nil
}

// LoadClientAuth 读 CA 证书与可选的吊销名单。denyFile 为空表示不启用名单。
//
// 名单文件写了但读不到、或者有一行不合法，都直接报错而不是跳过：一份读不进来
// 的吊销名单比没有名单更危险，因为运维会以为它生效了。
func LoadClientAuth(caFile, denyFile string) (*ClientAuth, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("读客户端 CA 证书失败: %w", err)
	}
	deny := ""
	if denyFile != "" {
		b, err := os.ReadFile(denyFile)
		if err != nil {
			return nil, fmt.Errorf("读吊销名单失败: %w", err)
		}
		deny = string(b)
	}
	a, err := NewClientAuth(caPEM, deny)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", caFile, err)
	}
	return a, nil
}

// parseDenyList 解析吊销名单。
//
// 一行一条，格式是 sha256:<64 位十六进制>，后面可以跟一段备注（只进日志）。
// 空行与 # 开头的行跳过。sha256: 前缀可省略。
func (a *ClientAuth) parseDenyList(text, label string) error {
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		fp := strings.ToLower(strings.TrimPrefix(fields[0], "sha256:"))
		if len(fp) != 64 {
			return fmt.Errorf("%s 第 %d 行不是 sha256 指纹: %s", label, i+1, fields[0])
		}
		if _, err := hex.DecodeString(fp); err != nil {
			return fmt.Errorf("%s 第 %d 行不是十六进制: %s", label, i+1, fields[0])
		}
		note := ""
		if len(fields) > 1 {
			note = strings.Join(fields[1:], " ")
		}
		a.denied[fp] = note
	}
	return nil
}

// TLSConfig 组装服务器侧的 TLS 配置。
//
// ClientAuth 取 VerifyClientCertIfGiven，不取 RequireAndVerifyClientCert。
// 标准库文档写的是两者都要求「发过来的证书必须有效」，区别只在没发证书时：
// 取前者，没带证书的请求能走到 HTTP 层，拿到一个说明页；取后者握手直接失败，
// 浏览器只画它自己的错误页，我们连一句解释都插不进去。而「还没装证书」正是
// 最常见的失败。没带证书的拦截在 withClientCertAuth 里做。
func (a *ClientAuth) TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("读服务器证书或私钥失败: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    a.caPool,
		// 显式写出来，不留标准库默认值。1.2 是当前浏览器与服务端的公共底线。
		MinVersion: tls.VersionTLS12,
	}, nil
}

// withClientCertAuth 是 HTTP 层的客户端证书闸门。
//
// TLS 层已经校验过一遍（VerifyClientCertIfGiven 下客户端发来的证书必须有效，
// 否则握手就断了）。这里再查不是重复劳动：
//   - 客户端没发证书时握手是成功的，必须在这一层拦住；
//   - VerifiedChains 为空说明这条链没验过，宁可拒。
//
// 挂在安全头里面、方法判断与限流外面：401 要带安全头，未认证的请求也不该
// 消耗并发槽位与限流配额。
func (a *ClientAuth) withClientCertAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaf, ok := peerLeaf(r)
		if !ok {
			writeClientCertPage(w, http.StatusUnauthorized, "需要客户端证书",
				"这台服务只对持有客户端证书的浏览器开放。把签发给你的证书装进系统证书库，"+
					"再刷新这个页面。装法见仓库 README 的「开启客户端证书认证」一节。")
			return
		}
		if !hasClientAuthEKU(leaf) {
			log.Printf("[auth] 拒绝用途不符的客户端证书 subject=%q", sanitizeMsg(leaf.Subject.String()))
			writeClientCertPage(w, http.StatusForbidden, "证书用途不符",
				"这张证书不是客户端证书，缺少 clientAuth 用途，不能用来访问本服务。"+
					"请重新签发一张带 clientAuth 的证书。")
			return
		}
		if note, denied := a.deniedFingerprint(leaf); denied {
			log.Printf("[auth] 拒绝已吊销的客户端证书 subject=%q serial=%s 备注=%q",
				sanitizeMsg(leaf.Subject.String()), leaf.SerialNumber, sanitizeMsg(note))
			writeClientCertPage(w, http.StatusForbidden, "证书已被吊销",
				"你的证书本身是有效的，但已被签发者列入吊销名单。请重新签发一张。")
			return
		}
		if isPageRequest(r.URL.Path) {
			log.Printf("[auth] 通过 subject=%q serial=%s",
				sanitizeMsg(leaf.Subject.String()), leaf.SerialNumber)
		}
		next.ServeHTTP(w, r)
	})
}

// peerLeaf 取出客户端证书的叶子，并确认它真的通过了链校验。
func peerLeaf(r *http.Request) (*x509.Certificate, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, false
	}
	if len(r.TLS.VerifiedChains) == 0 {
		return nil, false
	}
	return r.TLS.PeerCertificates[0], true
}

// hasClientAuthEKU 判断证书是否被允许用于客户端认证。
//
// 没有 EKU 扩展的证书按 RFC 5280 视为可用于任何用途，所以只在「写了 EKU 但
// 不含 clientAuth」时拒绝。标准库的 TLS 栈大概率已经查过这一条，这里再查一次
// 是为了让这条性质不依赖标准库的实现细节。
func hasClientAuthEKU(c *x509.Certificate) bool {
	if len(c.ExtKeyUsage) == 0 {
		return true
	}
	for _, e := range c.ExtKeyUsage {
		if e == x509.ExtKeyUsageClientAuth || e == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// deniedFingerprint 按证书 DER 的 SHA-256 查吊销名单，第二个返回值表示命中。
func (a *ClientAuth) deniedFingerprint(c *x509.Certificate) (string, bool) {
	if len(a.denied) == 0 {
		return "", false
	}
	note, ok := a.denied[Fingerprint(c)]
	return note, ok
}

// Fingerprint 返回证书 DER 的 SHA-256 小写十六进制，与吊销名单的写法一致。
//
// 导出给自检与运维脚本用：签发一张证书之后，拿它去填名单。
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// isPageRequest 判断这次请求是不是「打开一个页面」，而不是页面里的子资源。
//
// 只在页面级请求上打身份日志：一个阅读页会带几十个 /static/ 与 /doc/*/res/
// 子请求，逐条记会把日志淹掉，反而看不出谁在什么时候访问过。
func isPageRequest(p string) bool {
	if strings.HasPrefix(p, "/static/") {
		return false
	}
	return !strings.Contains(p, "/res/")
}

// writeClientCertPage 输出一张说明页。
//
// 故意不回 JSON：看到这一页的是浏览器用户，不是脚本。也不发 WWW-Authenticate，
// 客户端证书不是 HTTP 认证方案，发那个头会误导。
//
// CSP 单独收紧一次：默认策略不给内联样式，这里显式放开 style-src。页面内容
// 全是常量，放宽样式不影响安全。
func writeClientCertPage(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`+html.EscapeString(title)+`</title>
<style>
body{margin:0;padding:3rem 1.5rem;font:16px/1.7 system-ui,-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;color:#1f2328;background:#f6f8fa}
main{max-width:40rem;margin:0 auto;background:#fff;border:1px solid #d0d7de;border-radius:8px;padding:2rem}
h1{font-size:1.25rem;margin:0 0 1rem}
p{margin:0;color:#57606a}
</style>
</head>
<body><main><h1>`+html.EscapeString(title)+`</h1><p>`+html.EscapeString(detail)+`</p></main></body>
</html>
`)
}
