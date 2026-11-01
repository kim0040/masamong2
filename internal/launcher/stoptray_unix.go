//go:build !windows

package launcher

// StopTray is a Windows notification-area lifecycle operation.
func StopTray(string) error { return nil }
