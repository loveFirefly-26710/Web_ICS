//go:build windows

package main

import (
	"os/exec"
)

// openURL 用系统默认浏览器打开 url。
//
// 走 rundll32 的 url.dll 协议处理器，不用 `cmd /c start`：
// 后者会闪一个控制台窗口，而且 url 里一旦有 & 或空格，start 的参数
// 解析会把它们当成新命令或分隔符，很容易开出错误的页面。
func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
