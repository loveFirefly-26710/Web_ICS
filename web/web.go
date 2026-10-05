// Package web 把前端资源编译进二进制。
//
// 内嵌之后 dist/web_ics.exe 可以单独拷到任何目录运行，不再依赖旁边的 web/ 目录。
// 开发时仍然读磁盘上的 web/，改前端不用重编译：选择逻辑在
// internal/server 的 serveWebAsset 里，磁盘优先，找不到才用这份内嵌的。
//
// 这个目录同时是 Go 包和静态资源目录，把它拷到别处当纯静态目录用时记得排除 *.go。
package web

import "embed"

// Assets 是内嵌的前端资源。
//
// 路径用正斜杠，例如 "index.html"、"static/home.js"。
//
//go:embed index.html viewer.html static
var Assets embed.FS
