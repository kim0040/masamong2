//go:build windows

package corpus

import "testing"

// refuseFIFO has nothing to check on Windows, which has no named pipes in the file system.
func refuseFIFO(*testing.T, string) {}
