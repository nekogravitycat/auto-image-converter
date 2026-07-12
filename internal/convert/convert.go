// Package convert performs PNG-to-JPEG/HEIF conversion and applies the
// configured post-conversion action.
//
// Safety is the central concern: the original PNG is only ever deleted after a
// conversion has been fully verified (output exists and is non-empty). Any
// failure leaves the original untouched and removes partial output.
//
// The package separates a shared Engine from a per-job JobSpec. One Engine holds
// the resources shared by every monitored folder — a single global worker
// semaphore and a single warm HEIF worker pool — so N folders converting at once
// never oversubscribe the CPU. A JobSpec carries one folder's own settings
// (format, quality, post-action, output location), so different folders convert
// independently.
package convert

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// tempSuffix is appended to the final output name while a conversion is in
// progress, so a partial or crashed encode never leaves a file at the final
// name.
const tempSuffix = ".converting.tmp"

// Engine holds the resources shared across all jobs: the global worker
// semaphore that bounds total concurrent conversions, and (when any job targets
// HEIF) the single warm Python worker pool. It is safe for concurrent use.
type Engine struct {
	log            *logx.Logger
	heifScriptPath string
	heifPool       *heifPool     // shared; nil when no job targets HEIF
	sem            chan struct{} // global shared worker pool, cap = maxWorkers
	// onResult, when set, is invoked once for every attempted conversion (both
	// success and failure), so a single hook can drive statistics and the UI
	// activity feed. It must be set before any job starts and be safe for
	// concurrent use.
	onResult func(Result, error)
}

// SetResultHook registers a callback invoked after each conversion attempt. Call
// it once, before starting any jobs; the hook must be safe for concurrent use.
func (e *Engine) SetResultHook(fn func(Result, error)) {
	e.onResult = fn
}

// NewEngine creates the shared conversion engine. maxWorkers sizes both the
// global worker semaphore and (when usesHEIF) the HEIF worker pool. When any job
// targets HEIF, a pool of warm Python workers is prepared so each conversion
// reuses an already-imported interpreter instead of paying the startup cost per
// file. Callers must invoke Close to shut the pool down.
func NewEngine(maxWorkers int, usesHEIF bool, log *logx.Logger, heifScriptPath string) *Engine {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	e := &Engine{
		log:            log,
		heifScriptPath: heifScriptPath,
		sem:            make(chan struct{}, maxWorkers),
	}
	if usesHEIF {
		e.heifPool = newHeifPool(heifScriptPath, log, maxWorkers)
	}
	return e
}

// Close releases resources held by the Engine, shutting down any HEIF worker
// processes. It is safe to call even when no pool was created, and more than
// once.
func (e *Engine) Close() {
	if e.heifPool != nil {
		e.heifPool.Close()
	}
}

// Acquire takes a slot in the global worker pool, blocking until one is free.
// Release returns it. Callers (watch and batch) hold a slot only for the
// duration of an actual encode, so slow-to-write files never starve the pool.
func (e *Engine) Acquire() { e.sem <- struct{}{} }

// Release returns a slot taken by Acquire.
func (e *Engine) Release() { <-e.sem }

// ValidateHEIF checks that external dependencies required for HEIF output are
// available, but only when at least one job targets HEIF (i.e. the pool exists).
// When HEIF is not in use it is a no-op.
func (e *Engine) ValidateHEIF() error {
	if e.heifPool == nil {
		return nil
	}
	if err := e.checkHEIFEnvironment(); err != nil {
		return fmt.Errorf("a job targets HEIF but the HEIF conversion environment is not ready: %w", err)
	}
	return nil
}

// Result describes a completed conversion, for logging and statistics.
type Result struct {
	Src           string
	Dst           string
	OriginalBytes int64
	OutputBytes   int64
	Replaced      bool // whether the original was deleted (replace mode)
}

// JobSpec is one folder's conversion settings, derived from a config.JobConfig.
// It is what Convert and the traversal helpers need, decoupled from the file
// format of the configuration.
type JobSpec struct {
	Name         string
	WatchDir     string // absolute, cleaned
	OutputDir    string // absolute, cleaned (used only in output_folder mode)
	TargetFormat string
	Quality      int
	PostAction   string
	Recursive    bool
	MaxDepth     int
}

// SpecFromJob builds a JobSpec from a validated JobConfig, resolving directories
// to absolute cleaned form and normalizing the enum casing the rest of the
// package expects.
func SpecFromJob(j config.JobConfig) JobSpec {
	return JobSpec{
		Name:         j.Name,
		WatchDir:     fsutil.AbsClean(j.WatchDirectory),
		OutputDir:    fsutil.AbsClean(j.OutputDirectory),
		TargetFormat: strings.ToUpper(strings.TrimSpace(j.TargetFormat)),
		Quality:      j.Quality,
		PostAction:   strings.ToLower(strings.TrimSpace(j.PostAction)),
		Recursive:    j.Recursive,
		MaxDepth:     j.MaxDepth,
	}
}

// OutputExtension returns the destination file extension for this spec's format.
func (s JobSpec) OutputExtension() string {
	if s.TargetFormat == config.FormatHEIF {
		return ".heic"
	}
	return ".jpg"
}

