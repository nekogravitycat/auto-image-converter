// Package control provides small process-lifecycle helpers: a single-instance
// guard (see singleton.go) and a shutdown-signal context.
//
// Shutdown is normally driven by the UI (the tray "Exit" command), which cancels
// the manager's root context directly. NotifyStop additionally cancels that
// context on an OS interrupt or termination signal, so a console run or a
// session logoff still triggers the same graceful drain.
package control

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// NotifyStop returns a context that is cancelled when the process receives an
// interrupt or termination signal, and a function that releases the signal
// registration (call it, typically via defer, when done). The UI can also cancel
// shutdown independently through its own cancel function.
func NotifyStop(parent context.Context) (context.Context, func()) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
