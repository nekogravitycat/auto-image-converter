package logx

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRotatingWriter_RotatesAtMaxSize verifies that crossing maxSize moves the
// current file to a ".1" backup and starts a fresh file, bounding total size.
func TestRotatingWriter_RotatesAtMaxSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotatingWriter(path, 100)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	// Two 60-byte writes: the first fits, the second crosses 100 and must rotate
	// first, leaving the second write alone in a fresh file.
	chunk := []byte(strings.Repeat("a", 60))
	if _, err := w.Write(chunk); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := w.Write(chunk); err != nil {
		t.Fatalf("second write: %v", err)
	}

	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("expected rotated backup at %s: %v", path+".1", err)
	}
	if len(backup) != 60 {
		t.Fatalf("backup should hold the first write (60 bytes), got %d", len(backup))
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if len(current) != 60 {
		t.Fatalf("current file should hold only the second write (60 bytes), got %d", len(current))
	}
}

// TestRotatingWriter_ContinuesExistingFile verifies an existing log is appended
// to (its size is carried over), not truncated, on open.
func TestRotatingWriter_ContinuesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	w, err := newRotatingWriter(path, 1<<20)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "existing\nmore\n" {
		t.Fatalf("expected append, got %q", got)
	}
}

// TestLogger_ConcurrentWritesAreSafe exercises the logger from many goroutines
// under the race detector to confirm the rotating writer's locking holds.
func TestLogger_ConcurrentWritesAreSafe(t *testing.T) {
	log, err := New(filepath.Join(t.TempDir(), "app.log"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer log.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				log.Infof("goroutine %d line %d", n, j)
			}
		}(i)
	}
	wg.Wait()
}
