package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// seedConfig writes content to a temp config path and returns it.
func seedConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return path
}

// TestLoadPreservesValidValuesAndFillsMissing checks that a partial config keeps
// every readable value (including booleans set to false, which must not be
// mistaken for "missing") and fills the absent settings from defaults, then
// rewrites the file so a reload is clean.
func TestLoadPreservesValidValuesAndFillsMissing(t *testing.T) {
	path := seedConfig(t, `
app:
  max_workers: 3
jobs:
  - name: "Shots"
    watch_directory: "C:\\shots"
    enabled: false
    recursive: false
    target_format: "HEIF"
`)
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.MaxWorkers != 3 {
		t.Errorf("max_workers = %d, want preserved 3", cfg.App.MaxWorkers)
	}
	if len(cfg.Jobs) != 1 {
		t.Fatalf("job count = %d, want 1", len(cfg.Jobs))
	}
	job := cfg.Jobs[0]

	// Preserved values (the false booleans prove missing != present-zero):
	if job.WatchDirectory != `C:\shots` {
		t.Errorf("watch_directory = %q, want preserved", job.WatchDirectory)
	}
	if job.Enabled {
		t.Errorf("enabled = true, want preserved false")
	}
	if job.Recursive {
		t.Errorf("recursive = true, want preserved false")
	}
	if job.TargetFormat != FormatHEIF {
		t.Errorf("target_format = %q, want preserved HEIF", job.TargetFormat)
	}

	// Filled from defaults:
	if !job.BatchOnStartup {
		t.Errorf("batch_on_startup = false, want default true")
	}
	if job.Mode != ModeMonitor {
		t.Errorf("mode = %q, want default %q", job.Mode, ModeMonitor)
	}
	if job.Quality != generatedQuality {
		t.Errorf("quality = %d, want default %d", job.Quality, generatedQuality)
	}
	if len(warnings) == 0 {
		t.Fatal("expected warnings about missing settings")
	}

	// The file was rewritten to a complete, canonical form; a reload finds
	// nothing to fix and returns an identical config (idempotent).
	reloaded, warnings2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(warnings2) != 0 {
		t.Errorf("reload should produce no warnings, got: %v", warnings2)
	}
	if !reflect.DeepEqual(reloaded, cfg) {
		t.Errorf("reloaded config differs from first load:\n first=%+v\n second=%+v", cfg, reloaded)
	}
}

// TestLoadDropsUnknownFields checks that unrecognized keys are reported and
// removed from the rewritten file, while valid siblings are preserved.
func TestLoadDropsUnknownFields(t *testing.T) {
	path := seedConfig(t, `
app:
  max_workers: 4
  bogus_option: 123
jobs:
  - name: "Shots"
    watch_directory: "C:\\shots"
    target_format: "JPEG"
    quality: 70
mystery: true
`)
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Quality != 70 {
		t.Errorf("quality not preserved: %+v", cfg.Jobs)
	}

	foundUnknown := false
	for _, w := range warnings {
		if strings.Contains(w, "unrecognized") {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Errorf("expected an 'unrecognized setting' warning, got: %v", warnings)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "bogus_option") || strings.Contains(string(data), "mystery") {
		t.Errorf("rewritten config still contains unknown keys:\n%s", data)
	}
}

// TestLoadRecoversFromWrongType checks that a single wrong-typed field is
// recoverable: it falls back to its default while correctly-typed siblings are
// preserved, rather than the whole file being discarded.
func TestLoadRecoversFromWrongType(t *testing.T) {
	path := seedConfig(t, `
jobs:
  - name: "Shots"
    watch_directory: "C:\\shots"
    quality: "high"
`)
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load should recover from one wrong-typed field, got error: %v", err)
	}
	if len(cfg.Jobs) != 1 {
		t.Fatalf("job count = %d, want 1", len(cfg.Jobs))
	}
	if cfg.Jobs[0].WatchDirectory != `C:\shots` {
		t.Errorf("watch_directory = %q, want preserved", cfg.Jobs[0].WatchDirectory)
	}
	// A wrong-typed scalar is coerced to its zero value by yaml, then corrected
	// to a valid default; either way it must land in range, not stay as garbage.
	if q := cfg.Jobs[0].Quality; q < 1 || q > 100 {
		t.Errorf("quality = %d, want a valid in-range default after wrong type", q)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning for the wrong-typed quality field")
	}
}

// TestLoadFullCorruptionReturnsError checks that genuinely unparseable YAML is
// still treated as fatal (safe defaults + error), not silently rewritten.
func TestLoadFullCorruptionReturnsError(t *testing.T) {
	path := seedConfig(t, "app: [unterminated: flow: sequence")
	cfg, _, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for unparseable YAML")
	}
	// Safe defaults are still returned so the caller can carry on.
	if cfg.App.MaxWorkers != defaultWorkers {
		t.Errorf("max_workers = %d, want safe default %d on corruption", cfg.App.MaxWorkers, defaultWorkers)
	}
}
