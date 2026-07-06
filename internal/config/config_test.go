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
	if cfg.Converter.TargetFormat != FormatJPEG {
		t.Errorf("default target format = %q, want %q", cfg.Converter.TargetFormat, FormatJPEG)
	}
	if cfg.Converter.Quality != generatedQuality {
		t.Errorf("default quality = %d, want %d", cfg.Converter.Quality, generatedQuality)
	}
	if cfg.FileManagement.PostAction != ActionReplace {
		t.Errorf("default post_action = %q, want %q", cfg.FileManagement.PostAction, ActionReplace)
	}

	// The generated file must parse cleanly on reload.
	reloaded, _, err := Load(path)
	if err != nil {
		t.Fatalf("reloading generated config failed: %v", err)
	}
	if reloaded.Converter.Quality != generatedQuality {
		t.Errorf("reloaded quality = %d, want %d", reloaded.Converter.Quality, generatedQuality)
	}
	if reloaded.Watcher.WatchDirectory != cfg.Watcher.WatchDirectory {
		t.Errorf("reloaded watch dir = %q, want %q", reloaded.Watcher.WatchDirectory, cfg.Watcher.WatchDirectory)
	}
}

func TestValidateCorrectsInvalidFields(t *testing.T) {
	c := Config{}
	c.Watcher.WatchDirectory = `C:\some\dir`
	c.Converter.TargetFormat = "png"
	c.Converter.Quality = 999
	c.Converter.MaxWorkers = 0
	c.FileManagement.PostAction = "nuke"

	warnings := c.applyDefaultsAndValidate()

	if c.Converter.TargetFormat != FormatJPEG {
		t.Errorf("target format = %q, want fallback %q", c.Converter.TargetFormat, FormatJPEG)
	}
	if c.Converter.Quality != fallbackQuality {
		t.Errorf("quality = %d, want fallback %d", c.Converter.Quality, fallbackQuality)
	}
	if c.Converter.MaxWorkers != defaultWorkers {
		t.Errorf("max workers = %d, want fallback %d", c.Converter.MaxWorkers, defaultWorkers)
	}
	if c.FileManagement.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want fallback %q", c.FileManagement.PostAction, ActionReplace)
	}
	if len(warnings) < 4 {
		t.Errorf("expected at least 4 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateNormalizesCaseAndClampsDepth(t *testing.T) {
	c := Config{}
	c.Watcher.WatchDirectory = `C:\dir`
	c.Watcher.MaxDepth = -5
	c.Converter.TargetFormat = "heif"
	c.Converter.Quality = 50
	c.Converter.MaxWorkers = 2
	c.FileManagement.PostAction = "REPLACE"

	c.applyDefaultsAndValidate()

	if c.Converter.TargetFormat != FormatHEIF {
		t.Errorf("target format = %q, want %q", c.Converter.TargetFormat, FormatHEIF)
	}
	if c.FileManagement.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want %q", c.FileManagement.PostAction, ActionReplace)
	}
	if c.Watcher.MaxDepth != 0 {
		t.Errorf("max depth = %d, want 0 (negative clamped)", c.Watcher.MaxDepth)
	}
}

func TestOutputFolderWithoutDirFallsBackToReplace(t *testing.T) {
	c := Config{}
	c.Watcher.WatchDirectory = `C:\dir`
	c.Converter.TargetFormat = "JPEG"
	c.Converter.Quality = 90
	c.Converter.MaxWorkers = 1
	c.FileManagement.PostAction = ActionOutputFolder
	c.FileManagement.OutputDirectory = "   "

	c.applyDefaultsAndValidate()

	if c.FileManagement.PostAction != ActionReplace {
		t.Errorf("post_action = %q, want fallback to %q when output_directory is empty",
			c.FileManagement.PostAction, ActionReplace)
	}
}

func TestOutputExtension(t *testing.T) {
	jpeg := Config{Converter: ConverterConfig{TargetFormat: FormatJPEG}}
	if got := jpeg.OutputExtension(); got != ".jpg" {
		t.Errorf("JPEG extension = %q, want .jpg", got)
	}
	heif := Config{Converter: ConverterConfig{TargetFormat: FormatHEIF}}
	if got := heif.OutputExtension(); got != ".heic" {
		t.Errorf("HEIF extension = %q, want .heic", got)
	}
}
