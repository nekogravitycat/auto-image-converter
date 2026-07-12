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
	"sync"
	"sync/atomic"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/fsutil"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// tempSuffix is appended to the final output name while a conversion is in
// progress, so a partial or crashed encode never leaves a file at the final
// name.
const tempSuffix = ".converting.tmp"

// Engine holds the resources shared across all jobs: the global worker
// semaphore that bounds total concurrent conversions, and the single warm Python
// worker pool used for HEIF output. It is safe for concurrent use.
type Engine struct {
	log            *logx.Logger
	heifScriptPath string
	maxWorkers     int
	sem            chan struct{} // global shared worker pool, cap = maxWorkers
	// onResult, when set, is invoked once for every attempted conversion (both
	// success and failure), so a single hook can drive statistics and the UI
	// activity feed. It must be set before any job starts and be safe for
	// concurrent use.
	onResult func(Result, error)

	// The HEIF pool is created on first use rather than up front, so that every
	// path that can ask for HEIF gets one — including an ad-hoc (drag-and-drop)
	// conversion, which is not a configured job and therefore cannot be predicted
	// when the engine is built.
	heifMu   sync.Mutex
	heifPool *heifPool
	closed   bool

	// tempSeq makes each in-progress temp file name unique, so two conversions
	// that target the same output name never write to the same temp path.
	tempSeq atomic.Uint64
}

// SetResultHook registers a callback invoked after each conversion attempt. Call
// it once, before starting any jobs; the hook must be safe for concurrent use.
func (e *Engine) SetResultHook(fn func(Result, error)) {
	e.onResult = fn
}

// NewEngine creates the shared conversion engine. maxWorkers sizes both the
// global worker semaphore and the HEIF worker pool. Callers must invoke Close to
// shut the pool down.
func NewEngine(maxWorkers int, log *logx.Logger, heifScriptPath string) *Engine {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &Engine{
		log:            log,
		heifScriptPath: heifScriptPath,
		maxWorkers:     maxWorkers,
		sem:            make(chan struct{}, maxWorkers),
	}
}

// heif returns the shared HEIF worker pool, creating it on first use. A pool of
// warm Python workers means each conversion reuses an already-imported
// interpreter instead of paying the startup cost per file.
func (e *Engine) heif() (*heifPool, error) {
	e.heifMu.Lock()
	defer e.heifMu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("cannot encode HEIF: the conversion engine is closed")
	}
	if e.heifPool == nil {
		e.heifPool = newHeifPool(e.heifScriptPath, e.log, e.maxWorkers)
	}
	return e.heifPool, nil
}

// WarmHEIF creates the HEIF worker pool ahead of the first conversion, so a job
// that is known to target HEIF does not pay the interpreter startup cost on its
// first file. It is optional: the pool is created on demand regardless.
func (e *Engine) WarmHEIF() {
	_, _ = e.heif()
}

// Close releases resources held by the Engine, shutting down any HEIF worker
// processes. It is safe to call even when no pool was created, and more than
// once. After Close, HEIF conversions fail rather than resurrecting the pool.
func (e *Engine) Close() {
	e.heifMu.Lock()
	pool := e.heifPool
	e.heifPool = nil
	e.closed = true
	e.heifMu.Unlock()

	if pool != nil {
		pool.Close()
	}
}

// Closed reports whether the engine has been closed. Owners use it to assert
// that an engine is not shut down while conversions are still running through it.
func (e *Engine) Closed() bool {
	e.heifMu.Lock()
	defer e.heifMu.Unlock()
	return e.closed
}

// Acquire takes a slot in the global worker pool, blocking until one is free.
// Release returns it. Callers (watch and batch) hold a slot only for the
// duration of an actual encode, so slow-to-write files never starve the pool.
func (e *Engine) Acquire() { e.sem <- struct{}{} }

// Release returns a slot taken by Acquire.
func (e *Engine) Release() { <-e.sem }

// ValidateHEIF checks that the external dependencies required for HEIF output
// (a Python interpreter, the bundled script, and a working pillow-heif) are
// available. Callers invoke it when something is known to target HEIF, so the
// cause is reported once at startup instead of once per failed screenshot.
func (e *Engine) ValidateHEIF() error {
	if err := checkHEIFEnvironment(e.heifScriptPath); err != nil {
		return fmt.Errorf("HEIF output is selected but the HEIF conversion environment is not ready: %w", err)
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
// directory lies strictly within the watch root.
//
// An output directory equal to the watch root is deliberately not excluded:
// there the job means "write the converted file next to its source but keep the
// original", and every source mirrors onto its own directory. Excluding it
// would exclude the entire tree, silently converting nothing.
func (s JobSpec) IgnoredDir() (string, bool) {
	if s.PostAction != config.ActionOutputFolder {
		return "", false
	}
	rel, err := filepath.Rel(s.WatchDir, s.OutputDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false // same as, or outside, the watch root; nothing to exclude
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

	desiredPath, err := spec.outputPath(srcPath)
	if err != nil {
		return Result{Src: srcPath}, err
	}

	// The temp name carries a per-engine sequence number, so two conversions
	// racing for the same output name (possible whenever distinct sources share a
	// base name and a destination directory) never write to the same temp file.
	tmpPath := fmt.Sprintf("%s.%d%s", desiredPath, e.tempSeq.Add(1), tempSuffix)

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

	// Reserve the output name only now that there is something to put there, and
	// reserve it atomically (create-exclusive) rather than by testing for absence
	// — two concurrent conversions must not both decide the same name is free.
	finalPath, err := claimOutputName(desiredPath)
	if err != nil {
		removeQuietly(tmpPath)
		return Result{Src: srcPath}, err
	}

	// Renaming over the reservation we just made is atomic on Windows.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		removeQuietly(tmpPath)
		removeQuietly(finalPath) // drop the empty reservation
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
