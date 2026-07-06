package convert

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// heifPool manages a set of long-lived Python worker processes that each import
// pillow-heif once and then convert an unbounded stream of images.
//
// The problem it solves: spawning a fresh interpreter per image makes the
// startup + import cost (the dominant cost, far larger than the encode itself)
// scale with the number of files, which saturates the CPU during a large
// startup batch. Keeping workers warm pays that cost only once per worker.
//
// Concurrency is bounded by slots (capacity = pool size). A worker is created
// lazily on demand, reused across jobs while healthy, and discarded (never
// reused) the moment it misbehaves — so a crashed or wedged worker self-heals
// on the next job without ever blocking the pool.
type heifPool struct {
	python     string
	scriptPath string
	log        *logx.Logger

	slots chan struct{} // acquired for the duration of each job; bounds concurrency

	// job terminates every worker process when this program exits, even under a
	// hard kill; nil if the OS would not grant one (workers then still exit on
	// stdin EOF at a normal shutdown).
	job *killOnCloseJob

	resolveOnce sync.Once
	resolveErr  error

	mu     sync.Mutex
	free   []*heifWorker
	closed bool
}

// heifRequest and heifResponse are the newline-delimited JSON protocol shared
// with the --serve mode of heif_convert.py.
type heifRequest struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Quality int    `json:"quality"`
}

type heifResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// newHeifPool creates a pool of the given size. The size is clamped to at least
// one so a misconfigured worker count can never deadlock the slot channel.
func newHeifPool(scriptPath string, log *logx.Logger, size int) *heifPool {
	if size < 1 {
		size = 1
	}
	job, err := newKillOnCloseJob()
	if err != nil {
		log.Warnf("HEIF workers: guaranteed-cleanup job unavailable (%v); workers still exit on stdin EOF at a normal shutdown", err)
		job = nil
	}
	return &heifPool{
		scriptPath: scriptPath,
		log:        log,
		slots:      make(chan struct{}, size),
		job:        job,
	}
}

// resolveEnv locates the Python interpreter and confirms the script exists. It
// runs at most once; the result is cached because both failures are permanent
// for the lifetime of the process (nothing installs Python mid-run).
func (p *heifPool) resolveEnv() {
	python, err := findPython()
	if err != nil {
		p.resolveErr = fmt.Errorf("cannot encode HEIF: %w", err)
		return
	}
	if _, err := os.Stat(p.scriptPath); err != nil {
		p.resolveErr = fmt.Errorf("HEIF conversion script not available at %s: %w", p.scriptPath, err)
		return
	}
	p.python = python
}

// encode converts src to dst via a warm worker, blocking only while it holds a
// concurrency slot. It returns nil on success and a descriptive error on
// failure; on any error the caller leaves the original file untouched.
func (p *heifPool) encode(src, dst string, quality int) error {
	p.resolveOnce.Do(p.resolveEnv)
	if p.resolveErr != nil {
		return p.resolveErr
	}

	// Acquiring a slot bounds concurrency to the pool size. Callers (batch and
	// watch) are already capped at the same worker count, so this never blocks
	// for long.
	p.slots <- struct{}{}
	defer func() { <-p.slots }()

	w, err := p.take()
	if err != nil {
		return err
	}

	runErr := w.run(src, dst, quality, heifEncodeTimeout)
	if w.broken {
		// A compromised worker (broken pipe, timeout, protocol error) is killed
		// and dropped; the next job will spawn a fresh one.
		w.kill()
		return runErr
	}
	// The worker is healthy — reuse it — even if this particular image failed
	// to convert (e.g. a corrupt PNG).
	p.put(w)
	return runErr
}

// take returns a reusable idle worker, or spawns a new one if none is free.
func (p *heifPool) take() (*heifWorker, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("HEIF worker pool is closed")
	}
	if n := len(p.free); n > 0 {
		w := p.free[n-1]
		p.free[n-1] = nil
		p.free = p.free[:n-1]
		p.mu.Unlock()
		return w, nil
	}
	p.mu.Unlock()
	return p.spawn()
}

