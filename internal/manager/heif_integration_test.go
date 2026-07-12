package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/convert"
)

// heifScript returns the path to the bundled HEIF script, skipping the test when
// the HEIF runtime (Python + pillow-heif) is not installed on this machine.
func heifScript(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python"); err != nil {
		t.Skip("python not on PATH; skipping HEIF integration test")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "tools", "heif_convert.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("HEIF script not found at %s", script)
	}
	if err := exec.Command("python", script, "--check").Run(); err != nil {
		t.Skipf("HEIF environment self-test failed (pillow-heif not installed?): %v", err)
	}
	return script
}

// TestConvertOnceCanProduceHEIFWithoutAHEIFJob guards a regression: the HEIF
// worker pool used to be created only when a *configured job* targeted HEIF. A
// drag-and-drop conversion is not a job, so choosing HEIF for dropped files while
// no job used HEIF (the common case — and the only case on a fresh install with
// no jobs at all) failed every single file with "worker pool not initialized".
func TestConvertOnceCanProduceHEIFWithoutAHEIFJob(t *testing.T) {
	script := heifScript(t)

	dir := t.TempDir()
	log := testLogger(t)
	src := filepath.Join(dir, "dropped.png")
	writePNG(t, src)

	// A configuration whose only job is JPEG: nothing here targets HEIF.
	cfg := config.Config{
		Version: config.CurrentVersion,
		App:     config.AppConfig{MaxWorkers: 2},
		Jobs: []config.JobConfig{{
			Name:           "JPEG only",
			WatchDirectory: t.TempDir(),
			Enabled:        true,
			Mode:           config.ModeMonitor,
			TargetFormat:   config.FormatJPEG,
			Quality:        90,
			PostAction:     config.ActionReplace,
		}},
	}
	m := New(cfg, filepath.Join(dir, "config.yml"), filepath.Join(dir, "stats.json"), script, filepath.Join(dir, "app.exe"), log)
	defer m.Shutdown()

	// Exactly what the drop dialog produces: a one-off HEIF conversion.
	spec := convert.SpecFromJob(config.JobConfig{
		Name:         "Drag & drop",
		TargetFormat: config.FormatHEIF,
		Quality:      90,
		PostAction:   config.ActionReplace,
	})
	m.ConvertOnce([]string{src}, spec, "Drag & drop")

	heic := filepath.Join(dir, "dropped.heic")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(heic) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !fileExists(heic) {
		session, _ := m.Stats()
		t.Fatalf("dropped file was not converted to HEIF (%d converted, %d failed)", session.Converted, session.Failed)
	}
	if fileExists(src) {
		t.Error("original PNG should have been replaced")
	}
	if session, _ := m.Stats(); session.Failed != 0 {
		t.Errorf("failed conversions = %d, want 0", session.Failed)
	}
}
