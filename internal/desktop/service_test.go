package desktop

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

func TestServiceFailureAndDuplicateNeverReplay(t *testing.T) {
	for _, status := range []string{"completed", "partial", "interrupted", "crash"} {
		t.Run(status, func(t *testing.T) {
			var service Service
			snap, _ := service.Store.Put("owner", example(), time.Now())
			req := &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "type", Text: "SYNTHETIC"}}}
			calls := 0
			run := func(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
				calls++
				if job.Reference.ID != snap.ID {
					t.Fatal("lost reference")
				}
				if status == "crash" {
					return protocol.DesktopResult{}, nil, errors.New("process died")
				}
				return protocol.DesktopResult{Status: status, Completed: 1}, nil, nil
			}
			id := NewID()
			res, _ := service.Do(context.Background(), "owner", "controller", "act", id, req, run)
			if status == "crash" && res.Status != "unknown" {
				t.Fatalf("%+v", res)
			}
			for _, again := range []string{id, NewID()} {
				res, _ = service.Do(context.Background(), "owner", "controller", "act", again, req, run)
				if res.Status != "unknown" {
					t.Fatalf("%+v", res)
				}
			}
			if calls != 1 {
				t.Fatalf("replayed %d times", calls)
			}
		})
	}
}
func TestServiceBusyDoesNotQueueInput(t *testing.T) {
	var service Service
	service.busy.Lock()
	defer service.busy.Unlock()
	res, _ := service.Do(context.Background(), "owner", "controller", "act", NewID(), &protocol.DesktopRequest{ScreenshotID: "id", Actions: []protocol.DesktopAction{{Type: "wait"}}}, func(context.Context, Job) (protocol.DesktopResult, []byte, error) {
		t.Fatal("queued call executed")
		return protocol.DesktopResult{}, nil, nil
	})
	if res.Status != "unknown" {
		t.Fatalf("%+v", res)
	}
}
func TestResultFramingRejectsTruncationAndWrongPayload(t *testing.T) {
	header := protocol.DesktopResult{Status: "partial", Completed: 1, FailedIndex: 1, Snapshot: &protocol.DesktopSnapshot{ID: "screen"}}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}
	var buf bytes.Buffer
	if err := WriteResult(&buf, header, jpeg); err != nil {
		t.Fatal(err)
	}
	res, got, err := ReadResult(&buf)
	if err != nil || res.Completed != 1 || !bytes.Equal(got, jpeg) {
		t.Fatalf("%+v %v", res, err)
	}
	for _, kind := range []protocol.FrameType{protocol.FrameStdout, protocol.FrameJSON} {
		header.ImageBytes = len(jpeg)
		header.MIME = "image/jpeg"
		var bad bytes.Buffer
		_ = protocol.WriteMessage(&bad, protocol.Message{Kind: protocol.KindDesktopResult, DesktopResult: &header})
		_ = protocol.WriteFrame(&bad, kind, jpeg)
		if _, _, err = ReadResult(&bad); err == nil {
			t.Fatal("accepted wrong frame")
		}
	}
	var partial bytes.Buffer
	_ = WriteResult(&partial, header, jpeg)
	data := partial.Bytes()
	if _, _, err = ReadResult(bytes.NewReader(data[:len(data)-1])); err == nil {
		t.Fatal("accepted truncated image")
	}
}
