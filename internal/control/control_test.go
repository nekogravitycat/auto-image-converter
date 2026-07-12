package control

import (
	"context"
	"testing"
	"time"
)

// TestNotifyStop_ParentCancelCancelsContext confirms NotifyStop derives from its
// parent: cancelling the parent cancels the returned context. (The signal path
// is exercised in production; sending a real interrupt to the test process would
// be disruptive, so the derivation guarantee is what we assert here.)
func TestNotifyStop_ParentCancelCancelsContext(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := NotifyStop(parent)
	defer stop()

	cancelParent()

	select {
	case <-ctx.Done():
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after the parent was cancelled")
	}
}

// TestNotifyStop_StopReleases confirms the returned stop function cancels the
// context (releasing the signal registration).
func TestNotifyStop_StopReleases(t *testing.T) {
	ctx, stop := NotifyStop(context.Background())
	stop()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after stop() was called")
	}
}
