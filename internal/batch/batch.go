// Package batch performs a one-time scan of the watch directory and converts
// all existing PNG files in parallel, bounded by the configured worker count.
package batch

import (
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// Run scans the watch directory for existing PNG files (respecting the
// recursion and depth settings, and skipping the output subtree) and converts
// them concurrently, capped at cfg.Converter.MaxWorkers.
func Run(cfg config.Config, conv *convert.Converter, log *logx.Logger) {
	root := fsutil.AbsClean(cfg.Watcher.WatchDirectory)
	ignoredDir, hasIgnored := conv.IgnoredDir()
	rules := fsutil.TraversalRules{
		Root:       root,
		Recursive:  cfg.Watcher.Recursive,
		MaxDepth:   cfg.Watcher.MaxDepth,
		IgnoredDir: ignoredDir,
		HasIgnored: hasIgnored,
	}

	files := collectPNGs(root, rules, log)
	if len(files) == 0 {
		log.Infof("startup batch: no existing PNG files found in %s", root)
		return
	}
	log.Infof("startup batch: converting %d existing PNG file(s)", len(files))

	semaphore := make(chan struct{}, cfg.Converter.MaxWorkers)
	var wg sync.WaitGroup
	for _, file := range files {
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
	wg.Wait()
	log.Infof("startup batch: finished")
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
