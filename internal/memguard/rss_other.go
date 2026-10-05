//go:build !windows && !linux

package memguard

// readRSS 在其它平台没有可靠实现，返回 false 让调用方退回近似值。
func readRSS() (uint64, bool) { return 0, false }
