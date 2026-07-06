// Package config loads, validates, and (when absent) generates the application
// configuration file (config.yml).
//
// The design goal is resilience: a missing, corrupt, or partially invalid
// configuration must never crash the application. Missing files are generated
// with sensible defaults; unreadable or malformed files fall back to safe
// defaults; individual invalid fields are corrected in place and reported as
// warnings so the caller can log them.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Supported target formats.
const (
	FormatJPEG = "JPEG"
	FormatHEIF = "HEIF"
)

// Supported post-conversion actions.
const (
	// ActionReplace writes the converted file next to the source and deletes
	// the original PNG after a verified successful conversion.
	ActionReplace = "replace"
	// ActionOutputFolder writes the converted file into a separate output
	// directory (mirroring the source's relative subpath) and keeps the
	// original PNG untouched.
	ActionOutputFolder = "output_folder"
)

// Default and safe-fallback values.
const (
	defaultFormat    = FormatJPEG
	fallbackQuality  = 80 // quality used when the configured value is invalid
	generatedQuality = 90 // quality written into a freshly generated config
	defaultWorkers   = 4
	defaultAction    = ActionReplace
)

// Config is the full application configuration, mirroring config.yml.
type Config struct {
	Watcher        WatcherConfig        `yaml:"watcher"`
	Converter      ConverterConfig      `yaml:"converter"`
	FileManagement FileManagementConfig `yaml:"file_management"`
}

// WatcherConfig controls the background watcher and startup batch.
type WatcherConfig struct {
	WatchDirectory string `yaml:"watch_directory"`
	Enabled        bool   `yaml:"enabled"`
	BatchOnStartup bool   `yaml:"batch_on_startup"`
	Recursive      bool   `yaml:"recursive"`
	// MaxDepth limits recursion: 0 means unlimited, N means at most N levels
	// below the watch root.
	MaxDepth int `yaml:"max_depth"`
}

// ConverterConfig controls the conversion engine.
type ConverterConfig struct {
	TargetFormat string `yaml:"target_format"`
	Quality      int    `yaml:"quality"`
	MaxWorkers   int    `yaml:"max_workers"`
}

// FileManagementConfig controls what happens to files after conversion.
type FileManagementConfig struct {
	PostAction      string `yaml:"post_action"`
	OutputDirectory string `yaml:"output_directory"`
}

// Load reads the configuration from path.
//
// Behavior:
//   - If the file does not exist, a default file is generated at path and the
//     defaults are returned.
//   - If the file cannot be read or parsed, safe defaults are returned together
//     with a non-nil error (the caller should log it and continue).
//   - Otherwise the file is parsed and validated; invalid fields are corrected
//     and returned as warnings.
//
// Load never panics.
func Load(path string) (cfg Config, warnings []string, err error) {
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		cfg = defaultConfig()
		if genErr := writeDefault(path, cfg); genErr != nil {
			return cfg, nil, fmt.Errorf("could not create default config at %s: %w", path, genErr)
		}
		return cfg, []string{fmt.Sprintf("config file not found; generated default at %s", path)}, nil
	}
	if readErr != nil {
		return SafeDefaults(), nil, fmt.Errorf("could not read config at %s: %w", path, readErr)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return SafeDefaults(), nil, fmt.Errorf("config at %s is corrupt: %w", path, err)
	}

	warnings = cfg.applyDefaultsAndValidate()
	return cfg, warnings, nil
}

// OutputExtension returns the file extension (including the dot) for the
// configured target format.
func (c Config) OutputExtension() string {
	if c.Converter.TargetFormat == FormatHEIF {
		return ".heic"
	}
	return ".jpg"
}

