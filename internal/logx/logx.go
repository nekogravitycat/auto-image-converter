// Package logx provides a small file-based logger for the application.
//
// Because the application is compiled with -ldflags="-H=windowsgui" it has no
// console window, so all diagnostic output is written to a log file located
// next to the executable. All messages are in English.
//
// The log file is size-rotated so a long-running background process cannot grow
// it without bound: when the active file would exceed maxLogBytes it is renamed
// to a single ".1" backup and a fresh file is started, capping disk use at
// roughly twice maxLogBytes.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// maxLogBytes is the size at which the active log file is rotated. One backup is
// kept, so total on-disk log usage is bounded at about twice this value.
const maxLogBytes = 5 << 20 // 5 MiB

// Logger writes timestamped, leveled messages to a size-rotated file (and also
// to stderr, which is useful for console builds and harmlessly discarded by the
// windowsgui build).
type Logger struct {
	l    *log.Logger
	file *rotatingWriter // nil when the file could not be opened (stderr only)
	fan  *fanoutWriter
}

// New creates a Logger that appends to the file at path, rotating it by size. If
// the file cannot be opened, a Logger writing only to stderr is returned
// together with the error, so the application can still run and report
// diagnostics.
func New(path string) (*Logger, error) {
	fan := &fanoutWriter{}
	rw, err := newRotatingWriter(path, maxLogBytes)
	if err != nil {
		fan.add(os.Stderr)
		return &Logger{l: log.New(fan, "", log.LstdFlags), fan: fan}, fmt.Errorf("could not open log file %s: %w", path, err)
	}
	// Write to both the file and stderr; stderr is harmless (and useful) for
	// console builds and simply goes nowhere for the windowsgui build.
	fan.add(rw)
	fan.add(os.Stderr)
	return &Logger{
		l:    log.New(fan, "", log.LstdFlags),
		file: rw,
		fan:  fan,
	}, nil
}

// Infof logs an informational message.
func (lg *Logger) Infof(format string, args ...any) {
	lg.l.Printf("[INFO] "+format, args...)
}

// Warnf logs a warning message.
func (lg *Logger) Warnf(format string, args ...any) {
	lg.l.Printf("[WARN] "+format, args...)
}

// Errorf logs an error message.
func (lg *Logger) Errorf(format string, args ...any) {
	lg.l.Printf("[ERROR] "+format, args...)
}

// Close closes the underlying log file, if any.
func (lg *Logger) Close() error {
	if lg.file != nil {
		return lg.file.Close()
	}
	return nil
}

// fanoutWriter writes each message to every registered sink under a lock, so
// log output can be duplicated to the file and stderr atomically. Each
// log.Logger call issues exactly one Write, so each sink receives one whole
// line per message.
type fanoutWriter struct {
	mu      sync.Mutex
	writers []io.Writer
}

func (f *fanoutWriter) add(w io.Writer) {
	f.mu.Lock()
	f.writers = append(f.writers, w)
	f.mu.Unlock()
}

func (f *fanoutWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// rotatingWriter is an io.Writer backed by a file that is rotated once it grows
// past maxSize. It is safe for concurrent use, matching log.Logger's guarantee.
type rotatingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

// newRotatingWriter opens (creating if needed) the file at path for appending
// and records its current size so an existing log is continued, then rotated
// once it crosses maxSize.
func newRotatingWriter(path string, maxSize int64) (*rotatingWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, statErr := file.Stat(); statErr == nil {
		size = info.Size()
	}
	return &rotatingWriter{path: path, maxSize: maxSize, file: file, size: size}, nil
}

// Write appends p, rotating first if the file would exceed maxSize. If the file
// has become unavailable (a rotation failed to reopen it), the write is dropped
// silently and reported as successful so it does not abort the sibling stderr
// writer in the MultiWriter — diagnostics still reach stderr.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return len(p), nil
	}
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			w.file = nil
			return len(p), nil
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the active file, renames it to a single ".1" backup (replacing
// any previous backup), and opens a fresh empty file. The caller holds w.mu.
func (w *rotatingWriter) rotate() error {
	_ = w.file.Close()
	// Best effort: on Windows os.Rename replaces an existing destination. If it
	// fails (e.g. the backup is locked) the O_TRUNC below still bounds the size,
	// only leaving the previous backup in place.
	_ = os.Rename(w.path, w.path+".1")
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.file = file
	w.size = 0
	return nil
}

// Close closes the underlying file. Safe to call more than once.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
