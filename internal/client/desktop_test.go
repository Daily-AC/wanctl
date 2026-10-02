package client

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/desktop"
	"wanctl/internal/protocol"
)

func desktopImage(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	if format == "png" {
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := jpeg.Encode(&b, img, nil); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}
func TestDesktopNewProtocolKeepsJPEGAndPartialResults(t *testing.T) {
	raw := desktopImage(t, "jpeg")
	for _, status := range []string{"completed", "partial", "interrupted", "unknown"} {
		t.Run(status, func(t *testing.T) {
			c, d := net.Pipe()
			defer c.Close()
			defer d.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			go func() {
				m, _ := protocol.ReadMessage(d)
				_ = desktop.WriteResult(d, protocol.DesktopResult{RequestID: m.RequestID, Status: status, Completed: 1, FailedIndex: 1, Snapshot: &protocol.DesktopSnapshot{ID: "new", Width: 8, Height: 4}}, raw)
			}()
			res, err := desktopOver(c, protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: "id", Desktop: &protocol.DesktopRequest{}})
			if err != nil || res.Result.Status != status || res.Result.Completed != 1 || !bytes.Equal(res.Image, raw) {
				t.Fatalf("%+v %v", res, err)
			}
		})
	}
}
func TestDesktopScreenshotCompatibilityConnectionCounts(t *testing.T) {
	pngData := desktopImage(t, "png")
	for _, kind := range []string{"old", "nonwindows"} {
		t.Run(kind, func(t *testing.T) {
			var dials atomic.Int32
			var requests atomic.Int32
			done := make(chan struct{}, 2)
			dial := func(context.Context) (net.Conn, error) {
				index := dials.Add(1)
				c, d := net.Pipe()
				_ = c.SetDeadline(time.Now().Add(time.Second))
				go func() {
					defer d.Close()
					defer func() { done <- struct{}{} }()
					m, _ := protocol.ReadMessage(d)
					requests.Add(1)
					if index == 1 {
						if m.Kind != protocol.KindDesktop {
							t.Errorf("first request %+v", m)
							return
						}
						if kind == "old" {
							_ = protocol.WriteMessage(d, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: desktop"})
							return
						}
						_ = protocol.WriteMessage(d, protocol.Message{Kind: protocol.KindError, Reason: protocol.DesktopWindowsOnly})
						m, _ = protocol.ReadMessage(d)
						requests.Add(1)
					}
					if m.Kind != protocol.KindExec || m.Command != "screenshot" || !m.Elevate || !m.OneShot {
						t.Errorf("legacy request %+v", m)
						return
					}
					_ = protocol.WriteFrame(d, protocol.FrameStdout, pngData)
					_ = protocol.WriteMessage(d, protocol.Message{Kind: protocol.KindExit})
				}()
				return c, nil
			}
			res, err := desktopCallWithDial(context.Background(), "device", "screenshot", "", protocol.DesktopRequest{}, dial)
			if err != nil || !res.Legacy || res.Result.Snapshot != nil || !bytes.Equal(res.Image, pngData) {
				t.Fatalf("%+v %v", res, err)
			}
			expected := int32(1)
			if kind == "old" {
				expected = 2
			}
			if dials.Load() != expected || requests.Load() != 2 {
				t.Fatalf("dials=%d requests=%d", dials.Load(), requests.Load())
			}
			for i := int32(0); i < expected; i++ {
				<-done
			}
		})
	}
}
func TestDesktopActUnsupportedAndAmbiguousTransportNeverRetry(t *testing.T) {
	for _, response := range []string{"unknown request: desktop", protocol.DesktopWindowsOnly, "disconnect", "lost image"} {
		t.Run(response, func(t *testing.T) {
			var dials atomic.Int32
			done := make(chan struct{})
			dial := func(context.Context) (net.Conn, error) {
				dials.Add(1)
				c, d := net.Pipe()
				_ = c.SetDeadline(time.Now().Add(time.Second))
				go func() {
					defer d.Close()
					defer close(done)
					m, _ := protocol.ReadMessage(d)
					if response == "disconnect" {
						return
					}
					if response == "lost image" {
						_ = protocol.WriteMessage(d, protocol.Message{Kind: protocol.KindDesktopResult, DesktopResult: &protocol.DesktopResult{RequestID: m.RequestID, Status: "partial", Completed: 1, Snapshot: &protocol.DesktopSnapshot{ID: "id"}, MIME: "image/jpeg", ImageBytes: 500}})
						return
					}
					_ = protocol.WriteMessage(d, protocol.Message{Kind: protocol.KindError, Reason: response})
				}()
				return c, nil
			}
			res, err := desktopCallWithDial(context.Background(), "device", "act", "", protocol.DesktopRequest{ScreenshotID: "s", Actions: []protocol.DesktopAction{{Type: "type", Text: "test"}}}, dial)
			if err == nil || dials.Load() != 1 {
				t.Fatalf("replayed: %+v %v dials=%d", res, err, dials.Load())
			}
			var unsupported *UnsupportedError
			var lost *DesktopStateUnknownError
			switch response {
			case "unknown request: desktop":
				if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "desktop act") {
					t.Fatal(err)
				}
			case protocol.DesktopWindowsOnly:
				if !errors.Is(err, desktop.ErrWindowsOnly) {
					t.Fatal(err)
				}
			default:
				if !errors.As(err, &lost) || !strings.Contains(err.Error(), "do not replay") {
					t.Fatal(err)
				}
			}
			if response == "lost image" && (res == nil || res.Result.Completed != 1) {
				t.Fatal("partial action evidence lost")
			}
			<-done
		})
	}
}
func TestDesktopMismatchRefusesCoordinateContract(t *testing.T) {
	raw := desktopImage(t, "jpeg")
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	go func() {
		m, _ := protocol.ReadMessage(d)
		_ = desktop.WriteResult(d, protocol.DesktopResult{RequestID: m.RequestID, Status: "completed", Snapshot: &protocol.DesktopSnapshot{ID: "s", Width: 16, Height: 4}}, raw)
	}()
	if _, err := desktopOver(c, protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: "id"}); err == nil {
		t.Fatal("accepted resized/mismatched image")
	}
}

func TestDesktopCancellationClosesStreamAndDoesNotReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, closed := make(chan struct{}), make(chan struct{})
	var dials atomic.Int32
	dial := func(context.Context) (net.Conn, error) {
		dials.Add(1)
		c, d := net.Pipe()
		go func() {
			defer d.Close()
			defer close(closed)
			_, _ = protocol.ReadMessage(d)
			close(entered)
			_, _ = protocol.ReadMessage(d)
		}()
		return c, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := desktopCallWithDial(ctx, "device", "act", "", protocol.DesktopRequest{ScreenshotID: "id", Actions: []protocol.DesktopAction{{Type: "wait", Millis: 10000}}}, dial)
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		var lost *DesktopStateUnknownError
		if !errors.As(err, &lost) {
			t.Fatalf("cancelled act must remain unknown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled client kept waiting")
	}
	<-closed
	if dials.Load() != 1 {
		t.Fatal("cancelled call reconnected")
	}
}
