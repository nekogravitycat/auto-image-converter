package convert

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// newHEIFTestEngine builds a HEIF Engine and skips the test when the Python +
// pillow-heif runtime is unavailable, so these tests never break CI on machines
// without the HEIF stack.
func newHEIFTestEngine(t *testing.T, workers int) *Engine {
	t.Helper()
	script, _ := filepath.Abs(filepath.Join("..", "..", "tools", "heif_convert.py"))
	log, _ := logx.New(filepath.Join(t.TempDir(), "t.log"))
	t.Cleanup(func() { log.Close() })

	e := NewEngine(workers, log, script)
	t.Cleanup(e.Close)

	if err := checkHEIFEnvironment(script); err != nil {
		t.Skipf("HEIF environment not available: %v", err)
	}
	return e
}

// heifQuality is the quality used by the HEIF integration tests.
const heifQuality = 90

// writePNG writes a tiny valid PNG and returns its path.
func writePNG(t *testing.T, path string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHEIFPoolWarmReuse converts many files through one warm worker and shows
// that only the first conversion pays the interpreter import cost; the rest
// reuse the already-imported process and are dramatically faster.
func TestHEIFPoolWarmReuse(t *testing.T) {
	c := newHEIFTestEngine(t, 1)
	dir := t.TempDir()

	const n = 12
	var first, warmTotal time.Duration
	for i := range n {
		src := writePNG(t, filepath.Join(dir, "in"+strconv.Itoa(i)+".png"))
		dst := filepath.Join(dir, "out"+strconv.Itoa(i)+".heic")

		start := time.Now()
		if err := c.encodeHEIFFile(src, dst, heifQuality); err != nil {
			t.Fatalf("conversion %d failed: %v", i, err)
		}
		elapsed := time.Since(start)

		if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
			t.Fatalf("conversion %d produced no output (stat err=%v)", i, err)
		}
		if i == 0 {
			first = elapsed // includes the one-time import cost
		} else {
			warmTotal += elapsed
		}
	}
	warmAvg := warmTotal / (n - 1)
	t.Logf("first (cold, incl. import) = %s; warm avg = %s over %d conversions",
		first, warmAvg, n-1)
	// The whole point of the pool: a warm conversion must be far cheaper than a
	// cold one. If the worker were re-spawned per file, warm and cold would be
	// comparable. Require the warm average to be at least 2x faster than cold.
	if warmAvg*2 > first {
		t.Errorf("warm reuse not effective: warm avg %s vs cold %s (expected warm to be much faster)",
			warmAvg, first)
	}
}

// TestHEIFPoolSurvivesBadInput proves that a cleanly-failed conversion (bad
// input) does not poison the worker: the pool keeps serving afterwards.
func TestHEIFPoolSurvivesBadInput(t *testing.T) {
	c := newHEIFTestEngine(t, 1)
	dir := t.TempDir()

	// A file that is not a valid image: the worker should report failure but
	// stay alive.
	bad := filepath.Join(dir, "bad.png")
	if err := os.WriteFile(bad, []byte("this is not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.encodeHEIFFile(bad, filepath.Join(dir, "bad.heic"), heifQuality); err == nil {
		t.Fatal("expected an error converting a non-image file")
	}

	// The very next conversion must still work, using the same live worker.
	good := writePNG(t, filepath.Join(dir, "good.png"))
	dst := filepath.Join(dir, "good.heic")
	if err := c.encodeHEIFFile(good, dst, heifQuality); err != nil {
		t.Fatalf("pool did not recover after a failed job: %v", err)
	}
	if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
		t.Fatalf("expected output after recovery (stat err=%v)", err)
	}
}

// TestHEIFPoolConcurrent runs conversions concurrently up to the worker count,
// mirroring how batch/watch drive the pool, and confirms they all succeed.
func TestHEIFPoolConcurrent(t *testing.T) {
	const workers = 4
	c := newHEIFTestEngine(t, workers)
	dir := t.TempDir()

	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	sem := make(chan struct{}, workers) // mirror batch/watch concurrency cap
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			src := writePNG(t, filepath.Join(dir, "c_in"+strconv.Itoa(i)+".png"))
			dst := filepath.Join(dir, "c_out"+strconv.Itoa(i)+".heic")
			errs[i] = c.encodeHEIFFile(src, dst, heifQuality)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent conversion %d failed: %v", i, err)
		}
	}
}
