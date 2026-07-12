package convert

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// heifEncodeTimeout bounds a single HEIF conversion so a hung or wedged Python
// interpreter can never block a worker indefinitely.
const heifEncodeTimeout = 2 * time.Minute

// heifCheckTimeout bounds the startup environment self-test.
const heifCheckTimeout = 30 * time.Second

// pythonCandidates are the interpreter commands tried, in order, to run the
// bundled HEIF conversion script. "py" is the Windows Python launcher.
var pythonCandidates = []string{"python", "python3", "py"}

// findPython locates a usable Python interpreter on PATH and returns its
// resolved absolute path. It returns an error naming what was tried if none is
// found, so the log can tell the user exactly what to install.
func findPython() (string, error) {
	for _, name := range pythonCandidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no Python interpreter found on PATH (tried: %s)",
		strings.Join(pythonCandidates, ", "))
}

// encodeHEIFFile converts the image at srcPath into a HEIF file at dstPath using
// a warm worker from the HEIF pool (a long-lived Python process with pillow-heif
// already imported). The pool is created on first use.
//
// pillow-heif's own EXIF/ICC passthrough is relied upon for HEIF metadata
// (best-effort). Any worker error, conversion failure, or timeout is treated as
// a failure so the caller leaves the original file untouched.
func (e *Engine) encodeHEIFFile(srcPath, dstPath string, quality int) error {
	pool, err := e.heif()
	if err != nil {
		return err
	}
	return pool.encode(srcPath, dstPath, quality)
}

// checkHEIFEnvironment verifies that HEIF conversion can actually run: a Python
// interpreter is on PATH, the bundled script is present, and the script's own
// self-test (which imports pillow-heif and performs a trial encode) succeeds.
//
// It is a startup guard: catching the problem here means a clear, actionable
// message is logged once, instead of every screenshot failing silently later.
func checkHEIFEnvironment(scriptPath string) error {
	python, err := findPython()
	if err != nil {
		return err
	}
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("HEIF conversion script not found at %s: %w", scriptPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), heifCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, scriptPath, "--check")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("HEIF environment check timed out after %s (using %s)", heifCheckTimeout, python)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("HEIF environment check failed (using %s): %w: %s", python, err, detail)
		}
		return fmt.Errorf("HEIF environment check failed (using %s): %w", python, err)
	}
	return nil
}
