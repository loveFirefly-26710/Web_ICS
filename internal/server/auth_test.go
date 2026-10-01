package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"web_ics/internal/doclib"
	"web_ics/internal/nav"
)

// ---------------------------------------------------------------- 造证书

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "web_ics test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("自签 CA 失败: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 CA 失败: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue 用 CA 签一张证书，返回可直接给 tls.Config 用的结构与叶子。
func (ca *testCA) issue(t *testing.T, tmpl *x509.Certificate) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	tmpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析签发的证书失败: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// serverCert 签一张给 127.0.0.1 用的服务器证书。
func (ca *testCA) serverCert(t *testing.T) tls.Certificate {
	t.Helper()
	c, _ := ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return c
}

// clientCert 签一张客户端证书。ekus 为 nil 表示不带 EKU 扩展。
func (ca *testCA) clientCert(t *testing.T, cn string, ekus []x509.ExtKeyUsage) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	return ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: ekus,
	})
}

// writePEM 把证书与私钥落到临时文件，供 LoadX509KeyPair 走真实加载路径。
func writePEM(t *testing.T, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	var certPEM []byte
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatalf("序列化私钥失败: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// ---------------------------------------------------------------- 握手级测试

// startAuthServer 起一个真 TLS 测试服务器，闸门是我们要验的那一层。
func startAuthServer(t *testing.T, ca *ClientAuth, serverCert tls.Certificate) *httptest.Server {
	t.Helper()
	certPath, keyPath := writePEM(t, serverCert)
	tlsCfg, err := ca.TLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("组装 TLS 配置失败: %v", err)
	}

	srv := httptest.NewUnstartedServer(ca.withClientCertAuth(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		})))
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// getWith 用指定客户端证书请求一次，返回状态码与错误文本。
func getWith(t *testing.T, srv *httptest.Server, roots *x509.CertPool, cert *tls.Certificate) (int, string) {
	t.Helper()
	cfg := &tls.Config{RootCAs: roots}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

func TestClientCertAuthHandshake(t *testing.T) {
	ca := newTestCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	goodCert, _ := ca.clientCert(t, "good@example", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	revokedCert, revokedLeaf := ca.clientCert(t, "revoked@example", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	// 有 EKU 但不含 clientAuth。这张能不能被拦住，标准库文档没有明说，
	// 下面的断言只要求「不能拿到 200」，并把实际拦在哪一层打出来。
	wrongEKU, _ := ca.clientCert(t, "server-only@example", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	// 另一个 CA 签的证书，链校验必然失败。
	otherCA := newTestCA(t)
	otherCert, _ := otherCA.clientCert(t, "other@example", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	deny := "# 测试名单\nsha256:" + Fingerprint(revokedLeaf) + " 测试用\n"
	auth, err := NewClientAuth(ca.pem, deny)
	if err != nil {
		t.Fatalf("构造 ClientAuth 失败: %v", err)
	}
	srv := startAuthServer(t, auth, ca.serverCert(t))

	t.Run("不带证书被拦在 HTTP 层", func(t *testing.T) {
		code, body := getWith(t, srv, pool, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d，期望 401（err=%s）", code, body)
		}
		if !strings.Contains(body, "需要客户端证书") {
			t.Errorf("401 页面没有说明文案: %q", body)
		}
		if !strings.Contains(body, "text/html") && !strings.Contains(body, "<html") {
			t.Errorf("401 应当返回 HTML 说明页，得到 %q", body)
		}
	})

	t.Run("带有效证书放行", func(t *testing.T) {
		code, body := getWith(t, srv, pool, &goodCert)
		if code != http.StatusOK || body != "ok" {
			t.Fatalf("status = %d body = %q，期望 200 ok", code, body)
		}
	})

	t.Run("吊销名单命中给 403", func(t *testing.T) {
		code, body := getWith(t, srv, pool, &revokedCert)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d，期望 403（err=%s）", code, body)
		}
		if !strings.Contains(body, "吊销") {
			t.Errorf("403 页面没有说明文案: %q", body)
		}
	})

	t.Run("EKU 不含 clientAuth 不能通过", func(t *testing.T) {
		code, msg := getWith(t, srv, pool, &wrongEKU)
		if code == http.StatusOK {
			t.Fatal("EKU 只有 serverAuth 的证书不该拿到 200")
		}
		// 这一条是设计里留下的待确认项，把实测结果打出来：
		// code=0 说明握手就被标准库拒了，code=403 说明是闸门拒的。
		t.Logf("实测：EKU 只有 serverAuth 时 status=%d，%s", code, truncateForLog(msg))
	})

	t.Run("不受信 CA 签的证书不能通过", func(t *testing.T) {
		code, msg := getWith(t, srv, pool, &otherCert)
		if code == http.StatusOK {
			t.Fatal("别的 CA 签的证书不该拿到 200")
		}
		if code != 0 {
			t.Logf("不受信证书被拒于 HTTP 层 status=%d，%s", code, truncateForLog(msg))
		}
	})
}

// ---------------------------------------------------------------- 配置与闸门

func TestNewClientAuthRejectsBadDenyList(t *testing.T) {
	ca := newTestCA(t)
	cases := map[string]string{
		"长度不对":   "sha256:abcd",
		"不是十六进制": "sha256:" + strings.Repeat("z", 64),
		"少了内容":   "sha256:",
	}
	for name, deny := range cases {
		if _, err := NewClientAuth(ca.pem, deny); err == nil {
			t.Errorf("%s：应当报错，实际通过了", name)
		}
	}
	// 合法的几条要能过：带前缀、不带前缀、带备注、空行与注释。
	ok := "# 注释\n\n" +
		"sha256:" + strings.Repeat("a", 64) + "\n" +
		strings.Repeat("b", 64) + " 备注文字\n"
	if _, err := NewClientAuth(ca.pem, ok); err != nil {
		t.Errorf("合法名单不该报错: %v", err)
	}
}

func TestNewClientAuthRejectsBadCA(t *testing.T) {
	if _, err := NewClientAuth([]byte("这不是 PEM"), ""); err == nil {
		t.Error("CA 不是有效 PEM 时应当报错")
	}
}

func TestLoadClientAuthFromFiles(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	denyPath := filepath.Join(dir, "deny.txt")
	if err := os.WriteFile(caPath, ca.pem, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(denyPath, []byte("sha256:"+strings.Repeat("c", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadClientAuth(caPath, denyPath)
	if err != nil {
		t.Fatalf("从文件加载失败: %v", err)
	}
	if len(auth.denied) != 1 {
		t.Errorf("名单条数 = %d，期望 1", len(auth.denied))
	}
	// 名单文件写了但不存在时要报错，不能当成「没有名单」。
	if _, err := LoadClientAuth(caPath, filepath.Join(dir, "nope.txt")); err == nil {
		t.Error("名单文件不存在时应当报错")
	}
}

// 中间件顺序：401 也要带安全头，且未认证的请求不该碰到并发闸门。
func TestGateSitsInsideSecurityHeaders(t *testing.T) {
	ca := newTestCA(t)
	auth, err := NewClientAuth(ca.pem, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ClientAuth = auth
	srv := New(cfg, doclib.Empty(t.TempDir()), nav.NewCache(), nil)

	rec := httptest.NewRecorder()
	// 手工构造一个没有 TLS 状态的请求，等价于「客户端没发证书」。
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/libs", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("401 响应缺少安全头，X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "style-src") {
		t.Errorf("说明页应当自己收紧 CSP，得到 %q", got)
	}
}

// 不配 ClientAuth 时行为与加这个功能之前一致：没有闸门，直接 200。
func TestNoClientAuthMeansNoGate(t *testing.T) {
	srv := New(DefaultConfig(), doclib.Empty(t.TempDir()), nav.NewCache(), nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/libs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("没配 client-ca 时 /api/libs 应当 200，得到 %d", rec.Code)
	}
}

// truncateForLog 截断错误文本，避免测试日志里出现整页内容。
func truncateForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
