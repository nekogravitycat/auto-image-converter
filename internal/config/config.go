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
	"bytes"
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
//   - If the file cannot be read, or is not valid YAML at all, safe defaults are
//     returned together with a non-nil error (the caller should log it and
//     continue).
//   - Otherwise the file is parsed leniently: every value that can be read is
//     kept, and any setting that is missing, of the wrong type, unrecognized, or
//     out of range falls back to its default. When any such correction was
//     needed the file is rewritten in clean, complete, canonical form (valid
//     values preserved, defaults for the rest) and each correction is reported
//     as a warning.
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

	// Parse leniently into a pointer-field mirror so that a genuinely absent key
	// (nil) is distinguishable from one explicitly set to its zero value — which
	// matters for the boolean fields whose default (true) is not their zero value
	// (false). A type mismatch on an individual field yields a *yaml.TypeError,
	// leaves that field nil, and is recoverable; only a real syntax error, which
	// makes nothing readable, is fatal.
	var raw rawConfig
	if uErr := yaml.Unmarshal(data, &raw); uErr != nil {
		var te *yaml.TypeError
		if !errors.As(uErr, &te) {
			return SafeDefaults(), nil, fmt.Errorf("config at %s is corrupt: %w", path, uErr)
		}
	}

	var missing []string
	cfg, missing = raw.toConfig()
	for _, name := range missing {
		warnings = append(warnings, fmt.Sprintf("setting %s is missing or invalid; using default", name))
	}
	for _, name := range detectUnknownFields(data) {
		warnings = append(warnings, fmt.Sprintf("unrecognized setting %q; ignoring it", name))
	}

	warnings = append(warnings, cfg.applyDefaultsAndValidate()...)

	// If the file was missing keys, carried unrecognized keys, or held values we
	// had to correct, rewrite it in clean, complete, canonical form — preserving
	// every value we could read and filling the rest with defaults. The result is
	// idempotent: a subsequent load finds nothing to fix and does not rewrite.
	if len(warnings) > 0 {
		if wErr := writeDefault(path, cfg); wErr != nil {
			warnings = append(warnings, fmt.Sprintf("could not rewrite cleaned config at %s: %v", path, wErr))
		} else {
			warnings = append(warnings, fmt.Sprintf("rewrote %s with the valid values, defaults for the rest", path))
		}
	}
	return cfg, warnings, nil
}

// rawConfig mirrors Config but with pointer fields, so Load can tell a missing
// key (nil) apart from one present with a zero value. This distinction matters
// for the boolean watcher fields, whose default is true rather than their zero
// value of false.
type rawConfig struct {
	Watcher        rawWatcher        `yaml:"watcher"`
	Converter      rawConverter      `yaml:"converter"`
	FileManagement rawFileManagement `yaml:"file_management"`
}

type rawWatcher struct {
	WatchDirectory *string `yaml:"watch_directory"`
	Enabled        *bool   `yaml:"enabled"`
	BatchOnStartup *bool   `yaml:"batch_on_startup"`
	Recursive      *bool   `yaml:"recursive"`
	MaxDepth       *int    `yaml:"max_depth"`
}

type rawConverter struct {
	TargetFormat *string `yaml:"target_format"`
	Quality      *int    `yaml:"quality"`
	MaxWorkers   *int    `yaml:"max_workers"`
}

type rawFileManagement struct {
	PostAction      *string `yaml:"post_action"`
	OutputDirectory *string `yaml:"output_directory"`
}

// toConfig converts the leniently-parsed raw form into a full Config, filling
// every absent (or wrong-typed, hence nil) field from the defaults and returning
// the dotted names of the fields it had to fill. Range/enum validation of the
// values that *were* present is left to applyDefaultsAndValidate.
func (r rawConfig) toConfig() (Config, []string) {
	cfg := defaultConfig()
	var missing []string

	set := func(present bool, name string) bool {
		if present {
			return true
		}
		missing = append(missing, name)
		return false
	}

	if set(r.Watcher.WatchDirectory != nil, "watcher.watch_directory") {
		cfg.Watcher.WatchDirectory = *r.Watcher.WatchDirectory
	}
	if set(r.Watcher.Enabled != nil, "watcher.enabled") {
		cfg.Watcher.Enabled = *r.Watcher.Enabled
	}
	if set(r.Watcher.BatchOnStartup != nil, "watcher.batch_on_startup") {
		cfg.Watcher.BatchOnStartup = *r.Watcher.BatchOnStartup
	}
	if set(r.Watcher.Recursive != nil, "watcher.recursive") {
		cfg.Watcher.Recursive = *r.Watcher.Recursive
	}
	if set(r.Watcher.MaxDepth != nil, "watcher.max_depth") {
		cfg.Watcher.MaxDepth = *r.Watcher.MaxDepth
	}

	if set(r.Converter.TargetFormat != nil, "converter.target_format") {
		cfg.Converter.TargetFormat = *r.Converter.TargetFormat
	}
	if set(r.Converter.Quality != nil, "converter.quality") {
		cfg.Converter.Quality = *r.Converter.Quality
	}
	if set(r.Converter.MaxWorkers != nil, "converter.max_workers") {
		cfg.Converter.MaxWorkers = *r.Converter.MaxWorkers
	}

	if set(r.FileManagement.PostAction != nil, "file_management.post_action") {
		cfg.FileManagement.PostAction = *r.FileManagement.PostAction
	}
	if set(r.FileManagement.OutputDirectory != nil, "file_management.output_directory") {
		cfg.FileManagement.OutputDirectory = *r.FileManagement.OutputDirectory
	}

	return cfg, missing
}

