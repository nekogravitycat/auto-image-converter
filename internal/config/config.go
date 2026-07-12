// Package config loads, validates, and (when absent) generates the application
// configuration file (config.yml).
//
// The design goal is resilience: a missing, corrupt, or partially invalid
// configuration must never crash the application. Missing files are generated
// with sensible defaults; unreadable or malformed files fall back to safe
// defaults; individual invalid fields are corrected in place and reported as
// warnings so the caller can log them.
//
// The configuration is organized as a global "app" block plus a list of
// independent monitored-folder "jobs", each carrying its own conversion
// settings. The list is normally managed through the UI, but hand-edits remain
// valid and are re-read (and cleaned) on the next start.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// currentVersion is the schema version written into generated files. It exists
// so future format changes can be detected; there is no legacy migration.
const currentVersion = 1

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

// Supported job run modes.
const (
	// ModeMonitor watches the folder continuously and converts new files as
	// they appear (optionally converting existing files once at startup).
	ModeMonitor = "monitor"
	// ModeOnce converts the files already present and then stops; it never
	// installs a watcher. Useful for a one-off pass over a folder.
	ModeOnce = "once"
)

// Default and safe-fallback values.
const (
	defaultFormat    = FormatJPEG
	fallbackQuality  = 80 // quality used when the configured value is invalid
	generatedQuality = 90 // quality written into a freshly generated job
	defaultWorkers   = 8  // global shared worker pool size
	defaultAction    = ActionReplace
	defaultMode      = ModeMonitor
)

// Config is the full application configuration, mirroring config.yml.
type Config struct {
	Version int         `yaml:"version"`
	App     AppConfig   `yaml:"app"`
	Jobs    []JobConfig `yaml:"jobs"`
}

// AppConfig holds global settings shared by every job.
type AppConfig struct {
	// MaxWorkers is the size of the single worker pool shared across all jobs.
	MaxWorkers     int  `yaml:"max_workers"`
	StartMinimized bool `yaml:"start_minimized"`
	// Autostart mirrors the Windows startup registry entry; the UI keeps the two
	// in sync when the checkbox is toggled.
	Autostart bool `yaml:"autostart"`
}

// JobConfig describes one monitored folder and how its files are converted. All
// conversion settings are per-job, so different folders can use different
// formats, qualities, and post-actions.
type JobConfig struct {
	Name           string `yaml:"name"`
	WatchDirectory string `yaml:"watch_directory"`
	Enabled        bool   `yaml:"enabled"`
	Mode           string `yaml:"mode"`
	BatchOnStartup bool   `yaml:"batch_on_startup"`
	Recursive      bool   `yaml:"recursive"`
	// MaxDepth limits recursion: 0 means unlimited, N means at most N levels
	// below the watch root.
	MaxDepth        int    `yaml:"max_depth"`
	TargetFormat    string `yaml:"target_format"`
	Quality         int    `yaml:"quality"`
	PostAction      string `yaml:"post_action"`
	OutputDirectory string `yaml:"output_directory"`
}

// OutputExtension returns the file extension (including the dot) for the job's
// target format.
func (j JobConfig) OutputExtension() string {
	if strings.ToUpper(j.TargetFormat) == FormatHEIF {
		return ".heic"
	}
	return ".jpg"
}

// DefaultJob returns sensible defaults for a brand-new job created through the
// UI (before the user fills in a folder).
func DefaultJob() JobConfig {
	return defaultJob()
}

// NormalizeJob fills any missing/invalid fields of a job with defaults and
// returns the cleaned job together with a list of the corrections made. The UI
// calls it before saving a job the user just edited.
func NormalizeJob(j JobConfig) (JobConfig, []string) {
	warnings := j.applyDefaultsAndValidate(0)
	return j, warnings
}

// UsesHEIF reports whether any job targets HEIF, which decides whether the HEIF
// worker pool needs to be prepared.
func (c Config) UsesHEIF() bool {
	for _, j := range c.Jobs {
		if strings.ToUpper(j.TargetFormat) == FormatHEIF {
			return true
		}
	}
	return false
}

