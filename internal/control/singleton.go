package control

import (
	"golang.org/x/sys/windows"
)

// SingleInstanceMutexName is the session-local Win32 mutex used to ensure only
// one copy of the program runs at a time. It is session-scoped (no "Global\"
// prefix) to match StopEventName: the intended deployment is a per-user startup
// shortcut, so "one instance" means one per interactive session.
const SingleInstanceMutexName = "auto-image-converter-singleton"

// InstanceLock represents ownership of the single-instance mutex. Hold it for
// the whole lifetime of the process and Release it (typically via defer) on
// exit. The OS also releases the mutex automatically if the process dies
// without cleanup, so a crashed instance never blocks the next launch.
type InstanceLock struct {
	handle windows.Handle
}

// AcquireSingleInstance attempts to become the sole running instance.
//
//   - ok == true  → this process now owns the lock; call Release on shutdown.
//   - ok == false → another instance already holds it; the caller should exit.
//   - err != nil  → the guard could not be established at all (rare). The caller
//     may choose to continue without protection rather than refuse to start.
//
// The distinguishing signal is that CreateMutex returns a valid handle *and*
// ERROR_ALREADY_EXISTS when a mutex of the same name already exists.
func AcquireSingleInstance() (lock *InstanceLock, ok bool, err error) {
	return acquireNamedInstance(SingleInstanceMutexName)
}

// acquireNamedInstance is AcquireSingleInstance parameterized by mutex name, so
// tests can exercise the real Win32 path on a name of their own instead of the
// application's — which the running application itself may already own.
func acquireNamedInstance(mutexName string) (lock *InstanceLock, ok bool, err error) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, false, err
	}

	h, err := windows.CreateMutex(nil, false, name)
	if h == 0 {
		// No handle at all: the guard itself failed. Report err so the caller
		// can decide whether to proceed unprotected.
		return nil, false, err
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		// The mutex already existed: another instance owns it. We still received
		// our own handle to the same object, so close it and step aside.
		_ = windows.CloseHandle(h)
		return nil, false, nil
	}

	return &InstanceLock{handle: h}, true, nil
}

// Release closes the mutex handle, allowing a future instance to acquire it. It
// is safe to call on a nil lock and more than once.
func (l *InstanceLock) Release() {
	if l == nil || l.handle == 0 {
		return
	}
	_ = windows.CloseHandle(l.handle)
	l.handle = 0
}
