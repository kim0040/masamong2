package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                = windows.NewLazySystemDLL("psapi.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	getProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	getProcessHandleCnt  = kernel32.NewProc("GetProcessHandleCount")
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS from psapi.h.
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

// platformRSSMB is the working set, Windows' closest match to Linux VmRSS, so a
// load test on a Windows machine still reports memory.
func platformRSSMB() float64 {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	ok, _, _ := getProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if ok == 0 {
		return -1
	}
	return float64(c.workingSetSize) / (1 << 20)
}

// platformHandles counts open handles (files, sockets, events), the leak signal
// that open file descriptors give on Linux.
func platformHandles() int {
	var n uint32
	ok, _, _ := getProcessHandleCnt.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
	if ok == 0 {
		return -1
	}
	return int(n)
}
