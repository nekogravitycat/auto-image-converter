package control

import "testing"

// TestAcquireSingleInstance_SecondCallIsRefused exercises the real Win32 path:
// while one InstanceLock is held, a second AcquireSingleInstance must report
// that another instance already owns the mutex (ok == false), and once the
// first lock is released a fresh acquisition must succeed again.
func TestAcquireSingleInstance_SecondCallIsRefused(t *testing.T) {
	first, ok, err := AcquireSingleInstance()
	if err != nil {
		t.Fatalf("first AcquireSingleInstance: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("first AcquireSingleInstance: expected to acquire the lock, but it was refused")
	}

	// A second attempt while the first is held must be refused, not error out.
	if _, ok, err := AcquireSingleInstance(); err != nil {
		t.Fatalf("second AcquireSingleInstance: unexpected error: %v", err)
	} else if ok {
		t.Fatal("second AcquireSingleInstance: expected refusal while the lock is held")
	}

	// Releasing the first lock must let a new instance acquire it again.
	first.Release()

	third, ok, err := AcquireSingleInstance()
	if err != nil {
		t.Fatalf("third AcquireSingleInstance: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("third AcquireSingleInstance: expected to re-acquire the lock after release")
	}
	third.Release()

	// Release must be safe to call again and on a nil lock.
	third.Release()
	var nilLock *InstanceLock
	nilLock.Release()
}
