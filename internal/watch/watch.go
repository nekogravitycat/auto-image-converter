// Package watch monitors the configured directory tree for new PNG files using
// fsnotify (event-driven; no polling) and converts them once they are fully
// written.
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// File-readiness tuning: a file is considered ready once its size stops
// changing across consecutive polls and it can be opened for reading.
const (
	readinessInterval    = 400 * time.Millisecond
	readinessStablePolls = 2
	readinessMaxAttempts = 75 // ~30s ceiling before giving up
)

// shutdownDrainTimeout bounds how long a graceful shutdown waits for in-flight
// conversions before returning; a wedged conversion must not hang exit.
// Stragglers are terminated by process shutdown (and the HEIF workers' cleanup
// job).
const shutdownDrainTimeout = 30 * time.Second

// watcher holds the state needed to react to filesystem events.
type watcher struct {
	cfg      config.Config
	conv     *convert.Converter
	log      *logx.Logger
	fsw      *fsnotify.Watcher
	rules    fsutil.TraversalRules
	sem      chan struct{}
	ctx      context.Context // cancelled on shutdown; drives graceful drain
	wg       sync.WaitGroup  // tracks in-flight conversion goroutines
	mu       sync.Mutex
	inFlight map[string]bool
}

// Run starts watching the configured directory tree and blocks until ctx is
// cancelled. It returns an error only if the watcher cannot be created.
func Run(ctx context.Context, cfg config.Config, conv *convert.Converter, log *logx.Logger) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	rules := conv.TraversalRules()
	root := rules.Root

	w := &watcher{
		cfg:      cfg,
		conv:     conv,
		log:      log,
		fsw:      fsw,
		rules:    rules,
		sem:      make(chan struct{}, cfg.Converter.MaxWorkers),
		ctx:      ctx,
		inFlight: make(map[string]bool),
	}

	w.addTree(root)
	log.Infof("watching %s (recursive=%v, max_depth=%d)", root, cfg.Watcher.Recursive, cfg.Watcher.MaxDepth)

	for {
		select {
		case <-ctx.Done():
			log.Infof("watcher: shutdown requested, waiting for in-flight conversions")
			w.drain()
			return nil
		case event, ok := <-fsw.Events:
			if !ok {
				w.drain()
				return nil
			}
			w.handleEvent(event)
		case err, ok := <-fsw.Errors:
			if !ok {
				w.drain()
				return nil
			}
			log.Errorf("watcher error: %v", err)
		}
	}
}

// addTree registers dir and all of its non-pruned descendant directories with
// the watcher. fsnotify does not recurse on its own, so every directory within
// the allowed depth must be added explicitly.
func (w *watcher) addTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.log.Warnf("watch setup: cannot access %s: %v", path, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if w.rules.PruneDir(path) {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(path); err != nil {
			w.log.Warnf("could not watch directory %s: %v", path, err)
		}
		return nil
	})
}

// handleEvent reacts to a single filesystem event.
func (w *watcher) handleEvent(e fsnotify.Event) {
	if e.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}

	info, err := os.Stat(e.Name)
	if err != nil {
		return // file may have been removed or renamed already
	}

	if info.IsDir() {
		// A new subdirectory: start watching it (and its allowed descendants).
		if e.Op&fsnotify.Create != 0 && !w.rules.PruneDir(e.Name) {
			w.addTree(e.Name)
		}
		return
	}

	if !convert.IsPNG(e.Name) {
		return
	}
	if w.inIgnored(e.Name) {
		return
	}
	w.schedule(e.Name)
}

// inIgnored reports whether path lies within the excluded output subtree.
func (w *watcher) inIgnored(path string) bool {
	if !w.rules.HasIgnored {
		return false
	}
	return fsutil.Within(w.rules.IgnoredDir, fsutil.AbsClean(path))
}

// schedule converts path once, de-duplicating concurrent events for the same
// file and bounding total concurrency with the worker semaphore.
func (w *watcher) schedule(path string) {
	w.mu.Lock()
	if w.inFlight[path] {
		w.mu.Unlock()
		return
	}
	w.inFlight[path] = true
	w.mu.Unlock()

	// Add to the wait group before launching so a shutdown that begins right now
	// still drains this conversion. schedule is only ever called from the single
	// event-loop goroutine, which stops (and only then calls wg.Wait) once ctx is
	// cancelled, so Add never races with Wait.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.inFlight, path)
			w.mu.Unlock()
		}()

		// Wait for the file to finish being written *before* taking a worker
		// slot. Readiness polling can last up to ~30s; holding a slot during it
		// would let a few slowly-written files starve the pool so that no actual
		// conversion can run. The slot bounds encoding concurrency, not waiting.
		if !waitUntilReady(w.ctx, path) {
			w.log.Warnf("file not ready or vanished, skipping: %s", path)
			return
		}

		w.sem <- struct{}{}
		defer func() { <-w.sem }()

		if err := w.conv.Convert(path); err != nil {
			w.log.Errorf("conversion failed for %s: %v", path, err)
		}
	}()
}

// drain waits for in-flight conversion goroutines to finish, up to
// shutdownDrainTimeout. Stragglers past the deadline are left to be terminated
// by process exit (and the HEIF workers' cleanup job), so a wedged conversion
// cannot hang shutdown.
func (w *watcher) drain() {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownDrainTimeout):
		w.log.Warnf("watcher: in-flight conversions did not finish within %s; they will be terminated", shutdownDrainTimeout)
	}
}

// waitUntilReady blocks until path appears fully written (its size is stable
// across consecutive polls and it can be opened for reading) or a timeout is
// reached. It returns false if the file never stabilizes, disappears, or ctx is
// cancelled — the last so a shutdown does not wait out the full readiness
// ceiling for a file that is still being written.
func waitUntilReady(ctx context.Context, path string) bool {
	var lastSize int64 = -1
	stable := 0

	for attempt := 0; attempt < readinessMaxAttempts; attempt++ {
		info, err := os.Stat(path)
		if err != nil {
			return false // file removed or inaccessible
		}
		size := info.Size()

		if size > 0 && size == lastSize {
			stable++
			if stable >= readinessStablePolls {
				if f, err := os.Open(path); err == nil {
					f.Close()
					return true
				}
			}
		} else {
			stable = 0
		}
		lastSize = size
		select {
		case <-ctx.Done():
			return false // shutting down; abandon this not-yet-ready file
		case <-time.After(readinessInterval):
		}
	}
	return false
}
