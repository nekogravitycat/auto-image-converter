// Package paths resolves application file locations relative to the running
// executable rather than the current working directory.
//
// The application is intended to run as a Windows background utility launched
// from a startup shortcut. In that situation the current working directory can
// default to something like C:\Windows\System32, so any relative path such as
// "./config.yml" would resolve to the wrong location. All application files are
// therefore anchored to the directory that contains the executable.
package paths

import (
	"os"
	"path/filepath"
)

// Names of the standard files and directories, relative to the base directory.
const (
	ConfigFileName = "config.yml"
	ToolsDirName   = "tools"
	LogFileName    = "auto-image-converter.log"
	// HeifScriptName is the bundled Python script that performs HEIF encoding
	// via pillow-heif (invoked only when HEIF output is selected).
	HeifScriptName = "heif_convert.py"
)

// Paths holds absolute locations for the application's files, all anchored to
// the directory that contains the executable.
type Paths struct {
	// BaseDir is the directory containing the running executable.
	BaseDir string
	// ConfigFile is the absolute path to config.yml.
	ConfigFile string
	// ToolsDir is the absolute path to the bundled sidecar tools directory.
	ToolsDir string
	// LogFile is the absolute path to the log file.
	LogFile string
}

// Resolve computes the application paths from the location of the executable.
//
// When os.Executable() fails (which should be rare), it falls back to the
// current working directory so the program can still start instead of crashing.
func Resolve() Paths {
	base := baseDir()
	return Paths{
		BaseDir:    base,
		ConfigFile: filepath.Join(base, ConfigFileName),
		ToolsDir:   filepath.Join(base, ToolsDirName),
		LogFile:    filepath.Join(base, LogFileName),
	}
}

// HeifScriptPath returns the absolute path to the bundled HEIF conversion
// script.
func (p Paths) HeifScriptPath() string {
	return filepath.Join(p.ToolsDir, HeifScriptName)
}

// baseDir returns the directory containing the executable, falling back to the
// working directory if the executable path cannot be determined.
func baseDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			return wd
		}
		return "."
	}
	// Resolve symlinks so the base directory is stable even when the executable
	// is launched through a link. Ignore errors and fall back to the raw path.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
