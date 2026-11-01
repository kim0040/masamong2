package memlimit

import "os"

func detect() (uint64, string) { return linuxMemory(os.ReadFile) }
