//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openURL 在非 Windows 平台尝试用桌面环境的默认浏览器打开 url。
//
// 服务器上通常没有 xdg-open，也没有桌面环境，调用失败是常态。
// 调用方只记一条日志，不影响服务本身。
func openURL(url string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	return exec.Command(cmd, url).Start()
}
