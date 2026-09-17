package mcp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

// noisyPNG is a PNG that does not compress: random pixels, so the encoder
// cannot shrink it and the size is really the size. A flat image would sail
// under any cap and prove nothing about the downscaling path.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(1))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A capture that fits goes back untouched: the caller asked to look at a
// screen, not at a re-encoding of one.
func TestSmallCaptureIsReturnedAsIs(t *testing.T) {
	raw := noisyPNG(t, 200, 100)
	data, mime, w, h, note, err := fitImage(raw, maxImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, raw) {
		t.Error("a capture under the cap was re-encoded")
	}
	if mime != "image/png" || w != 200 || h != 100 || note != "" {
		t.Errorf("got %s %dx%d note=%q", mime, w, h, note)
	}
}

// Over the cap the image is shrunk until it fits, and the result says so —
// a caller reading coordinates off it has to know it is not looking at the
// device's own resolution.
func TestOversizedCaptureIsDownscaledUnderTheCap(t *testing.T) {
	raw := noisyPNG(t, 1600, 1200)
	if len(raw) <= 4<<20 {
		t.Fatalf("fixture is only %d bytes; it must exceed the 4 MiB cap to test anything", len(raw))
	}
	data, mime, w, h, note, err := fitImage(raw, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4<<20 {
		t.Fatalf("result is %d bytes, over the cap", len(data))
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %s, want image/jpeg for the downscaled variant", mime)
	}
	if w >= 1600 || h >= 1200 || w == 0 || h == 0 {
		t.Errorf("dimensions %dx%d are not a downscale of 1600x1200", w, h)
	}
	if !strings.Contains(note, "downscaled from 1600x1200") || !strings.Contains(note, "JPEG") {
		t.Errorf("note does not say what happened: %q", note)
	}
}

// The tool result a host receives carries the pixels as image content, not as a
// wall of base64 in a text block, plus one line saying what it is.
func TestScreenshotResultCarriesImageContent(t *testing.T) {
	raw := noisyPNG(t, 120, 80)
	res := screenshotResult(raw)
	if res.IsError {
		t.Fatalf("result is an error: %+v", res.Content)
	}
	var text string
	var img *mcpapi.ImageContent
	for _, c := range res.Content {
		switch v := c.(type) {
		case mcpapi.TextContent:
			text = v.Text
		case mcpapi.ImageContent:
			img = &v
		}
	}
	if img == nil {
		t.Fatal("result has no image content")
	}
	if img.MIMEType != "image/png" {
		t.Errorf("mime = %s", img.MIMEType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("image data is not base64: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Error("the image content is not the captured PNG")
	}
	if !strings.Contains(text, "120x80") || !strings.Contains(text, "image/png") {
		t.Errorf("text line = %q, want the dimensions and the format", text)
	}
}

// Bytes that are not a PNG mean the device did not run a capture at all, and
// the likeliest reason is an agent too old to know how.
func TestScreenshotResultRefusesNonPNGWithTheUpdateHint(t *testing.T) {
	res := screenshotResult([]byte("screenshot: command not found\n"))
	if !res.IsError {
		t.Fatal("garbage was accepted as an image")
	}
	text := res.Content[0].(mcpapi.TextContent).Text
	if !strings.Contains(text, "wanctl update") {
		t.Errorf("error does not point at the fix: %q", text)
	}
}
