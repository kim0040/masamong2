package main

import (
	"syscall"
	"unsafe"
)

// procMemory reads the process counters Windows keeps: current and peak working set, and private
// bytes (committed memory only this process owns).
type procMemory struct {
	WorkingSet     uint64
	PeakWorkingSet uint64
	Private        uint64
}

var (
	psapi                = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemory = psapi.NewProc("GetProcessMemoryInfo")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProc   = kernel32.NewProc("GetCurrentProcess")
)

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

func readProcMemory() procMemory {
	h, _, _ := procGetCurrentProc.Call()
	var c processMemoryCountersEx
	c.Cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := procGetProcessMemory.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(c.Cb)); r == 0 {
		return procMemory{}
	}
	return procMemory{WorkingSet: uint64(c.WorkingSetSize), PeakWorkingSet: uint64(c.PeakWorkingSetSize), Private: uint64(c.PrivateUsage)}
}