// Load reads the configuration from path.
//
// Behavior:
//   - If the file does not exist, a default file is generated at path (with an
//     empty job list) and the defaults are returned.
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

// Save writes cfg to path in clean, canonical form. The UI calls this whenever
// the user changes settings, keeping the file the single source of truth.
func Save(path string, cfg Config) error {
	return writeDefault(path, cfg)
}

// rawConfig mirrors Config but with pointer fields, so Load can tell a missing
// key (nil) apart from one present with a zero value. This distinction matters
// for the boolean fields whose default is true rather than their zero value of
// false.
type rawConfig struct {
	Version *int      `yaml:"version"`
	App     rawApp    `yaml:"app"`
	Jobs    []*rawJob `yaml:"jobs"`
}

type rawApp struct {
	MaxWorkers     *int  `yaml:"max_workers"`
	StartMinimized *bool `yaml:"start_minimized"`
	Autostart      *bool `yaml:"autostart"`
}

type rawJob struct {
	Name            *string `yaml:"name"`
	WatchDirectory  *string `yaml:"watch_directory"`
	Enabled         *bool   `yaml:"enabled"`
	Mode            *string `yaml:"mode"`
	BatchOnStartup  *bool   `yaml:"batch_on_startup"`
	Recursive       *bool   `yaml:"recursive"`
	MaxDepth        *int    `yaml:"max_depth"`
	TargetFormat    *string `yaml:"target_format"`
	Quality         *int    `yaml:"quality"`
	PostAction      *string `yaml:"post_action"`
	OutputDirectory *string `yaml:"output_directory"`
}

