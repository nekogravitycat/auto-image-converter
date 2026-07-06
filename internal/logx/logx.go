// Package logx provides a small file-based logger for the application.
//
// Because the application is compiled with -ldflags="-H=windowsgui" it has no
// console window, so all diagnostic output is written to a log file located
// next to the executable. All messages are in English.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
)

// Logger writes timestamped, leveled messages to a file (and optionally an
// additional writer such as stderr for console builds).
type Logger struct {
	l    *log.Logger
	file *os.File
}

// New creates a Logger that appends to the file at path. If the file cannot be
// opened, a Logger writing to stderr is returned together with the error, so
// the application can still run and report diagnostics.
func New(path string) (*Logger, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return &Logger{l: log.New(os.Stderr, "", log.LstdFlags)}, fmt.Errorf("could not open log file %s: %w", path, err)
	}
	// Write to both the file and stderr; stderr is harmless (and useful) for
	// console builds and simply goes nowhere for the windowsgui build.
	writer := io.MultiWriter(file, os.Stderr)
	return &Logger{
		l:    log.New(writer, "", log.LstdFlags),
		file: file,
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
