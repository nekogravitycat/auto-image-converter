package convert

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
)

// outputPath computes the preferred destination path for a converted source file
// and ensures the containing directory exists. The name is not yet reserved —
// see claimOutputName, which resolves collisions at the moment the output is
// ready to be put in place.
//
//   - In replace mode the output sits next to the source.
//   - In output_folder mode the source's subpath relative to the watch root is
//     mirrored under the output directory, preserving nested structure.
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

	return filepath.Join(destDir, fileName), nil
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

// maxNameAttempts bounds the search for a free output name, so a pathological
// directory cannot spin forever.
const maxNameAttempts = 10000

// claimOutputName reserves an unused output name at or near desired and returns
// it. It creates the file exclusively (O_CREATE|O_EXCL) rather than testing for
// absence first: two conversions racing for the same name must not both conclude
// it is free, which is exactly what happens when distinct sources share a base
// name and a destination directory. The winner keeps `desired`; the loser moves
// on to "-1", "-2", and so on.
//
// The reservation is an empty file. The caller renames the finished output over
// it (atomic on Windows) or, on failure, removes it.
func claimOutputName(desired string) (string, error) {
	ext := filepath.Ext(desired)
	stem := strings.TrimSuffix(desired, ext)

	for i := 0; i < maxNameAttempts; i++ {
		candidate := desired
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return candidate, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("could not create output %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("could not find an unused output name for %s after %d attempts", desired, maxNameAttempts)
}
