package convert

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// pngChunk builds a single PNG chunk: length, type, data, CRC.
func pngChunk(chunkType string, data []byte) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, chunkType...)
	b = append(b, data...)
	crc := crc32.ChecksumIEEE(append([]byte(chunkType), data...))
	b = binary.BigEndian.AppendUint32(b, crc)
	return b
}

// pngWithExif produces a valid 1x1 PNG containing an eXIf chunk with the given
// payload, inserted just before the trailing IEND chunk.
func pngWithExif(t *testing.T, exif []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	base := buf.Bytes()
	// The IEND chunk is the final 12 bytes (len=0, "IEND", crc).
	insertAt := len(base) - 12
	out := append([]byte{}, base[:insertAt]...)
	out = append(out, pngChunk("eXIf", exif)...)
	out = append(out, base[insertAt:]...)
	return out
}

func TestExtractPNGExif(t *testing.T) {
	exif := []byte("II*\x00this-is-fake-exif")
	data := pngWithExif(t, exif)

	got, err := extractPNGExif(data)
	if err != nil {
		t.Fatalf("extractPNGExif error: %v", err)
	}
	if !bytes.Equal(got, exif) {
		t.Fatalf("extracted exif = %q, want %q", got, exif)
	}
}

func TestExtractPNGExifNoneAndNonPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	got, err := extractPNGExif(buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error for PNG without exif: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil exif for PNG without eXIf chunk, got %d bytes", len(got))
	}

	if _, err := extractPNGExif([]byte("not a png")); err != errNotPNG {
		t.Errorf("expected errNotPNG, got %v", err)
	}
}

func TestEncodeJPEGEmbedsExif(t *testing.T) {
	exif := []byte("MM\x00*embedded-exif-payload")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))

	data, warn := encodeJPEG(img, 90, exif)
	if warn != nil {
		t.Fatalf("unexpected exif warning: %v", warn)
	}
	if data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("output is not a JPEG stream")
	}
	needle := append([]byte(exifPrefix), exif...)
	if !bytes.Contains(data, needle) {
		t.Errorf("APP1 EXIF segment not found in JPEG output")
	}
}

func TestCompositeAndOpaque(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{0, 0, 0, 0}) // transparent pixel
	if isOpaque(img) {
		t.Errorf("image with a transparent pixel reported as opaque")
	}

	composited := compositeOnWhite(img)
	r, g, b, a := composited.At(0, 0).RGBA()
	if r>>8 != 255 || g>>8 != 255 || b>>8 != 255 || a>>8 != 255 {
		t.Errorf("composited transparent pixel = (%d,%d,%d,%d), want opaque white",
			r>>8, g>>8, b>>8, a>>8)
	}
}

func TestEnsureUnique(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.jpg")

	if got := ensureUnique(p); got != p {
		t.Errorf("ensureUnique on free path = %q, want %q", got, p)
	}

	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "shot-1.jpg")
	if got := ensureUnique(p); got != want {
		t.Errorf("ensureUnique on taken path = %q, want %q", got, want)
	}
}