// IgnoredDir returns the absolute output directory that must be excluded from
// watching and scanning to prevent conversion loops, and whether such exclusion
// applies. Exclusion applies only in output_folder mode when the output
// directory lies within the watch root.
func (s JobSpec) IgnoredDir() (string, bool) {
	if s.PostAction != config.ActionOutputFolder {
		return "", false
	}
	rel, err := filepath.Rel(s.WatchDir, s.OutputDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false // output root is outside the watch root; nothing to exclude
	}
	return s.OutputDir, true
}

// TraversalRules returns the directory-traversal scope shared by the watcher,
// the startup batch, and the temp sweep for this spec: the watch root, the
// recursion/depth settings, and the output subtree to exclude (when applicable).
func (s JobSpec) TraversalRules() fsutil.TraversalRules {
	ignoredDir, hasIgnored := s.IgnoredDir()
	return fsutil.TraversalRules{
		Root:       s.WatchDir,
		Recursive:  s.Recursive,
		MaxDepth:   s.MaxDepth,
		IgnoredDir: ignoredDir,
		HasIgnored: hasIgnored,
	}
}

// IsPNG reports whether path has a .png extension (case-insensitive).
func IsPNG(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".png")
}

// IsTempFile reports whether path is an in-progress conversion temp file — one
// this program writes while encoding and renames away once the output is
// verified. A leftover temp means a previous run was killed mid-conversion; it
// is always safe to delete, because the original PNG is only removed after a
// successful rename, so the conversion simply gets redone. See batch.SweepTemps.
func IsTempFile(path string) bool {
	return strings.HasSuffix(path, tempSuffix)
}

// Convert converts a single PNG file according to spec and applies the
// post-conversion action.
//
// Non-PNG paths are ignored (a zero Result and nil error). On any failure the
// original file is left untouched and any partial output is removed. Convert
// does not itself take a worker slot; callers bound concurrency via
// Engine.Acquire/Release. Every attempt that does real work (a PNG source) is
// reported through the result hook, if one is set.
func (e *Engine) Convert(spec JobSpec, srcPath string) (Result, error) {
	res, err := e.convert(spec, srcPath)
	if e.onResult != nil && (res.Dst != "" || err != nil) {
		e.onResult(res, err)
	}
	return res, err
}

// convert performs the actual conversion work; Convert wraps it to fire the
// result hook.
func (e *Engine) convert(spec JobSpec, srcPath string) (Result, error) {
	if !IsPNG(srcPath) {
		return Result{}, nil
	}

	var origSize int64
	if info, err := os.Stat(srcPath); err == nil {
		origSize = info.Size()
	}

	finalPath, err := spec.outputPath(srcPath)
	if err != nil {
		return Result{Src: srcPath}, err
	}
	tmpPath := finalPath + tempSuffix

	if err := e.encodeTo(spec, srcPath, tmpPath); err != nil {
		removeQuietly(tmpPath)
		return Result{Src: srcPath}, err
	}

	// Verify the output exists and is non-empty before touching the original.
	info, err := os.Stat(tmpPath)
	if err != nil || info.Size() == 0 {
		removeQuietly(tmpPath)
		return Result{Src: srcPath}, fmt.Errorf("conversion produced no valid output for %s", srcPath)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		removeQuietly(tmpPath)
		return Result{Src: srcPath}, fmt.Errorf("could not finalize output %s: %w", finalPath, err)
	}

	e.log.Infof("converted %s -> %s (%d bytes)", srcPath, finalPath, info.Size())

	replaced := e.applyPostAction(spec, srcPath)
	return Result{
		Src:           srcPath,
		Dst:           finalPath,
		OriginalBytes: origSize,
		OutputBytes:   info.Size(),
		Replaced:      replaced,
	}, nil
}

// encodeTo dispatches to the encoder for the spec's target format, writing the
// result to dstPath.
func (e *Engine) encodeTo(spec JobSpec, srcPath, dstPath string) error {
	switch spec.TargetFormat {
	case config.FormatHEIF:
		return e.encodeHEIFFile(srcPath, dstPath, spec.Quality)
	default:
		return e.encodeJPEGFile(srcPath, dstPath, spec.Quality)
	}
}

// encodeJPEGFile decodes the source PNG, carries over its EXIF (best-effort),
// and writes a JPEG to dstPath at the given quality.
func (e *Engine) encodeJPEGFile(srcPath, dstPath string, quality int) error {
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
		e.log.Warnf("could not read EXIF from %s: %v", srcPath, err)
	}

	data, exifWarning := encodeJPEG(img, quality, exif)
	if exifWarning != nil {
		e.log.Warnf("EXIF not embedded for %s: %v", srcPath, exifWarning)
	}

	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", dstPath, err)
	}
	return nil
}

// applyPostAction performs the configured action on the original file after a
// verified successful conversion, returning whether the original was deleted.
// Failures here are logged but do not undo the successful conversion.
func (e *Engine) applyPostAction(spec JobSpec, srcPath string) bool {
	if spec.PostAction != config.ActionReplace {
		return false // output_folder mode keeps the original in place
	}
	if err := os.Remove(srcPath); err != nil {
		e.log.Warnf("converted but could not delete original %s: %v", srcPath, err)
		return false
	}
	e.log.Infof("deleted original %s", srcPath)
	return true
}

// removeQuietly deletes path, ignoring the error (used for cleaning up partial
// output). A missing file is not an error.
func removeQuietly(path string) {
	_ = os.Remove(path)
}
