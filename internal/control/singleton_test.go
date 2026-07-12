package control

import "testing"

// TestAcquireSingleInstance_SecondCallIsRefused exercises the real Win32 path:
// while one InstanceLock is held, a second acquisition must report that another
// instance already owns the mutex (ok == false), and once the first lock is
// released a fresh acquisition must succeed again.
//
// It deliberately uses a test-only mutex name rather than the application's. With
// the real name the test would fail on any machine where the app itself is
// running — it would be the "other instance" the guard is designed to refuse.
func TestAcquireSingleInstance_SecondCallIsRefused(t *testing.T) {
	const mutexName = "auto-image-converter-singleton-test"

	first, ok, err := acquireNamedInstance(mutexName)
	if err != nil {
		t.Fatalf("first acquisition: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("first acquisition: expected to acquire the lock, but it was refused")
	}

	// A second attempt while the first is held must be refused, not error out.
	if _, ok, err := acquireNamedInstance(mutexName); err != nil {
		t.Fatalf("second acquisition: unexpected error: %v", err)
	} else if ok {
		t.Fatal("second acquisition: expected refusal while the lock is held")
	}

	// Releasing the first lock must let a new instance acquire it again.
	first.Release()

	third, ok, err := acquireNamedInstance(mutexName)
	if err != nil {
		t.Fatalf("third acquisition: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("third acquisition: expected to re-acquire the lock after release")
	}
	third.Release()

	// Release must be safe to call again and on a nil lock.
	third.Release()
	var nilLock *InstanceLock
	nilLock.Release()
}
