package convert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// encodeJPEG encodes img as JPEG at the given quality and, when exif is
// non-empty, embeds it as an APP1 segment.
//
// If the image is not opaque (i.e. has an alpha channel), it is first
// composited onto a solid white background, because JPEG has no transparency
// and would otherwise render transparent regions as black artifacts.
//
// The returned data is always a valid JPEG. A non-nil exifWarning means the
// pixels were encoded successfully but the EXIF could not be embedded
// (best-effort); the caller should log it and still write the data.
func encodeJPEG(img image.Image, quality int, exif []byte) (data []byte, exifWarning error) {
	if !isOpaque(img) {
		img = compositeOnWhite(img)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode failed: %w", err)
	}
	data = buf.Bytes()

	if len(exif) == 0 {
		return data, nil
	}

	merged, err := insertExifAPP1(data, exif)
	if err != nil {
		return data, err // best-effort: return the EXIF-less JPEG plus the warning
	}
	return merged, nil
}

// isOpaque reports whether img is fully opaque. Images that do not expose an
// Opaque method are conservatively treated as non-opaque so they are composited.
func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}

// compositeOnWhite draws src over a solid white background of the same size.
func compositeOnWhite(src image.Image) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Over)
	return dst
}

// exifPrefix is the identifier that precedes the TIFF data inside a JPEG APP1
// EXIF segment.
const exifPrefix = "Exif\x00\x00"

// insertExifAPP1 inserts an EXIF APP1 segment into a JPEG byte stream, after an
// existing APP0 (JFIF) segment if present, otherwise immediately after the SOI
// marker.
func insertExifAPP1(jpegData, exif []byte) ([]byte, error) {
	if len(jpegData) < 2 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		return nil, errors.New("not a JPEG stream")
	}

	// APP1 length covers the 2-byte length field, the "Exif\0\0" prefix, and the
	// EXIF payload. It must fit in a 16-bit field.
	segmentLength := 2 + len(exifPrefix) + len(exif)
	if segmentLength > 0xFFFF {
		return nil, fmt.Errorf("exif too large to embed (%d bytes)", len(exif))
	}

	segment := make([]byte, 0, 2+segmentLength)
	segment = append(segment, 0xFF, 0xE1) // APP1 marker
	segment = binary.BigEndian.AppendUint16(segment, uint16(segmentLength))
	segment = append(segment, exifPrefix...)
	segment = append(segment, exif...)

	insertPos := insertPosition(jpegData)

	out := make([]byte, 0, len(jpegData)+len(segment))
	out = append(out, jpegData[:insertPos]...)
	out = append(out, segment...)
	out = append(out, jpegData[insertPos:]...)
	return out, nil
}

// insertPosition returns the byte offset at which a new APP1 segment should be
// inserted: after an existing APP0 segment when present, otherwise right after
// the 2-byte SOI marker.
func insertPosition(jpegData []byte) int {
	const afterSOI = 2
	// Is there an APP0 (FFE0) segment immediately after SOI?
	if len(jpegData) >= 6 && jpegData[2] == 0xFF && jpegData[3] == 0xE0 {
		app0Length := int(binary.BigEndian.Uint16(jpegData[4:6]))
		end := 4 + app0Length // length field starts at offset 4 and counts itself
		if end <= len(jpegData) {
			return end
		}
	}
	return afterSOI
}
