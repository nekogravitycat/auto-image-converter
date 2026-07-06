package convert

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// encodeHEIFFile converts the image at srcPath into a HEIF file at dstPath by
// running the bundled heif_convert.py script through the system Python
// interpreter (which must have pillow-heif installed).
//
// pillow-heif's own EXIF/ICC passthrough is relied upon for HEIF metadata
// (best-effort). Any non-zero exit code — or a timeout — is treated as a
// failure so the caller leaves the original file untouched.
func (c *Converter) encodeHEIFFile(srcPath, dstPath string) error {
	python, err := findPython()
	if err != nil {
		return fmt.Errorf("cannot encode HEIF: %w", err)
	}
	if _, err := os.Stat(c.heifScriptPath); err != nil {
		return fmt.Errorf("HEIF conversion script not available at %s: %w", c.heifScriptPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), heifEncodeTimeout)
	defer cancel()

	// heif_convert.py CLI: heif_convert.py -q <quality> -o <output> <input>.
	// Arguments are passed directly (no shell), so paths need no escaping and
	// cannot be interpreted as commands.
	cmd := exec.CommandContext(ctx, python, c.heifScriptPath,
		"-q", strconv.Itoa(c.cfg.Converter.Quality),
		"-o", dstPath,
		srcPath,
	)
	// Prevent a console window from flashing when invoking the interpreter.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("HEIF conversion timed out after %s for %s", heifEncodeTimeout, srcPath)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("HEIF conversion failed for %s: %w: %s", srcPath, err, detail)
		}
		return fmt.Errorf("HEIF conversion failed for %s: %w", srcPath, err)
	}
	return nil
}

// checkHEIFEnvironment verifies that HEIF conversion can actually run: a Python
// interpreter is on PATH, the bundled script is present, and the script's own
// self-test (which imports pillow-heif and performs a trial encode) succeeds.
//
// It is a startup guard: catching the problem here means a clear, actionable
// message is logged once, instead of every screenshot failing silently later.
func (c *Converter) checkHEIFEnvironment() error {
	python, err := findPython()
	if err != nil {
		return err
	}
	if _, err := os.Stat(c.heifScriptPath); err != nil {
		return fmt.Errorf("HEIF conversion script not found at %s: %w", c.heifScriptPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), heifCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, c.heifScriptPath, "--check")
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
