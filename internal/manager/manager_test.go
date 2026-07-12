package manager

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
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

// testLogger returns a logger writing to a throwaway file.
func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, _ := logx.New(filepath.Join(t.TempDir(), "test.log"))
	t.Cleanup(func() { log.Close() })
	return log
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

// TestReconcileReportsMissingWatchFolder guards a regression: the status set for
// a job whose folder is gone used to be overwritten by the status-refresh pass at
// the end of reconcile, so the window reported "idle" for a folder that could not
// be watched at all.
func TestReconcileReportsMissingWatchFolder(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	m := newTestManager(t, config.Config{
		Version: config.CurrentVersion,
		App:     config.AppConfig{MaxWorkers: 2},
		Jobs: []config.JobConfig{{
			Name:           "Gone",
			WatchDirectory: missing,
			Enabled:        true,
			Mode:           config.ModeMonitor,
			TargetFormat:   config.FormatJPEG,
			Quality:        90,
			PostAction:     config.ActionReplace,
		}},
	})
	defer m.Shutdown()

	m.reconcile() // synchronous, unlike the fire-and-forget triggerReconcile

	jobs := m.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if !strings.Contains(jobs[0].Status, "watch folder not found") {
		t.Errorf("status = %q, want it to report the missing folder", jobs[0].Status)
	}
}

// TestReconcileStatusForRunnableJob checks the healthy counterpart: a job whose
// folder exists ends up reported as monitoring.
func TestReconcileStatusForRunnableJob(t *testing.T) {
	watchDir := t.TempDir()

	m := newTestManager(t, config.Config{
		Version: config.CurrentVersion,
		App:     config.AppConfig{MaxWorkers: 2},
		Jobs: []config.JobConfig{{
			Name:           "Live",
			WatchDirectory: watchDir,
			Enabled:        true,
			Mode:           config.ModeMonitor,
			TargetFormat:   config.FormatJPEG,
			Quality:        90,
			PostAction:     config.ActionReplace,
		}},
	})
	defer m.Shutdown()

	m.reconcile()

	if got := m.Jobs()[0].Status; got != "monitoring" {
		t.Errorf("status = %q, want %q", got, "monitoring")
	}
}

// TestEngineHandleRetireWaitsForItsLastUser guards a regression: rebuilding the
// engine for a new worker count used to close the engine that an in-flight batch
// was still converting through, shutting down its HEIF workers mid-conversion and
// failing every remaining file. A retired engine must stay open until its last
// user is done.
func TestEngineHandleRetireWaitsForItsLastUser(t *testing.T) {
	h := &engineHandle{eng: convert.NewEngine(1, testLogger(t), "")}

	h.use() // a batch starts converting through this engine

	h.retire() // reconcile swaps in a replacement
	if h.eng.Closed() {
		t.Fatal("engine was closed while a batch was still using it")
	}

	h.done() // the batch finishes
	if !h.eng.Closed() {
		t.Error("engine was not closed after its last user finished")
	}
}

// An engine that is retired with no users at all closes immediately.
func TestEngineHandleRetireWithNoUsersClosesNow(t *testing.T) {
	h := &engineHandle{eng: convert.NewEngine(1, testLogger(t), "")}
	h.retire()
	if !h.eng.Closed() {
		t.Error("an unused retired engine should be closed immediately")
	}
}

// TestChangingWorkersMidBatchConvertsEverything is the end-to-end counterpart:
// changing the worker count while a startup batch is running must not disturb it.
func TestChangingWorkersMidBatchConvertsEverything(t *testing.T) {
	watchDir := t.TempDir()
	const files = 12
	for i := range files {
		writePNG(t, filepath.Join(watchDir, fmt.Sprintf("shot-%d.png", i)))
	}

	m := newTestManager(t, config.Config{
		Version: config.CurrentVersion,
		App:     config.AppConfig{MaxWorkers: 1}, // serialize, so the batch is still in flight below
		Jobs: []config.JobConfig{{
			Name:           "Busy",
			WatchDirectory: watchDir,
			Enabled:        true,
			Mode:           config.ModeMonitor,
			BatchOnStartup: true,
			Recursive:      true,
			TargetFormat:   config.FormatJPEG,
			Quality:        90,
			PostAction:     config.ActionReplace,
		}},
	})
	m.Start()
	defer m.Shutdown()

	// Rebuild the engine, synchronously, while the startup batch is running.
	m.SetMaxWorkers(4)
	m.reconcile()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if session, _ := m.Stats(); session.Converted+session.Failed >= files {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	session, _ := m.Stats()
	if session.Failed != 0 {
		t.Errorf("failed = %d, want 0: the batch's engine was disturbed by the rebuild", session.Failed)
	}
	if session.Converted != files {
		t.Errorf("converted = %d, want %d", session.Converted, files)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
