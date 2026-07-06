package convert

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/nekogravitycat/auto-image-converter/internal/config"
	"github.com/nekogravitycat/auto-image-converter/internal/logx"
)

// TestHEIFIntegration exercises the real Python sidecar. It is skipped when the
// environment self-check fails (no Python / pillow-heif), so it never breaks CI
// on machines without the HEIF runtime.
func TestHEIFIntegration(t *testing.T) {
	script, _ := filepath.Abs(filepath.Join("..", "..", "tools", "heif_convert.py"))
	cfg := config.Config{Converter: config.ConverterConfig{TargetFormat: config.FormatHEIF, Quality: 90}}
	log, _ := logx.New(filepath.Join(t.TempDir(), "t.log"))
	defer log.Close()
	c := New(cfg, log, script)

	if err := c.checkHEIFEnvironment(); err != nil {
		t.Skipf("HEIF environment not available: %v", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "in.png")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.heic")

	if err := c.encodeHEIFFile(src, dst); err != nil {
		t.Fatalf("encodeHEIFFile: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty HEIF output, stat err=%v", err)
	}
	t.Logf("produced %d-byte HEIF via Go wrapper", info.Size())
}
