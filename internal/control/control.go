// Package control provides a Windows-native channel for asking the otherwise
// windowless background process to shut down gracefully.
//
// Because the program is built with -H=windowsgui it has no console, so console
// CTRL events cannot reach it and os.Interrupt is effectively undeliverable from
// an unrelated process. A named event object is the lightweight, dependency-free
// way for an external helper (stop.ps1) to trigger the same graceful drain that
// Ctrl+C would during a console run.
package control

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// StopEventName is the session-local Win32 event that, when set, requests a
// graceful shutdown. stop.ps1 opens and sets an event of this exact name; keep
// the two in sync.
const StopEventName = "auto-image-converter-stop"

// stopEventPollMillis bounds how long the waiter blocks per iteration so it can
// notice that shutdown was already triggered by another source (a console
// interrupt) and exit, instead of lingering until the event is ever set.
const stopEventPollMillis = 500

// NotifyStop returns a context that is cancelled when the process should shut
// down — either an OS interrupt/termination signal arrives, or the named stop
// event is set by an external helper such as stop.ps1. The returned function
// releases the signal registration; call it (typically via defer) when done.
//
// If the event cannot be created the process still runs; only the external
// graceful-stop path is unavailable, and that is logged rather than fatal.
func NotifyStop(parent context.Context, log *logx.Logger) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)

	name, err := windows.UTF16PtrFromString(StopEventName)
	if err != nil {
		log.Warnf("graceful stop channel unavailable: %v", err)
		return ctx, stop
	}
	// Manual reset (arg 1), initially non-signaled (arg 0). Manual reset keeps
	// the request latched so the poll below is guaranteed to observe it.
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		log.Warnf("graceful stop channel unavailable (%v); only a console interrupt will stop gracefully", err)
		return ctx, stop
	}

	go func() {
		for {
			r, waitErr := windows.WaitForSingleObject(h, stopEventPollMillis)
			if waitErr != nil {
				return // handle closed or wait failed; nothing more we can do
			}
			if r == windows.WAIT_OBJECT_0 {
				log.Infof("received external stop request; shutting down gracefully")
				stop()
				return
			}
			// Timed out: if shutdown already began via another path, stop waiting.
			if ctx.Err() != nil {
				return
			}
		}
	}()

	return ctx, stop
}
