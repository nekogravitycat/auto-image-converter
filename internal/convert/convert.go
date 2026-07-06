// Package convert performs PNG-to-JPEG/HEIF conversion and applies the
// configured post-conversion action.
//
// Safety is the central concern: the original PNG is only ever deleted after a
// conversion has been fully verified (output exists and is non-empty). Any
// failure leaves the original untouched and removes partial output.
package convert

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// tempSuffix is appended to the final output name while a conversion is in
// progress, so a partial or crashed encode never leaves a file at the final
// name.
const tempSuffix = ".converting.tmp"

// Converter converts PNG files according to the application configuration.
type Converter struct {
	cfg            config.Config
	log            *logx.Logger
	heifScriptPath string
	heifPool       *heifPool
	watchRoot      string
	outputRoot     string
}

// New creates a Converter. heifScriptPath is the absolute path to the bundled
// HEIF conversion script, used only when HEIF output is selected.
//
// When HEIF output is configured, a pool of warm Python workers is prepared so
// each conversion reuses an already-imported interpreter instead of paying the
// startup cost per file. Callers must invoke Close to shut the pool down.
func New(cfg config.Config, log *logx.Logger, heifScriptPath string) *Converter {
	c := &Converter{
		cfg:            cfg,
		log:            log,
		heifScriptPath: heifScriptPath,
		watchRoot:      absClean(cfg.Watcher.WatchDirectory),
		outputRoot:     absClean(cfg.FileManagement.OutputDirectory),
	}
	if cfg.Converter.TargetFormat == config.FormatHEIF {
		c.heifPool = newHeifPool(heifScriptPath, log, cfg.Converter.MaxWorkers)
	}
	return c
}

// Close releases resources held by the Converter, shutting down any HEIF worker
// processes. It is safe to call even when no pool was created, and more than
// once.
func (c *Converter) Close() {
	if c.heifPool != nil {
		c.heifPool.Close()
	}
}

// ValidateEnvironment checks that external dependencies required by the current
// configuration are available. When HEIF is selected it confirms that a Python
// interpreter, the bundled script, and a working pillow-heif encoder are all
// present; otherwise it returns a descriptive error.
func (c *Converter) ValidateEnvironment() error {
	if c.cfg.Converter.TargetFormat == config.FormatHEIF {
		if err := c.checkHEIFEnvironment(); err != nil {
			return fmt.Errorf("target_format is HEIF but the HEIF conversion environment is not ready: %w", err)
		}
	}
	return nil
}

// IgnoredDir returns the absolute output directory that must be excluded from
// watching and scanning to prevent conversion loops, and whether such exclusion
// applies. Exclusion applies only in output_folder mode when the output
// directory lies within the watch root.
func (c *Converter) IgnoredDir() (string, bool) {
	if c.cfg.FileManagement.PostAction != config.ActionOutputFolder {
		return "", false
	}
	rel, err := filepath.Rel(c.watchRoot, c.outputRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false // output root is outside the watch root; nothing to exclude
	}
	return c.outputRoot, true
}

// IsPNG reports whether path has a .png extension (case-insensitive).
func IsPNG(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".png")
}

// Convert converts a single PNG file and applies the post-conversion action.
//
// Non-PNG paths are ignored. On any failure the original file is left untouched
// and any partial output is removed.
func (c *Converter) Convert(srcPath string) error {
	if !IsPNG(srcPath) {
		return nil
	}

	finalPath, err := c.outputPath(srcPath)
	if err != nil {
		return err
	}
	tmpPath := finalPath + tempSuffix

	if err := c.encodeTo(srcPath, tmpPath); err != nil {
		removeQuietly(tmpPath)
		return err
	}

	// Verify the output exists and is non-empty before touching the original.
	info, err := os.Stat(tmpPath)
	if err != nil || info.Size() == 0 {
		removeQuietly(tmpPath)
		return fmt.Errorf("conversion produced no valid output for %s", srcPath)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		removeQuietly(tmpPath)
		return fmt.Errorf("could not finalize output %s: %w", finalPath, err)
	}

	c.log.Infof("converted %s -> %s (%d bytes)", srcPath, finalPath, info.Size())

	c.applyPostAction(srcPath)
	return nil
}

// encodeTo dispatches to the encoder for the configured target format, writing
// the result to dstPath.
func (c *Converter) encodeTo(srcPath, dstPath string) error {
	switch c.cfg.Converter.TargetFormat {
	case config.FormatHEIF:
		return c.encodeHEIFFile(srcPath, dstPath)
	default:
		return c.encodeJPEGFile(srcPath, dstPath)
	}
}

// encodeJPEGFile decodes the source PNG, carries over its EXIF (best-effort),
// and writes a JPEG to dstPath.
func (c *Converter) encodeJPEGFile(srcPath, dstPath string) error {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("could not read %s: %w", srcPath, err)
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("could not decode PNG %s: %w", srcPath, err)
	}

	exif, err := extractPNGExif(raw)
	if err != nil {
		c.log.Warnf("could not read EXIF from %s: %v", srcPath, err)
	}

	data, exifWarning := encodeJPEG(img, c.cfg.Converter.Quality, exif)
	if exifWarning != nil {
		c.log.Warnf("EXIF not embedded for %s: %v", srcPath, exifWarning)
	}

	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", dstPath, err)
	}
	return nil
}

// applyPostAction performs the configured action on the original file after a
// verified successful conversion. Failures here are logged but do not undo the
// successful conversion.
func (c *Converter) applyPostAction(srcPath string) {
	if c.cfg.FileManagement.PostAction != config.ActionReplace {
		return // output_folder mode keeps the original in place
	}
	if err := os.Remove(srcPath); err != nil {
		c.log.Warnf("converted but could not delete original %s: %v", srcPath, err)
		return
	}
	c.log.Infof("deleted original %s", srcPath)
}

// removeQuietly deletes path, ignoring the error (used for cleaning up partial
// output). A missing file is not an error.
func removeQuietly(path string) {
	_ = os.Remove(path)
}
