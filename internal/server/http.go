package server

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewHTTPServer 构造 http.Server，统一设置超时与头部上限。
//
// 每个超时都显式给出，不留 Go 的默认值（不限时）。慢速客户端靠这些超时
// 被踢掉，否则一个连接可以长期占住一个请求槽位。连接数本身由 Handler
// 里的并发闸门约束。
//
// tlsCfg 非空时调用方应当用 ListenAndServeTLS("", "") 启动：证书已经在
// tlsCfg.Certificates 里，不需要再传文件名。
func NewHTTPServer(addr string, h http.Handler, tlsCfg *tls.Config) *http.Server {
	// 标准库的 http.Server 拿 ReadHeaderTimeout 当作 TLS 握手的截止时间。
	// 开了客户端证书认证之后，握手期间用户要在浏览器里挑证书，10 秒经常不够，
	// 表现是「点确定没反应」：服务端早把连接掐了，日志里是一句握手 EOF。
	// 这里取 20 秒：比那个不够用的 10 秒翻一倍留余量，也把半开连接能占住
	// goroutine 与文件描述符的时间从 60 秒压到 20 秒。
	//
	// 正常访问不受影响：走到读请求头那一步的都已经过了证书校验，而未认证的
	// 握手本来就挡不住洪水（见 PLAN 的预期风险与应对设想）。
	headerTimeout := 10 * time.Second
	if tlsCfg != nil {
		headerTimeout = 20 * time.Second
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute, // 大 PDF 下载可能较慢
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 18, // 256KB
	}
}
