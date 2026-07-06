package convert

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
)

// outputPath computes the destination path for a converted source file and
// ensures the containing directory exists.
//
//   - In replace mode the output sits next to the source.
//   - In output_folder mode the source's subpath relative to the watch root is
//     mirrored under the output directory, preserving nested structure.
//
// The returned path is guaranteed not to collide with an existing file: if the
// natural name is taken, a numeric suffix is appended (e.g. shot-1.jpg).
func (c *Converter) outputPath(srcPath string) (string, error) {
	ext := c.cfg.OutputExtension()
	stem := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	fileName := stem + ext

	var destDir string
	if c.cfg.FileManagement.PostAction == config.ActionOutputFolder {
		destDir = c.mirroredDir(srcPath)
	} else {
		destDir = filepath.Dir(srcPath)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("could not create output directory %s: %w", destDir, err)
	}

	return ensureUnique(filepath.Join(destDir, fileName)), nil
}

// mirroredDir returns the directory under the output root that mirrors the
// source file's location relative to the watch root. Sources outside the watch
// root are placed directly in the output root.
func (c *Converter) mirroredDir(srcPath string) string {
	srcDir := filepath.Dir(absClean(srcPath))
	rel, err := filepath.Rel(c.watchRoot, srcDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return c.outputRoot
	}
	return filepath.Join(c.outputRoot, rel)
}

// ensureUnique returns path unchanged if no file exists there; otherwise it
// appends "-1", "-2", ... before the extension until an unused name is found.
func ensureUnique(path string) string {
	if !fileExists(path) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !fileExists(candidate) {
			return candidate
		}
	}
}

// fileExists reports whether a file or directory exists at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// absClean returns a cleaned absolute form of path, falling back to Clean when
// the absolute path cannot be determined.
func absClean(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