// toConfig converts the leniently-parsed raw form into a full Config, filling
// every absent (or wrong-typed, hence nil) field from the defaults and returning
// the dotted names of the fields it had to fill. Range/enum validation of the
// values that *were* present is left to applyDefaultsAndValidate.
func (r rawConfig) toConfig() (Config, []string) {
	cfg := baseDefaults()
	var missing []string

	// The version key is informational; a missing one is silently set to the
	// current version rather than reported, since it is not a user setting.
	if r.Version != nil {
		cfg.Version = *r.Version
	}

	fill := func(present bool, name string) bool {
		if present {
			return true
		}
		missing = append(missing, name)
		return false
	}

	if fill(r.App.MaxWorkers != nil, "app.max_workers") {
		cfg.App.MaxWorkers = *r.App.MaxWorkers
	}
	if fill(r.App.StartMinimized != nil, "app.start_minimized") {
		cfg.App.StartMinimized = *r.App.StartMinimized
	}
	if fill(r.App.Autostart != nil, "app.autostart") {
		cfg.App.Autostart = *r.App.Autostart
	}

	for i, rj := range r.Jobs {
		if rj == nil {
			continue
		}
		job := defaultJob()
		prefix := fmt.Sprintf("jobs[%d].", i)

		if fill(rj.Name != nil, prefix+"name") {
			job.Name = *rj.Name
		}
		// watch_directory is not reported as missing: an empty one simply means a
		// not-yet-configured job, which the manager surfaces at run time rather
		// than something to correct here.
		if rj.WatchDirectory != nil {
			job.WatchDirectory = *rj.WatchDirectory
		}
		if fill(rj.Enabled != nil, prefix+"enabled") {
			job.Enabled = *rj.Enabled
		}
		if fill(rj.Mode != nil, prefix+"mode") {
			job.Mode = *rj.Mode
		}
		if fill(rj.BatchOnStartup != nil, prefix+"batch_on_startup") {
			job.BatchOnStartup = *rj.BatchOnStartup
		}
		if fill(rj.Recursive != nil, prefix+"recursive") {
			job.Recursive = *rj.Recursive
		}
		if fill(rj.MaxDepth != nil, prefix+"max_depth") {
			job.MaxDepth = *rj.MaxDepth
		}
		if fill(rj.TargetFormat != nil, prefix+"target_format") {
			job.TargetFormat = *rj.TargetFormat
		}
		if fill(rj.Quality != nil, prefix+"quality") {
			job.Quality = *rj.Quality
		}
		if fill(rj.PostAction != nil, prefix+"post_action") {
			job.PostAction = *rj.PostAction
		}
		if fill(rj.OutputDirectory != nil, prefix+"output_directory") {
			job.OutputDirectory = *rj.OutputDirectory
		}

		cfg.Jobs = append(cfg.Jobs, job)
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

// applyDefaultsAndValidate corrects invalid fields in place and returns a list
// of human-readable warnings describing each correction.
func (c *Config) applyDefaultsAndValidate() []string {
	var warnings []string

	if c.Version == 0 {
		c.Version = currentVersion
	}

	if c.App.MaxWorkers < 1 {
		warnings = append(warnings, fmt.Sprintf(
			"invalid app.max_workers %d (must be >= 1); falling back to %d", c.App.MaxWorkers, defaultWorkers))
		c.App.MaxWorkers = defaultWorkers
	}

	for i := range c.Jobs {
		warnings = append(warnings, c.Jobs[i].applyDefaultsAndValidate(i)...)
	}

	return warnings
}

// applyDefaultsAndValidate corrects one job's invalid fields in place, returning
// a warning for each correction. index is used both for messages and to derive a
// stable fallback name so a rewrite stays idempotent.
func (j *JobConfig) applyDefaultsAndValidate(index int) []string {
	var warnings []string
	label := fmt.Sprintf("jobs[%d]", index)

	if strings.TrimSpace(j.Name) == "" {
		derived := deriveJobName(j.WatchDirectory, index)
		warnings = append(warnings, fmt.Sprintf("%s has no name; using %q", label, derived))
		j.Name = derived
	}

	mode := strings.ToLower(strings.TrimSpace(j.Mode))
	switch mode {
	case ModeMonitor, ModeOnce:
		j.Mode = mode
	default:
		warnings = append(warnings, fmt.Sprintf("%s: invalid mode %q; falling back to %s", label, j.Mode, defaultMode))
		j.Mode = defaultMode
	}

	format := strings.ToUpper(strings.TrimSpace(j.TargetFormat))
	switch format {
	case FormatJPEG, FormatHEIF:
		j.TargetFormat = format
	default:
		warnings = append(warnings, fmt.Sprintf("%s: invalid target_format %q; falling back to %s", label, j.TargetFormat, defaultFormat))
		j.TargetFormat = defaultFormat
	}

	if j.Quality < 1 || j.Quality > 100 {
		warnings = append(warnings, fmt.Sprintf("%s: invalid quality %d (must be 1-100); falling back to %d", label, j.Quality, fallbackQuality))
		j.Quality = fallbackQuality
	}

	action := strings.ToLower(strings.TrimSpace(j.PostAction))
	switch action {
	case ActionReplace, ActionOutputFolder:
		j.PostAction = action
	default:
		warnings = append(warnings, fmt.Sprintf("%s: invalid post_action %q; falling back to %s", label, j.PostAction, defaultAction))
		j.PostAction = defaultAction
	}

	if j.PostAction == ActionOutputFolder && strings.TrimSpace(j.OutputDirectory) == "" {
		warnings = append(warnings, fmt.Sprintf("%s: post_action is output_folder but output_directory is empty; falling back to replace", label))
		j.PostAction = ActionReplace
	}

	if j.MaxDepth < 0 {
		j.MaxDepth = 0
	}

	// watch_directory is intentionally left as-is when empty. There is no safe
	// folder to guess on the user's behalf, so an empty value is treated as
	// "not configured" and the manager reports it rather than the loader.

	return warnings
}

// deriveJobName produces a stable, human-readable name for a job that has none:
// the folder's base name when available, otherwise "job-N" (1-based). Stability
// keeps a rewrite idempotent — a subsequent load derives the same name and
// therefore finds nothing to fix.
func deriveJobName(watchDir string, index int) string {
	if trimmed := strings.TrimSpace(watchDir); trimmed != "" {
		if base := filepath.Base(filepath.Clean(trimmed)); base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return fmt.Sprintf("job-%d", index+1)
}

// baseDefaults returns the shared default configuration with an empty job list.
func baseDefaults() Config {
	return Config{
		Version: currentVersion,
		App: AppConfig{
			MaxWorkers:     defaultWorkers,
			StartMinimized: true,
			Autostart:      false,
		},
	}
}

// defaultJob returns the default settings for a single job, used to fill any
// fields absent from a partially-specified job in the file.
func defaultJob() JobConfig {
	return JobConfig{
		Enabled:        true,
		Mode:           defaultMode,
		BatchOnStartup: true,
		Recursive:      true,
		MaxDepth:       0,
		TargetFormat:   defaultFormat,
		Quality:        generatedQuality,
		PostAction:     defaultAction,
	}
}

// defaultConfig returns the configuration written into a freshly generated file:
// global defaults and no jobs (the user adds folders through the UI).
func defaultConfig() Config {
	return baseDefaults()
}

// SafeDefaults returns a conservative configuration used when the config file
// cannot be read or parsed.
func SafeDefaults() Config {
	return baseDefaults()
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
	var b strings.Builder
	b.WriteString("# Auto Image Converter Configuration File\n")
	b.WriteString("# Managed by the app UI; hand-edits are re-read (and cleaned) on the next start.\n\n")
	fmt.Fprintf(&b, "version: %d\n\n", cfg.Version)

	b.WriteString("# Global application settings.\n")
	b.WriteString("app:\n")
	fmt.Fprintf(&b, "  max_workers: %d       # Shared worker pool size across ALL folders\n", cfg.App.MaxWorkers)
	fmt.Fprintf(&b, "  start_minimized: %t # Start to the tray without opening the window\n", cfg.App.StartMinimized)
	fmt.Fprintf(&b, "  autostart: %t      # Launch at login (managed by the UI's checkbox)\n\n", cfg.App.Autostart)

	b.WriteString("# Monitored folders. Each entry is converted independently with its own\n")
	b.WriteString("# settings. Prefer adding or editing these from the app window.\n")
	if len(cfg.Jobs) == 0 {
		b.WriteString("jobs: []\n")
		return b.String()
	}
	b.WriteString("jobs:\n")
	for _, j := range cfg.Jobs {
		fmt.Fprintf(&b, "  - name: %q\n", j.Name)
		fmt.Fprintf(&b, "    watch_directory: \"%s\"\n", yamlPath(j.WatchDirectory))
		fmt.Fprintf(&b, "    enabled: %t\n", j.Enabled)
		fmt.Fprintf(&b, "    mode: %q            # monitor = watch continuously | once = convert existing files then stop\n", j.Mode)
		fmt.Fprintf(&b, "    batch_on_startup: %t\n", j.BatchOnStartup)
		fmt.Fprintf(&b, "    recursive: %t\n", j.Recursive)
		fmt.Fprintf(&b, "    max_depth: %d              # 0 = unlimited\n", j.MaxDepth)
		fmt.Fprintf(&b, "    target_format: %q     # JPEG | HEIF\n", j.TargetFormat)
		fmt.Fprintf(&b, "    quality: %d              # 1-100\n", j.Quality)
		fmt.Fprintf(&b, "    post_action: %q    # replace | output_folder\n", j.PostAction)
		fmt.Fprintf(&b, "    output_directory: \"%s\" # Used when post_action is output_folder\n", yamlPath(j.OutputDirectory))
	}
	return b.String()
}

// yamlPath escapes a Windows path for embedding inside a double-quoted YAML
// scalar by doubling backslashes.
func yamlPath(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}
