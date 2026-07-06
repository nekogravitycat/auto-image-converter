// Command auto-image-converter is a lightweight Windows background utility that
// watches a folder for new PNG screenshots and automatically converts them to
// JPEG or HEIF to save disk space.
//
// It is intended to be built with -ldflags="-H=windowsgui" so it runs without a
// console window; all diagnostics are written to a log file next to the
// executable.
package main

import (
	"context"
	"os"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/batch"
	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/control"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
	"github.com/nekogravitycat/auto-image-converter/internal/paths"
	"github.com/nekogravitycat/auto-image-converter/internal/watch"
)

func main() {
	appPaths := paths.Resolve()

	log, err := logx.New(appPaths.LogFile)
	if err != nil {
		// The logger fell back to stderr; record the problem and keep running.
		log.Warnf("%v", err)
	}
	defer log.Close()

	log.Infof("Auto Image Converter starting (base directory: %s)", appPaths.BaseDir)

	// Refuse to run a second copy in the same session. Two instances watching
	// the same tree would race on the shared "*.converting.tmp" name and on
	// output naming, which can corrupt output or delete an original twice. If the
	// guard itself cannot be established we log and continue rather than block
	// startup — losing the protection is better than refusing to run at all.
	lock, ok, err := control.AcquireSingleInstance()
	if err != nil {
		log.Warnf("could not establish single-instance guard (%v); continuing without it", err)
	} else if !ok {
		log.Errorf("another instance is already running; exiting")
		return
	} else {
		defer lock.Release()
	}

	cfg, warnings, err := config.Load(appPaths.ConfigFile)
	if err != nil {
		log.Errorf("configuration problem: %v", err)
		log.Warnf("continuing with safe default settings")
	}
	for _, warning := range warnings {
		log.Warnf("config: %s", warning)
	}
	log.Infof("configuration: format=%s quality=%d workers=%d post_action=%s watch=%s",
		cfg.Converter.TargetFormat, cfg.Converter.Quality, cfg.Converter.MaxWorkers,
		cfg.FileManagement.PostAction, cfg.Watcher.WatchDirectory)

	if strings.TrimSpace(cfg.Watcher.WatchDirectory) == "" {
		log.Errorf("watch_directory is not set in %s; open it, set watch_directory to the folder you want to monitor, then restart", appPaths.ConfigFile)
		return
	}

	if info, statErr := os.Stat(cfg.Watcher.WatchDirectory); statErr != nil || !info.IsDir() {
		log.Errorf("watch_directory does not exist or is not a directory: %s; set it to an existing folder in %s, then restart",
			cfg.Watcher.WatchDirectory, appPaths.ConfigFile)
		return
	}

	conv := convert.New(cfg, log, appPaths.HeifScriptPath())
	defer conv.Close()
	if err := conv.ValidateEnvironment(); err != nil {
		// HEIF is selected but its runtime (Python + pillow-heif + the bundled
		// script) is not ready. Conversions will fail safely, leaving originals
		// intact; make the cause obvious in the log.
		log.Errorf("%v", err)
		log.Errorf("HEIF conversions will fail until Python and pillow-heif are installed (pip install pillow-heif) and tools/%s is present; originals will be kept",
			paths.HeifScriptName)
	}

	// Install shutdown handling up front so it is graceful during the startup
	// batch too, not only once the watcher is running. NotifyStop reacts to a
	// console interrupt and to the named stop event set by stop.ps1 — the latter
	// being how this windowless process is normally asked to quit.
	ctx, stop := control.NotifyStop(context.Background(), log)
	defer stop()

	// Clean up any *.converting.tmp orphaned by a previous run that was killed
	// mid-conversion (crash, power loss, or hard kill). Safe to redo — the
	// original PNG is only removed after a verified rename.
	batch.SweepTemps(cfg, conv, log)

	if cfg.Watcher.BatchOnStartup {
		batch.Run(ctx, cfg, conv, log)
	}

	if !cfg.Watcher.Enabled {
		log.Infof("background watching is disabled; exiting")
		return
	}

	if err := watch.Run(ctx, cfg, conv, log); err != nil {
		log.Errorf("watcher stopped with error: %v", err)
		return
	}
	log.Infof("Auto Image Converter shutting down")
}
