//go:build windows

package memguard

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters 对应 Win32 的 PROCESS_MEMORY_COUNTERS。
//
// 字段顺序与宽度必须与 win32 定义严格一致，否则 GetProcessMemoryInfo
// 会按它自己的布局往这块内存里写，把栈上的数据写花。
// 前两个字段是 DWORD（4 字节），后面全是 SIZE_T（64 位下 8 字节）。
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// readRSS 读当前进程的工作集，也就是任务管理器里显示的那个数。
//
// 用 psapi 而不是 runtime.MemStats：后者的 Sys 是地址空间，与容器/任务管理器
// 的口径差得很远（工作集 21 MB 时它能报 146 MB）。
func readRSS() (uint64, bool) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	h, _, _ := procGetCurrentProcess.Call()
	r, _, _ := procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0, false
	}
	return uint64(c.workingSetSize), true
}
