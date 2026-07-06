// Package batch performs a one-time scan of the watch directory and converts
// all existing PNG files in parallel, bounded by the configured worker count.
package batch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// shutdownDrainTimeout bounds how long an interrupted batch waits for in-flight
// conversions to finish before returning; a wedged conversion must not hang
// exit. Stragglers are terminated by process shutdown (and the HEIF workers'
// cleanup job).
const shutdownDrainTimeout = 30 * time.Second

// Run scans the watch directory for existing PNG files (respecting the
// recursion and depth settings, and skipping the output subtree) and converts
// them concurrently, capped at cfg.Converter.MaxWorkers.
//
// Run honours ctx for graceful shutdown: once ctx is cancelled it stops
// launching new conversions and waits for the in-flight ones to finish. Because
// each conversion writes to a temp file and only renames it into place when
// complete, letting the in-flight ones drain never leaves a partial output
// behind — an interrupted batch simply stops early with no stray temp files.
func Run(ctx context.Context, cfg config.Config, conv *convert.Converter, log *logx.Logger) {
	rules := conv.TraversalRules()
	root := rules.Root

	files := collectPNGs(root, rules, log)
	if len(files) == 0 {
		log.Infof("startup batch: no existing PNG files found in %s", root)
		return
	}
	log.Infof("startup batch: converting %d existing PNG file(s)", len(files))

	semaphore := make(chan struct{}, cfg.Converter.MaxWorkers)
	var wg sync.WaitGroup
	for i, file := range files {
		if ctx.Err() != nil {
			log.Infof("startup batch: shutdown requested, skipping %d remaining file(s)", len(files)-i)
			break
		}
		wg.Add(1)
		semaphore <- struct{}{}
		go func(path string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			if err := conv.Convert(path); err != nil {
				log.Errorf("batch conversion failed for %s: %v", path, err)
			}
		}(file)
	}

	done := waitDone(&wg)
	if ctx.Err() != nil {
		// Shutting down: wait for in-flight conversions, but do not hang forever
		// on a wedged one.
		select {
		case <-done:
			log.Infof("startup batch: stopped after in-flight conversions completed")
		case <-time.After(shutdownDrainTimeout):
			log.Warnf("startup batch: in-flight conversions did not finish within %s; they will be terminated", shutdownDrainTimeout)
		}
		return
	}
	<-done
	log.Infof("startup batch: finished")
}

// waitDone returns a channel that is closed once wg's counter reaches zero, so
// callers can wait on it in a select (e.g. to impose a shutdown deadline).
func waitDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// SweepTemps deletes orphaned "*.converting.tmp" files left by a previous run
// that was killed mid-conversion (a hard kill, crash, or power loss — anything
// the cooperative shutdown in Run and the watcher cannot catch).
//
// Such a temp is always safe to remove: a conversion writes to the temp name
// and only renames it into place — and only deletes the original PNG — after
// the output is fully verified, so a leftover temp is incomplete work that will
// simply be redone. It sweeps the watched tree within the configured
// recursion/depth scope and, in output_folder mode, the output tree as well,
// since that is where converted files (and thus their temps) are written.
func SweepTemps(cfg config.Config, conv *convert.Converter, log *logx.Logger) {
	rules := conv.TraversalRules()
	removed := sweepTree(rules.Root, rules, log)

	// In output_folder mode the converted files live under the output root,
	// which the walk above deliberately excludes (or may never reach). Sweep it
	// fully so temps there are cleaned up too.
	if cfg.FileManagement.PostAction == config.ActionOutputFolder {
		outRoot := fsutil.AbsClean(cfg.FileManagement.OutputDirectory)
		removed += sweepTree(outRoot, fsutil.TraversalRules{Root: outRoot, Recursive: true}, log)
	}

	if removed > 0 {
		log.Infof("startup cleanup: removed %d orphaned temporary file(s)", removed)
	}
}

// sweepTree walks root within rules and deletes any conversion temp files it
// finds, returning how many were removed.
func sweepTree(root string, rules fsutil.TraversalRules, log *logx.Logger) int {
	removed := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rules.PruneDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if convert.IsTempFile(path) {
			if err := os.Remove(path); err != nil {
				log.Warnf("startup cleanup: could not remove %s: %v", path, err)
			} else {
				log.Infof("startup cleanup: removed orphaned %s", path)
				removed++
			}
		}
		return nil
	})
	return removed
}

// collectPNGs walks root and returns the paths of all PNG files within the
// allowed traversal scope.
func collectPNGs(root string, rules fsutil.TraversalRules, log *logx.Logger) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Warnf("batch scan: cannot access %s: %v", path, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rules.PruneDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if convert.IsPNG(path) {
			files = append(files, path)
		}
		return nil
	})
	return files
}