// detectUnknownFields reports the names of keys in the YAML that do not
// correspond to any known setting. It is best-effort and never affects the
// values already parsed — only the warnings and the decision to rewrite. It is
// called only after the lenient parse has confirmed the YAML is syntactically
// valid, so the strict decoder here can only surface field-level problems.
func detectUnknownFields(data []byte) []string {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var raw rawConfig
	var te *yaml.TypeError
	if err := dec.Decode(&raw); !errors.As(err, &te) {
		return nil
	}

	var fields []string
	for _, msg := range te.Errors {
		// KnownFields violations read "line N: field NAME not found in type ...";
		// type-mismatch errors (already handled as missing) are ignored here.
		if strings.Contains(msg, "not found in type") {
			fields = append(fields, unknownFieldName(msg))
		}
	}
	return fields
}

// unknownFieldName extracts the offending key from a yaml KnownFields error
// message of the form "line N: field NAME not found in type ...".
func unknownFieldName(msg string) string {
	const pre, post = "field ", " not found"
	i := strings.Index(msg, pre)
	j := strings.Index(msg, post)
	if i >= 0 && j > i+len(pre) {
		return msg[i+len(pre) : j]
	}
	return strings.TrimSpace(msg)
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

// writeDefault writes cfg to path as a commented YAML file. It is used both to
// generate the initial config and to rewrite a partially-invalid one, so it
// serializes every field from cfg rather than assuming defaults — otherwise a
// rewrite would silently reset values the user had set correctly.
func writeDefault(path string, cfg Config) error {
	return os.WriteFile(path, []byte(renderConfig(cfg)), 0o644)
}

// renderConfig produces the commented config-file contents for cfg, preserving
// all of its values.
func renderConfig(cfg Config) string {
	return fmt.Sprintf(`# Auto Image Converter Configuration File

# Watcher and Runtime Modes
watcher:
  # REQUIRED. Set this to the folder you want to monitor, then restart the program.
  # It is empty by default and the program will NOT run until you fill it in.
  # Example: "C:\\Users\\YourUsername\\Pictures\\Screenshots"
  watch_directory: "%s"
  enabled: %t          # Enable real-time background monitoring
  batch_on_startup: %t # Convert existing PNGs once at startup
  recursive: %t        # Also watch subfolders
  max_depth: %d           # 0 = unlimited; N = at most N levels below the watch root

# Conversion Core Settings
converter:
  target_format: "%s"  # Target format. Supported: JPEG (no external tool), HEIF (needs Python + pillow-heif)
  quality: %d            # Image quality, range 1-100
  max_workers: %d         # Maximum concurrent workers for the startup batch

# Post-Conversion File Management
file_management:
  post_action: "%s" # Supported: replace (convert in place, delete original), output_folder (convert into output_directory, keep original)
  output_directory: "%s" # Used when post_action is "output_folder"
`,
		yamlPath(cfg.Watcher.WatchDirectory),
		cfg.Watcher.Enabled,
		cfg.Watcher.BatchOnStartup,
		cfg.Watcher.Recursive,
		cfg.Watcher.MaxDepth,
		cfg.Converter.TargetFormat,
		cfg.Converter.Quality,
		cfg.Converter.MaxWorkers,
		cfg.FileManagement.PostAction,
		yamlPath(cfg.FileManagement.OutputDirectory),
	)
}

// yamlPath escapes a Windows path for embedding inside a double-quoted YAML
// scalar by doubling backslashes.
func yamlPath(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}
