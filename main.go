// Command auto-image-converter is a Windows utility that watches folders for new
// PNG screenshots and automatically converts them to JPEG or HEIF to save disk
// space. It runs from the system tray; a settings window (opened from the tray)
// manages the monitored folders, each with its own conversion settings, and
// offers one-time conversions and drag-and-drop.
//
// It is built with -ldflags="-H=windowsgui" so it runs without a console window;
// all diagnostics are written to a log file next to the executable, which the
// window's "Open log" button follows live in a terminal.
package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/control"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
	"github.com/nekogravitycat/auto-image-converter/internal/manager"
	"github.com/nekogravitycat/auto-image-converter/internal/paths"
	"github.com/nekogravitycat/auto-image-converter/internal/ui"
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

	// Refuse to run a second copy in the same session. Two instances watching the
	// same tree would race on the shared "*.converting.tmp" name and on output
	// naming. If the guard itself cannot be established we log and continue rather
	// than block startup.
	lock, ok, err := control.AcquireSingleInstance()
	if err != nil {
		log.Warnf("could not establish single-instance guard (%v); continuing without it", err)
	} else if !ok {
		// Without a console there is nothing to see, so tell the user why this
		// launch does nothing before exiting.
		log.Errorf("another instance is already running; exiting")
		ui.AlreadyRunningAlert()
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
	log.Infof("configuration: %d job(s), max_workers=%d", len(cfg.Jobs), cfg.App.MaxWorkers)

	exePath, exeErr := os.Executable()
	if exeErr != nil {
		log.Warnf("could not resolve executable path (%v); autostart may not work", exeErr)
	}
	statsPath := filepath.Join(appPaths.BaseDir, "stats.json")

	mgr := manager.New(cfg, appPaths.ConfigFile, statsPath, appPaths.HeifScriptPath(), exePath, log)

	// Cancelled on an OS interrupt or session logoff, so the UI ends its message
	// loop and the manager drains gracefully. Tray "Exit" does the same directly.
	ctx, stop := control.NotifyStop(context.Background())
	defer stop()

	// Open the window on first run (no jobs yet); otherwise honor the preference.
	startMinimized := cfg.App.StartMinimized && len(cfg.Jobs) > 0

	if err := ui.Run(ctx, mgr, log, appPaths.LogFile, startMinimized); err != nil {
		log.Errorf("UI failed to start: %v", err)
	}

	mgr.Shutdown()
	log.Infof("Auto Image Converter shutting down")
}
