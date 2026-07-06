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

// watcher holds the state needed to react to filesystem events.
type watcher struct {
	cfg      config.Config
	conv     *convert.Converter
	log      *logx.Logger
	fsw      *fsnotify.Watcher
	rules    fsutil.TraversalRules
	sem      chan struct{}
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

	root := fsutil.AbsClean(cfg.Watcher.WatchDirectory)
	ignoredDir, hasIgnored := conv.IgnoredDir()

	w := &watcher{
		cfg:  cfg,
		conv: conv,
		log:  log,
		fsw:  fsw,
		rules: fsutil.TraversalRules{
			Root:       root,
			Recursive:  cfg.Watcher.Recursive,
			MaxDepth:   cfg.Watcher.MaxDepth,
			IgnoredDir: ignoredDir,
			HasIgnored: hasIgnored,
		},
		sem:      make(chan struct{}, cfg.Converter.MaxWorkers),
		inFlight: make(map[string]bool),
	}

	w.addTree(root)
	log.Infof("watching %s (recursive=%v, max_depth=%d)", root, cfg.Watcher.Recursive, cfg.Watcher.MaxDepth)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(event)
		case err, ok := <-fsw.Errors:
			if !ok {
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

	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.inFlight, path)
			w.mu.Unlock()
		}()

		w.sem <- struct{}{}
		defer func() { <-w.sem }()

		if !waitUntilReady(path) {
			w.log.Warnf("file not ready or vanished, skipping: %s", path)
			return
		}
		if err := w.conv.Convert(path); err != nil {
			w.log.Errorf("conversion failed for %s: %v", path, err)
		}
	}()
}

// waitUntilReady blocks until path appears fully written (its size is stable
// across consecutive polls and it can be opened for reading) or a timeout is
// reached. It returns false if the file never stabilizes or disappears.
func waitUntilReady(path string) bool {
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
		time.Sleep(readinessInterval)
	}
	return false
}
