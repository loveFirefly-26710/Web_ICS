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
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute, // 大 PDF 下载可能较慢
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 18, // 256KB
	}
}