// applyDefaultsAndValidate corrects invalid fields in place and returns a list
// of human-readable warnings describing each correction.
func (c *Config) applyDefaultsAndValidate() []string {
	var warnings []string

	format := strings.ToUpper(strings.TrimSpace(c.Converter.TargetFormat))
	switch format {
	case FormatJPEG, FormatHEIF:
		c.Converter.TargetFormat = format
	default:
		warnings = append(warnings, fmt.Sprintf(
			"invalid target_format %q; falling back to %s", c.Converter.TargetFormat, defaultFormat))
		c.Converter.TargetFormat = defaultFormat
	}

	if c.Converter.Quality < 1 || c.Converter.Quality > 100 {
		warnings = append(warnings, fmt.Sprintf(
			"invalid quality %d (must be 1-100); falling back to %d", c.Converter.Quality, fallbackQuality))
		c.Converter.Quality = fallbackQuality
	}

	if c.Converter.MaxWorkers < 1 {
		warnings = append(warnings, fmt.Sprintf(
			"invalid max_workers %d (must be >= 1); falling back to %d", c.Converter.MaxWorkers, defaultWorkers))
		c.Converter.MaxWorkers = defaultWorkers
	}

	action := strings.ToLower(strings.TrimSpace(c.FileManagement.PostAction))
	switch action {
	case ActionReplace, ActionOutputFolder:
		c.FileManagement.PostAction = action
	default:
		warnings = append(warnings, fmt.Sprintf(
			"invalid post_action %q; falling back to %s", c.FileManagement.PostAction, defaultAction))
		c.FileManagement.PostAction = defaultAction
	}

	if c.FileManagement.PostAction == ActionOutputFolder &&
		strings.TrimSpace(c.FileManagement.OutputDirectory) == "" {
		warnings = append(warnings,
			"post_action is output_folder but output_directory is empty; falling back to replace")
		c.FileManagement.PostAction = ActionReplace
	}

	// watch_directory is intentionally left as-is when empty. There is no safe
	// directory to guess on the user's behalf, so an empty value is treated as
	// "not configured" and the caller (main) refuses to start.

	if c.Watcher.MaxDepth < 0 {
		c.Watcher.MaxDepth = 0
	}

	return warnings
}

// baseDefaults returns the shared default configuration without a quality value.
//
// watch_directory and output_directory are deliberately left empty: the program
// must not guess a folder to monitor or write into on the user's behalf. The
// user is required to set watch_directory before the program will run.
func baseDefaults() Config {
	return Config{
		Watcher: WatcherConfig{
			WatchDirectory: "",
			Enabled:        true,
			BatchOnStartup: true,
			Recursive:      true,
			MaxDepth:       0,
		},
		Converter: ConverterConfig{
			TargetFormat: FormatJPEG,
			MaxWorkers:   defaultWorkers,
		},
		FileManagement: FileManagementConfig{
			PostAction:      ActionReplace,
			OutputDirectory: "",
		},
	}
}

// defaultConfig returns the configuration written into a freshly generated file.
func defaultConfig() Config {
	c := baseDefaults()
	c.Converter.Quality = generatedQuality
	return c
}

// SafeDefaults returns a conservative configuration used when the config file
// cannot be read or parsed.
func SafeDefaults() Config {
	c := baseDefaults()
	c.Converter.Quality = fallbackQuality
	return c
}

// writeDefault writes a commented default configuration file to path.
func writeDefault(path string, cfg Config) error {
	content := fmt.Sprintf(`# Auto Image Converter Configuration File

# Watcher and Runtime Modes
watcher:
  # REQUIRED. Set this to the folder you want to monitor, then restart the program.
  # It is empty by default and the program will NOT run until you fill it in.
  # Example: "C:\\Users\\YourUsername\\Pictures\\Screenshots"
  watch_directory: "%s"
  enabled: true          # Enable real-time background monitoring
  batch_on_startup: true # Convert existing PNGs once at startup
  recursive: true        # Also watch subfolders
  max_depth: 0           # 0 = unlimited; N = at most N levels below the watch root

# Conversion Core Settings
converter:
  target_format: "JPEG"  # Target format. Supported: JPEG (no external tool), HEIF (needs Python + pillow-heif)
  quality: %d            # Image quality, range 1-100
  max_workers: %d         # Maximum concurrent workers for the startup batch

# Post-Conversion File Management
file_management:
  post_action: "replace" # Supported: replace (convert in place, delete original), output_folder (convert into output_directory, keep original)
  output_directory: "%s" # Used when post_action is "output_folder"
`,
		yamlPath(cfg.Watcher.WatchDirectory),
		cfg.Converter.Quality,
		cfg.Converter.MaxWorkers,
		yamlPath(cfg.FileManagement.OutputDirectory),
	)
	return os.WriteFile(path, []byte(content), 0o644)
}

// yamlPath escapes a Windows path for embedding inside a double-quoted YAML
// scalar by doubling backslashes.
func yamlPath(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}
