package mcp

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"
)

// pngClaiming encodes a tiny PNG and rewrites its header to claim w x h, which
// is all DecodeConfig reads.
func pngClaiming(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// signature (8) + length (4) + "IHDR" (4), then width, height.
	binary.BigEndian.PutUint32(b[16:], w)
	binary.BigEndian.PutUint32(b[20:], h)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

// A capture over the size cap is decoded to be downscaled; one whose header
// claims more pixels than any screen is refused before decoding allocates.
func TestHugeClaimedScreenshotIsRefusedBeforeDecoding(t *testing.T) {
	raw := pngClaiming(t, 100_000, 100_000)
	if cfg, err := png.DecodeConfig(bytes.NewReader(raw)); err != nil || cfg.Width != 100_000 {
		t.Fatalf("fixture header: %+v %v", cfg, err)
	}
	_, _, _, _, _, err := fitImage(raw, 1)
	if err == nil || !strings.Contains(err.Error(), "too large to downscale") {
		t.Fatalf("err = %v, want a refusal before decoding", err)
	}
}
