package manager

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// writePNG writes a small valid PNG at path.
func writePNG(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestManager(t *testing.T, cfg config.Config) *Manager {
	t.Helper()
	dir := t.TempDir()
	log, _ := logx.New(filepath.Join(dir, "test.log"))
	t.Cleanup(func() { log.Close() })
	cfgPath := filepath.Join(dir, "config.yml")
	statsPath := filepath.Join(dir, "stats.json")
	return New(cfg, cfgPath, statsPath, filepath.Join(dir, "heif.py"), filepath.Join(dir, "app.exe"), log)
}

// TestManagerConvertsOnStartup drives the full pipeline (manager -> batch ->
// engine -> convert) for a JPEG job, which needs no external tools, and verifies
// the startup batch converts an existing PNG and, in replace mode, removes the
// original.
func TestManagerConvertsOnStartup(t *testing.T) {
	watchDir := t.TempDir()
	writePNG(t, filepath.Join(watchDir, "shot.png"))

	cfg := config.Config{
		Version: 1,
		App:     config.AppConfig{MaxWorkers: 2},
		Jobs: []config.JobConfig{{
			Name:           "Test",
			WatchDirectory: watchDir,
			Enabled:        true,
			Mode:           config.ModeMonitor,
			BatchOnStartup: true,
			Recursive:      true,
			TargetFormat:   config.FormatJPEG,
			Quality:        85,
			PostAction:     config.ActionReplace,
		}},
	}

	m := newTestManager(t, cfg)
	m.Start()
	defer m.Shutdown()

	jpg := filepath.Join(watchDir, "shot.jpg")
	png := filepath.Join(watchDir, "shot.png")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(jpg) && !fileExists(png) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !fileExists(jpg) {
		t.Fatalf("expected converted %s to exist", jpg)
	}
	if fileExists(png) {
		t.Errorf("expected original %s to be removed in replace mode", png)
	}

	session, _ := m.Stats()
	if session.Converted < 1 {
		t.Errorf("session converted count = %d, want >= 1", session.Converted)
	}
}

// TestManagerAddJobPersists checks that adding a job writes it to the config
// file so a subsequent load sees it.
func TestManagerAddJobPersists(t *testing.T) {
	m := newTestManager(t, config.Config{Version: 1, App: config.AppConfig{MaxWorkers: 4}})

	// A folder that does not exist keeps the manager from launching a batch or
	// watcher, isolating the persistence behavior.
	id := m.AddJob(config.JobConfig{
		Name:           "Persisted",
		WatchDirectory: filepath.Join(t.TempDir(), "does-not-exist"),
		Enabled:        true,
		Mode:           config.ModeMonitor,
		TargetFormat:   config.FormatJPEG,
		Quality:        90,
		PostAction:     config.ActionReplace,
	})
	if id < 0 {
		t.Fatalf("AddJob returned invalid id %d", id)
	}

	data, err := os.ReadFile(m.cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if !strings.Contains(string(data), "Persisted") {
		t.Errorf("saved config does not contain the new job:\n%s", data)
	}

	reloaded, _, err := config.Load(m.cfgPath)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if len(reloaded.Jobs) != 1 || reloaded.Jobs[0].Name != "Persisted" {
		t.Errorf("reloaded jobs = %+v, want one job named Persisted", reloaded.Jobs)
	}

	m.Shutdown()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
