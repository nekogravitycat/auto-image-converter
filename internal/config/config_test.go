package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGeneratesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")

	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config file was not generated: %v", err)
	}
	if len(warnings) == 0 {
		t.Errorf("expected a warning about generating the default config")
	}
	if cfg.Version != currentVersion {
		t.Errorf("default version = %d, want %d", cfg.Version, currentVersion)
	}
	if len(cfg.Jobs) != 0 {
		t.Errorf("fresh config should have no jobs, got %d", len(cfg.Jobs))
	}
	if cfg.App.MaxWorkers != defaultWorkers {
		t.Errorf("default max_workers = %d, want %d", cfg.App.MaxWorkers, defaultWorkers)
	}
	if !cfg.App.StartMinimized {
		t.Errorf("default start_minimized = false, want true")
	}

	// The generated file must parse cleanly on reload, with no further warnings.
	reloaded, warnings2, err := Load(path)
	if err != nil {
		t.Fatalf("reloading generated config failed: %v", err)
	}
	if len(warnings2) != 0 {
		t.Errorf("reload of generated config produced warnings: %v", warnings2)
	}
	if reloaded.App.MaxWorkers != cfg.App.MaxWorkers {
		t.Errorf("reloaded max_workers = %d, want %d", reloaded.App.MaxWorkers, cfg.App.MaxWorkers)
	}
}

func TestJobValidateCorrectsInvalidFields(t *testing.T) {
	j := JobConfig{
		Name:           "Shots",
		WatchDirectory: `C:\some\dir`,
		Mode:           "bogus",
		TargetFormat:   "png",
		Quality:        999,
		PostAction:     "nuke",
	}

	warnings := j.applyDefaultsAndValidate(0)

	if j.Mode != ModeMonitor {
		t.Errorf("mode = %q, want fallback %q", j.Mode, ModeMonitor)
	}
	if j.TargetFormat != FormatJPEG {
		t.Errorf("target format = %q, want fallback %q", j.TargetFormat, FormatJPEG)
	}
	if j.Quality != fallbackQuality {
		t.Errorf("quality = %d, want fallback %d", j.Quality, fallbackQuality)
	}
	if j.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want fallback %q", j.PostAction, ActionReplace)
	}
	if len(warnings) < 4 {
		t.Errorf("expected at least 4 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestJobValidateNormalizesCaseAndClampsDepth(t *testing.T) {
	j := JobConfig{
		Name:           "Shots",
		WatchDirectory: `C:\dir`,
		Mode:           "MONITOR",
		MaxDepth:       -5,
		TargetFormat:   "heif",
		Quality:        50,
		PostAction:     "REPLACE",
	}

	j.applyDefaultsAndValidate(0)

	if j.Mode != ModeMonitor {
		t.Errorf("mode = %q, want %q", j.Mode, ModeMonitor)
	}
	if j.TargetFormat != FormatHEIF {
		t.Errorf("target format = %q, want %q", j.TargetFormat, FormatHEIF)
	}
	if j.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want %q", j.PostAction, ActionReplace)
	}
	if j.MaxDepth != 0 {
		t.Errorf("max depth = %d, want 0 (negative clamped)", j.MaxDepth)
	}
}

func TestOutputFolderWithoutDirFallsBackToReplace(t *testing.T) {
	j := JobConfig{
		Name:            "Shots",
		WatchDirectory:  `C:\dir`,
		Mode:            ModeMonitor,
		TargetFormat:    "JPEG",
		Quality:         90,
		PostAction:      ActionOutputFolder,
		OutputDirectory: "   ",
	}

	j.applyDefaultsAndValidate(0)

	if j.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want fallback to %q when output_directory is empty",
			j.PostAction, ActionReplace)
	}
}

func TestDeriveJobName(t *testing.T) {
	if got := deriveJobName(`C:\Users\me\Pictures\VRChat`, 0); got != "VRChat" {
		t.Errorf("deriveJobName from folder = %q, want VRChat", got)
	}
	if got := deriveJobName("   ", 2); got != "job-3" {
		t.Errorf("deriveJobName from empty = %q, want job-3", got)
	}
}

func TestOutputExtension(t *testing.T) {
	if got := (JobConfig{TargetFormat: FormatJPEG}).OutputExtension(); got != ".jpg" {
		t.Errorf("JPEG extension = %q, want .jpg", got)
	}
	if got := (JobConfig{TargetFormat: FormatHEIF}).OutputExtension(); got != ".heic" {
		t.Errorf("HEIF extension = %q, want .heic", got)
	}
}

func TestUsesHEIF(t *testing.T) {
	cfg := Config{Jobs: []JobConfig{{TargetFormat: FormatJPEG}, {TargetFormat: FormatHEIF}}}
	if !cfg.UsesHEIF() {
		t.Errorf("UsesHEIF = false, want true when a job targets HEIF")
	}
	cfg = Config{Jobs: []JobConfig{{TargetFormat: FormatJPEG}}}
	if cfg.UsesHEIF() {
		t.Errorf("UsesHEIF = true, want false when no job targets HEIF")
	}
}
