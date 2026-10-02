//go:build windows

package desktop

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"unsafe"

	"wanctl/internal/protocol"
)

type bitmapInfoHeader struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}

func (b *nativeBackend) Capture(crop *protocol.Rect) (protocol.DesktopSnapshot, []byte, error) {
	snap, err := displayState()
	if err != nil {
		return snap, nil, err
	}
	if crop != nil {
		if crop.X < snap.Source.X || crop.Y < snap.Source.Y || crop.Width <= 0 || crop.Height <= 0 || crop.X+crop.Width > snap.Source.X+snap.Source.Width || crop.Y+crop.Height > snap.Source.Y+snap.Source.Height {
			return snap, nil, errors.New("crop is outside current desktop")
		}
		snap.Source = *crop
	}
	scale := math.Min(1, 1280/float64(max(snap.Source.Width, snap.Source.Height)))
	snap.Width = max(1, int(math.Round(float64(snap.Source.Width)*scale)))
	snap.Height = max(1, int(math.Round(float64(snap.Source.Height)*scale)))
	snap.Scale = float64(snap.Width) / float64(snap.Source.Width)
	snap.Foreground, err = b.Foreground()
	if err != nil {
		return snap, nil, err
	}
	snap.Windows, err = b.Windows()
	if err != nil {
		return snap, nil, err
	}
	img, err := captureGDI(snap.Source, snap.Width, snap.Height)
	if err != nil {
		return snap, nil, err
	}
	after, err := displayState()
	if err != nil {
		return snap, nil, err
	}
	if after.Layout != snap.Layout {
		return snap, nil, errors.New("display changed while capturing; take a fresh screenshot")
	}
	// A locked/secure desktop is already rejected by displayState. An all-black
	// capture can also mean protected/fullscreen content that GDI cannot read;
	// it must not masquerade as a usable desktop screenshot.
	nonblack := false
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 || img.Pix[i+1] != 0 || img.Pix[i+2] != 0 {
			nonblack = true
			break
		}
	}
	if !nonblack {
		return snap, nil, errors.New("desktop capture is blank or protected; no usable image was returned")
	}
	var out bytes.Buffer
	if err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 82}); err != nil {
		return snap, nil, errors.New("could not encode desktop JPEG")
	}
	return snap, out.Bytes(), nil
}
func captureGDI(source protocol.Rect, width, height int) (*image.RGBA, error) {
	dc, _, _ := user32.NewProc("GetDC").Call(0)
	if dc == 0 {
		return nil, errors.New("desktop capture unavailable")
	}
	defer user32.NewProc("ReleaseDC").Call(0, dc)
	mem, _, _ := gdi32.NewProc("CreateCompatibleDC").Call(dc)
	if mem == 0 {
		return nil, errors.New("cannot create capture context")
	}
	defer gdi32.NewProc("DeleteDC").Call(mem)
	bitmap, _, _ := gdi32.NewProc("CreateCompatibleBitmap").Call(dc, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return nil, errors.New("cannot allocate capture bitmap")
	}
	defer gdi32.NewProc("DeleteObject").Call(bitmap)
	old, _, _ := gdi32.NewProc("SelectObject").Call(mem, bitmap)
	if old == 0 || old == ^uintptr(0) {
		return nil, errors.New("cannot select capture bitmap")
	}
	gdi32.NewProc("SetStretchBltMode").Call(mem, 4) // HALFTONE
	gdi32.NewProc("SetBrushOrgEx").Call(mem, 0, 0, 0)
	// Capture the physical virtual desktop straight into the output-sized
	// bitmap. This bounds memory even with several large mixed-DPI displays.
	ok, _, _ := gdi32.NewProc("StretchBlt").Call(mem, 0, 0, uintptr(width), uintptr(height), dc, uintptr(source.X), uintptr(source.Y), uintptr(source.Width), uintptr(source.Height), 0x00cc0020) // SRCCOPY
	gdi32.NewProc("SelectObject").Call(mem, old)                                                                                                                                                  // GetDIBits requires it unselected.
	if ok == 0 {
		return nil, errors.New("desktop capture failed; content may be protected")
	}
	pixels := make([]byte, width*height*4)
	header := bitmapInfoHeader{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	lines, _, _ := gdi32.NewProc("GetDIBits").Call(mem, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&pixels[0])), uintptr(unsafe.Pointer(&header)), 0)
	if int(lines) != height {
		return nil, errors.New("incomplete desktop capture")
	}
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+2] = pixels[i+2], pixels[i]
		pixels[i+3] = 255
	}
	return &image.RGBA{Pix: pixels, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}, nil
}
