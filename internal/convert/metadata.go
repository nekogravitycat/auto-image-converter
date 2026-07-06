package convert

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// pngSignature is the 8-byte magic that begins every PNG file.
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// errNotPNG indicates the provided bytes are not a PNG stream.
var errNotPNG = errors.New("not a PNG stream")

// extractPNGExif scans the raw bytes of a PNG file and returns the payload of
// its eXIf chunk, if present.
//
// This is necessary because image/png.Decode discards ancillary chunks such as
// eXIf, so the raw file must be parsed separately to recover EXIF metadata. The
// returned bytes are the Exif profile beginning at the TIFF header (byte order
// "II" or "MM"), exactly the form expected after the "Exif\0\0" prefix of a
// JPEG APP1 segment.
//
// It returns (nil, nil) when the file is a valid PNG that simply has no eXIf
// chunk, and (nil, errNotPNG) when the bytes are not a PNG at all.
func extractPNGExif(data []byte) ([]byte, error) {
	if len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return nil, errNotPNG
	}

	// Walk the chunk sequence: each chunk is [length:4][type:4][data:length][crc:4].
	offset := len(pngSignature)
	for offset+8 <= len(data) {
		length := binary.BigEndian.Uint32(data[offset : offset+4])
		chunkType := string(data[offset+4 : offset+8])
		dataStart := offset + 8
		dataEnd := dataStart + int(length)

		// Guard against truncated or malformed files (dataEnd + 4-byte CRC).
		if dataEnd+4 > len(data) || dataEnd < dataStart {
			break
		}

		switch chunkType {
		case "eXIf":
			exif := make([]byte, length)
			copy(exif, data[dataStart:dataEnd])
			return exif, nil
		case "IEND":
			return nil, nil
		}

		offset = dataEnd + 4 // advance past this chunk's CRC
	}

	return nil, nil
}
