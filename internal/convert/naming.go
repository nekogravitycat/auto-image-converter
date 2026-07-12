package convert

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
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
func (s JobSpec) outputPath(srcPath string) (string, error) {
	ext := s.OutputExtension()
	stem := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	fileName := stem + ext

	var destDir string
	if s.PostAction == config.ActionOutputFolder {
		destDir = s.mirroredDir(srcPath)
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
func (s JobSpec) mirroredDir(srcPath string) string {
	srcDir := filepath.Dir(fsutil.AbsClean(srcPath))
	rel, err := filepath.Rel(s.WatchDir, srcDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return s.OutputDir
	}
	return filepath.Join(s.OutputDir, rel)
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
