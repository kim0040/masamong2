//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	sessionUser32   = windows.NewLazySystemDLL("user32.dll")
	registerClass   = sessionUser32.NewProc("RegisterClassW")
	unregisterClass = sessionUser32.NewProc("UnregisterClassW")
	createWindow    = sessionUser32.NewProc("CreateWindowExW")
	destroyWindow   = sessionUser32.NewProc("DestroyWindow")
	defWindowProc   = sessionUser32.NewProc("DefWindowProcW")
	getMessage      = sessionUser32.NewProc("GetMessageW")
	dispatchMessage = sessionUser32.NewProc("DispatchMessageW")
	postMessage     = sessionUser32.NewProc("PostMessageW")
	postQuitMessage = sessionUser32.NewProc("PostQuitMessage")
	getModuleHandle = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	windowSequence  atomic.Uint32
)

const (
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmDestroy         = 0x0002
	wmDispose         = 0x8000 + 42
)

type windowClass struct {
	Style                              uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
}

type windowMessage struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	X, Y           int32
	Private        uint32
}

// A hidden TOP-LEVEL window receives shutdown broadcasts in an interactive session;
// HWND_MESSAGE would not. This lives in the supervisor, independently of the tray.
// A noninteractive Task Scheduler session is not guaranteed to receive these messages.
func supervisorNotifications(sigs chan os.Signal, finished <-chan struct{}) (func(), error) {
	_, cleanup, err := startSessionWindow(sigs, finished)
	return cleanup, err
}

func startSessionWindow(sigs chan os.Signal, finished <-chan struct{}) (uintptr, func(), error) {
	type result struct {
		hwnd uintptr
		err  error
	}
	ready := make(chan result, 1)
	exited := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(exited)
		name, _ := windows.UTF16PtrFromString(fmt.Sprintf("MasamongSupervisor-%d-%d", os.Getpid(), windowSequence.Add(1)))
		instance, _, _ := getModuleHandle.Call(0)
		notify := func(sig os.Signal) {
			if _, ending := sig.(sessionEndSignal); ending {
				// An end-session budget must supersede queued ordinary interrupts.
				// Do not lose it merely because os/signal filled the small buffer.
				for {
					select {
					case sigs <- sig:
						return
					case <-finished:
						return
					default:
					}
					select {
					case <-sigs:
					default:
					}
				}
			}
			select {
			case sigs <- sig:
			default:
			}
		}
		callback := windows.NewCallback(func(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
			switch msg {
			case wmQueryEndSession:
				return 1 // Windows may cancel the query; do not stop yet.
			case wmEndSession:
				if wparam != 0 {
					notify(sessionEndSignal{})
					// Keep the ENDSESSION handler alive until the child flushes, or our
					// bounded budget expires. Returning early lets Windows terminate us.
					timer := time.NewTimer(sessionEndGrace + 250*time.Millisecond)
					defer timer.Stop()
					select {
					case <-finished:
					case <-timer.C:
					}
				}
				return 0
			case wmClose:
				notify(os.Interrupt)
				return 0
			case wmDispose:
				destroyWindow.Call(hwnd)
				return 0
			case wmDestroy:
				postQuitMessage.Call(0)
				return 0
			}
			r, _, _ := defWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
			return r
		})
		wc := windowClass{WndProc: callback, Instance: instance, ClassName: name}
		if atom, _, err := registerClass.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			ready <- result{err: fmt.Errorf("register shutdown window: %w", err)}
			return
		}
		defer unregisterClass.Call(uintptr(unsafe.Pointer(name)), instance)
		hwnd, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
		if hwnd == 0 {
			ready <- result{err: fmt.Errorf("create shutdown window: %w", err)}
			return
		}
		ready <- result{hwnd: hwnd}
		var msg windowMessage
		for {
			ok, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(ok) <= 0 {
				break
			}
			dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()
	r := <-ready
	if r.err != nil {
		return 0, func() {}, r.err
	}
	return r.hwnd, func() {
		postMessage.Call(r.hwnd, wmDispose, 0, 0)
		<-exited
	}, nil
}
