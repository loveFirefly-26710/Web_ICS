//go:build linux

package memguard

import (
	"os"
	"strconv"
	"strings"
)

// readRSS 读 /proc/self/statm 的第二个字段（常驻页数），乘页大小。
//
// 这个数与 cgroup 的 memory.current、docker stats 看到的 RSS 是同一个口径。
func readRSS() (uint64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
