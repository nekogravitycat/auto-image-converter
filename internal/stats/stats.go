// Package stats tracks conversion counters — how many files were converted, how
// many failed, and how much disk space was saved — for display in the UI.
//
// Lifetime totals are persisted to a small JSON file next to the executable so
// they survive restarts; session totals count only the current run. All methods
// are safe for concurrent use.
package stats

import (
	"encoding/json"
	"os"
	"sync"
)

// Snapshot is an immutable view of the counters at a point in time.
type Snapshot struct {
	Converted  int64 `json:"converted"`
	Failed     int64 `json:"failed"`
	BytesSaved int64 `json:"bytes_saved"`
}

// Stats holds the live counters. Use Load to construct one.
type Stats struct {
	mu       sync.Mutex
	path     string
	lifetime Snapshot
	session  Snapshot
	dirty    bool
}

// Load reads persisted lifetime totals from path (missing or unreadable files
// simply start from zero) and returns a Stats ready for use.
func Load(path string) *Stats {
	s := &Stats{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.lifetime)
	}
	return s
}

// RecordConverted counts one successful conversion and adds bytesSaved (which
// may be zero or, for a file that grew, is clamped to zero) to the totals.
func (s *Stats) RecordConverted(bytesSaved int64) {
	if bytesSaved < 0 {
		bytesSaved = 0
	}
	s.mu.Lock()
	s.lifetime.Converted++
	s.lifetime.BytesSaved += bytesSaved
	s.session.Converted++
	s.session.BytesSaved += bytesSaved
	s.dirty = true
	s.mu.Unlock()
}

// RecordFailed counts one failed conversion.
func (s *Stats) RecordFailed() {
	s.mu.Lock()
	s.lifetime.Failed++
	s.session.Failed++
	s.dirty = true
	s.mu.Unlock()
}

// Lifetime returns the persisted-since-first-run totals.
func (s *Stats) Lifetime() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lifetime
}

// Session returns the totals for the current run.
func (s *Stats) Session() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// Save writes the lifetime totals to disk if they have changed since the last
// save. It is best-effort: an error is returned but callers typically ignore it,
// since losing a few counts is harmless. Call it periodically and on shutdown to
// avoid a write on every conversion during a large batch.
func (s *Stats) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	data, err := json.MarshalIndent(s.lifetime, "", "  ")
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.dirty = false
	s.mu.Unlock()
	return os.WriteFile(s.path, data, 0o644)
}
