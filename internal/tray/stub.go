//go:build !((darwin && cgo) || windows || linux)

package tray

import (
	"context"
	"errors"
)

// Supported is false: this build has no icon support (for example macOS without cgo).
func Supported() bool { return false }

var ErrRunning = errors.New("the Masamong icon is already running")

// Run is unavailable in this build.
func Run(context.Context, Options) error { return errors.New("this build has no menu bar icon") }
