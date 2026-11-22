package application

import "sync/atomic"

// runFlags are the service's "one at a time" marks for background passes. Each is set while its pass
// runs (or is claimed) and belongs to one service: the real instance and the practice instance in the
// same process never block each other.
type runFlags struct {
	retention   atomic.Bool // a usage_gc pass is running, scheduled or run from the console (retention.go)
	statsRollup atomic.Bool // the statistics rollup is running (statsClaim, stats_rollup.go)
	morning     atomic.Bool // the morning fortune post is running (fortune.go)
	reminders   atomic.Bool // the reminder pass is running (reminders.go)
	styles      atomic.Bool // the room style refresh is running (roomstyle.go)
	idleThreads atomic.Bool // closing idle threads is running (threads.go)
	notices     atomic.Bool // the notice pass is running (notices.go)
	noticesTick atomic.Bool // the scheduler's notice tick is running (schedule.go)
	updateNews  atomic.Bool // the release notes announcement is running (updatenews_announce.go)
}
