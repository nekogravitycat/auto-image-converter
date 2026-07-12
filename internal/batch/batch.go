// Package batch performs a one-time scan of a directory and converts all
// existing PNG files in parallel, bounded by the shared worker pool. It is used
// for a job's startup batch, for "convert now" actions, for one-time jobs, and
// for ad-hoc (drag-and-drop) conversions.
package batch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// Summary reports the outcome of a batch run, for notifications and logging.
type Summary struct {
	Converted  int
	Failed     int
	BytesSaved int64
}

// Run scans the job's watch directory for existing PNG files (respecting the
// recursion and depth settings, and skipping the output subtree) and converts
// them concurrently, bounded by the shared Engine's worker pool.
//
// Run honours ctx for graceful shutdown: once ctx is cancelled it stops
// launching new conversions and waits for the in-flight ones to finish. Because
// each conversion writes to a temp file and only renames it into place when
// complete, letting the in-flight ones drain never leaves a partial output
// behind — an interrupted batch simply stops early with no stray temp files.
func Run(ctx context.Context, spec convert.JobSpec, eng *convert.Engine, log *logx.Logger) Summary {
	rules := spec.TraversalRules()
	root := rules.Root

	files := collectPNGs(root, rules, log)
	return runFiles(ctx, spec, eng, log, files)
}

// RunFiles converts an explicit set of files (used for drag-and-drop / ad-hoc
// conversions), skipping any that are not PNGs. Each file is converted with the
// given spec; the spec's output/post-action settings decide where results go.
func RunFiles(ctx context.Context, spec convert.JobSpec, eng *convert.Engine, log *logx.Logger, files []string) Summary {
	var pngs []string
	for _, f := range files {
		if convert.IsPNG(f) {
			pngs = append(pngs, f)
		}
	}
	return runFiles(ctx, spec, eng, log, pngs)
}

// runFiles converts the given PNG paths concurrently, bounded by the shared
// worker pool, and returns a summary of the outcome.
func runFiles(ctx context.Context, spec convert.JobSpec, eng *convert.Engine, log *logx.Logger, files []string) Summary {
	if len(files) == 0 {
		log.Infof("[%s] batch: no PNG files to convert", spec.Name)
		return Summary{}
	}
	log.Infof("[%s] batch: converting %d PNG file(s)", spec.Name, len(files))

	var (
		converted, failed atomic.Int64
		saved             atomic.Int64
		wg                sync.WaitGroup
	)
	for i, file := range files {
		if ctx.Err() != nil {
			log.Infof("[%s] batch: shutdown requested, skipping %d remaining file(s)", spec.Name, len(files)-i)
			break
		}
		wg.Add(1)
		eng.Acquire()
		go func(path string) {
			defer wg.Done()
			defer eng.Release()
			res, err := eng.Convert(spec, path)
			if err != nil {
				failed.Add(1)
				log.Errorf("[%s] batch conversion failed for %s: %v", spec.Name, path, err)
				return
			}
			converted.Add(1)
			if d := res.OriginalBytes - res.OutputBytes; d > 0 {
				saved.Add(d)
			}
		}(file)
	}

	done := waitDone(&wg)
	if ctx.Err() != nil {
		// Shutting down: wait for in-flight conversions, but do not hang forever
		// on a wedged one.
		select {
		case <-done:
			log.Infof("[%s] batch: stopped after in-flight conversions completed", spec.Name)
		case <-time.After(shutdownDrainTimeout):
			log.Warnf("[%s] batch: in-flight conversions did not finish within %s; they will be terminated", spec.Name, shutdownDrainTimeout)
		}
	} else {
		<-done
		log.Infof("[%s] batch: finished", spec.Name)
	}

	return Summary{
		Converted:  int(converted.Load()),
		Failed:     int(failed.Load()),
		BytesSaved: saved.Load(),
	}
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
// simply be redone. It sweeps the job's watched tree within the configured
// recursion/depth scope and, in output_folder mode, the output tree as well,
// since that is where converted files (and thus their temps) are written.
func SweepTemps(spec convert.JobSpec, log *logx.Logger) int {
	rules := spec.TraversalRules()
	removed := sweepTree(rules.Root, rules, log)

	// In output_folder mode the converted files live under the output root,
	// which the walk above deliberately excludes (or may never reach). Sweep it
	// fully so temps there are cleaned up too.
	if ignored, ok := spec.IgnoredDir(); ok {
		removed += sweepTree(ignored, fsutil.TraversalRules{Root: ignored, Recursive: true}, log)
	} else if spec.PostAction == config.ActionOutputFolder && spec.OutputDir != "" {
		// output_folder mode with the output root outside the watch tree (the
		// case where it is inside is covered by the branch above).
		removed += sweepTree(spec.OutputDir, fsutil.TraversalRules{Root: spec.OutputDir, Recursive: true}, log)
	}

	if removed > 0 {
		log.Infof("[%s] startup cleanup: removed %d orphaned temporary file(s)", spec.Name, removed)
	}
	return removed
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
