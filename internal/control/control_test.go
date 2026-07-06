package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// TestNotifyStop_EventCancelsContext exercises the real Win32 path: after
// NotifyStop creates the named stop event, setting that event (as stop.ps1
// does) must cancel the returned context.
func TestNotifyStop_EventCancelsContext(t *testing.T) {
	log, _ := logx.New(filepath.Join(t.TempDir(), "test.log"))
	defer log.Close()

	ctx, stop := NotifyStop(context.Background(), log)
	defer stop()

	// The event exists now (CreateEvent ran synchronously inside NotifyStop).
	// Open and set it exactly as the stop helper would.
	name, err := windows.UTF16PtrFromString(StopEventName)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		t.Fatalf("OpenEvent: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetEvent(h); err != nil {
		t.Fatalf("SetEvent: %v", err)
	}

	select {
	case <-ctx.Done():
		// success: the waiter observed the event and cancelled the context.
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after the stop event was set")
	}
}