// put returns a healthy worker to the idle set, or kills it if the pool has
// since been closed.
func (p *heifPool) put(w *heifWorker) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		w.kill()
		return
	}
	p.free = append(p.free, w)
	p.mu.Unlock()
}

// spawn starts a new worker process in --serve mode.
func (p *heifPool) spawn() (*heifWorker, error) {
	cmd := exec.Command(p.python, p.scriptPath, "--serve")
	// Prevent a console window from flashing when launching the interpreter.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("HEIF worker: could not open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("HEIF worker: could not open stdout: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("HEIF worker: could not start %s: %w", p.python, err)
	}
	// Enroll the worker in the cleanup job so it cannot outlive the parent, even
	// if the parent is force-killed. Best-effort: a worker that fails to enroll
	// still exits on stdin EOF during an orderly shutdown.
	if p.job != nil {
		if err := p.job.assign(cmd.Process.Pid); err != nil {
			p.log.Warnf("HEIF worker: could not enroll in cleanup job (%v); it may outlive a hard kill", err)
		}
	}
	return &heifWorker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		stderr: stderr,
	}, nil
}

// Close shuts down the pool and all idle workers. Workers currently running a
// job are killed when they are returned via put. Close is safe to call more
// than once.
func (p *heifPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	free := p.free
	p.free = nil
	p.mu.Unlock()

	for _, w := range free {
		w.kill()
	}
	// Closing the job terminates any worker still checked out (e.g. one wedged
	// mid-encode that never returned via put), guaranteeing none is left behind.
	if p.job != nil {
		p.job.close()
	}
}

// heifWorker is a single long-lived Python process. It processes one job at a
// time; exclusive access is guaranteed by the pool (a worker is only ever held
// by one caller), so no per-worker lock is needed.
type heifWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
	broken bool // set when the worker can no longer be trusted for reuse
}

// run sends one conversion job and waits for the worker's response, bounded by
// timeout. It sets w.broken when the worker itself is at fault (as opposed to a
// job that failed cleanly), so the pool knows not to reuse it.
func (w *heifWorker) run(src, dst string, quality int, timeout time.Duration) error {
	req, err := json.Marshal(heifRequest{Src: src, Dst: dst, Quality: quality})
	if err != nil {
		w.broken = true
		return fmt.Errorf("HEIF worker: could not encode request: %w", err)
	}
	req = append(req, '\n')

	type outcome struct {
		resp heifResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		if _, err := w.stdin.Write(req); err != nil {
			done <- outcome{err: fmt.Errorf("HEIF worker: write failed: %w", err)}
			return
		}
		line, err := w.stdout.ReadBytes('\n')
		if err != nil {
			done <- outcome{err: fmt.Errorf("HEIF worker: read failed: %w", err)}
			return
		}
		var resp heifResponse
		if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
			done <- outcome{err: fmt.Errorf("HEIF worker: bad response %q: %w", strings.TrimSpace(string(line)), err)}
			return
		}
		done <- outcome{resp: resp}
	}()

	select {
	case <-time.After(timeout):
		w.broken = true
		return fmt.Errorf("HEIF conversion timed out after %s for %s", timeout, src)
	case r := <-done:
		if r.err != nil {
			w.broken = true
			if detail := strings.TrimSpace(w.stderr.String()); detail != "" {
				return fmt.Errorf("%w: %s", r.err, detail)
			}
			return r.err
		}
		if !r.resp.OK {
			// The worker is fine; this image just could not be converted.
			return fmt.Errorf("HEIF conversion failed for %s: %s", src, r.resp.Error)
		}
		return nil
	}
}

// kill terminates the worker process and reaps it. It is safe to call on an
// already-dead worker.
func (w *heifWorker) kill() {
	_ = w.stdin.Close() // EOF asks the worker to exit on its own...
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill() // ...and this guarantees it, unblocking any read.
	}
	_ = w.cmd.Wait()
}
