//go:build !race

package store

// raceDetector reports a -race build, where timing bounds do not hold.
const raceDetector = false
