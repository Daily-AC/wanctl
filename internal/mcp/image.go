package mcp

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
)

// maxImageBytes is what one screenshot may cost a caller. A model's context is
// the scarce thing here, and a 5 MiB lossless capture of a 4K desktop buys
// nothing over a 1 MiB one it can actually read: what a screenshot is for is
// seeing which dialog is open, not archiving pixels. Anything larger is
// downscaled until it fits, and the text line says so.
const maxImageBytes = 4 << 20

// fitImage returns the bytes to hand a caller, their media type and the
// dimensions they have.
//
// A capture under the cap goes back untouched, as PNG, which is what a caller
// asking to look at a screen wants. Over it, the image is downscaled and
// re-encoded as JPEG: a screenshot is mostly flat colour, so JPEG at quality 80
// is several times smaller than PNG at the same size, and the two together get
// under the cap without shrinking the screen into unreadability. Note says what
// happened, because a caller reading pixel coordinates off the result has to
// know it is not looking at the device's own resolution.
func fitImage(raw []byte, cap int) (data []byte, mime string, w, h int, note string, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("the device returned %d bytes that are not a decodable PNG", len(raw))
	}
	if len(raw) <= cap {
		return raw, "image/png", cfg.Width, cfg.Height, "", nil
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", 0, 0, "", fmt.Errorf("the device returned a %d-byte PNG that could not be decoded to downscale: %w", len(raw), err)
	}

	// Each pass takes about half the pixels. Starting from a ratio derived from
	// how far over the cap we are would be sharper, but JPEG's size does not
	// follow pixel count closely enough to predict in one step, and a capture
	// is not worth more than a handful of encodes.
	scale := 1.0
	for range 8 {
		scale *= 0.7
		small := downscale(img, scale)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 80}); err != nil {
			return nil, "", 0, 0, "", err
		}
		b := small.Bounds()
		if buf.Len() <= cap {
			return buf.Bytes(), "image/jpeg", b.Dx(), b.Dy(),
				fmt.Sprintf("downscaled from %dx%d and re-encoded as JPEG q80 to fit the %d MiB cap",
					cfg.Width, cfg.Height, cap>>20), nil
		}
		if b.Dx() <= 320 || b.Dy() <= 320 {
			break
		}
	}
	return nil, "", 0, 0, "", fmt.Errorf(
		"this screen does not fit in %d MiB even downscaled; capture it to a file on the device with wanctl_exec and fetch it instead", cap>>20)
}

// downscale box-filters img to a fraction of its size. Averaging the source
// pixels of each destination pixel rather than picking one of them is what
// keeps text on a shrunk screenshot legible instead of aliased into noise.
func downscale(src image.Image, scale float64) *image.RGBA {
	b := src.Bounds()
	w := max(int(float64(b.Dx())*scale), 1)
	h := max(int(float64(b.Dy())*scale), 1)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0 := b.Min.Y + y*b.Dy()/h
		y1 := max(b.Min.Y+(y+1)*b.Dy()/h, y0+1)
		for x := range w {
			x0 := b.Min.X + x*b.Dx()/w
			x1 := max(b.Min.X+(x+1)*b.Dx()/w, x0+1)
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, bl, n = r+cr>>8, g+cg>>8, bl+cb>>8, n+1
				}
			}
			if n == 0 {
				n = 1
			}
			dst.Set(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}
