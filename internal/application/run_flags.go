package application

import "sync/atomic"

// runFlags are the service's "one at a time" marks for background passes. Each is set while its pass
// runs (or is claimed) and belongs to one service: the real instance and the practice instance in the
// same process never block each other.
type runFlags struct {
	retention atomic.Bool // a usage_gc pass is running, scheduled or run from the console (retention.go)
}
