package memlimit

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

var globalMemoryStatus = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// MEMORYSTATUSEX uses 64-bit byte counts on both 32- and 64-bit Windows.
type memoryStatus struct {
	Length, Load                                                                                         uint32
	TotalPhys, AvailPhys, TotalPageFile, AvailPageFile, TotalVirtual, AvailVirtual, AvailExtendedVirtual uint64
}

func detect() (uint64, string) {
	var m memoryStatus
	m.Length = uint32(unsafe.Sizeof(m))
	ok, _, _ := globalMemoryStatus.Call(uintptr(unsafe.Pointer(&m)))
	if ok == 0 || m.TotalPhys == 0 {
		return 0, "unset"
	}
	n := m.TotalPhys
	if m.TotalVirtual > 0 && m.TotalVirtual < n {
		n = m.TotalVirtual
	}
	return n, "GlobalMemoryStatusEx"
}
