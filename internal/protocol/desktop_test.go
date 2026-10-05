package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

func TestDesktopWireRoundTripAndLegacy(t *testing.T) {
	for _, m := range []Message{
		{Kind: KindDesktop, Action: "act", RequestID: "id", Desktop: &DesktopRequest{ScreenshotID: "screen", Actions: []DesktopAction{{Type: "type", Text: "Unicode 中文 🚀"}, {Type: "drag", X: 2, Y: 7, ToX: 8, ToY: 9}}}},
		{Kind: KindDesktopResult, DesktopResult: &DesktopResult{Status: "partial", Completed: 1, FailedIndex: 1, ImageBytes: 4, MIME: "image/jpeg", Snapshot: &DesktopSnapshot{Origin: Point{X: -1920}, Width: 1280}}},
		{Kind: KindExec, Command: "screenshot", Elevate: true, OneShot: true},
	} {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatal(err)
		}
		raw := []byte{0xff, 0xd8, 0xff, 0xd9}
		if err := WriteFrame(&buf, FrameData, raw); err != nil {
			t.Fatal(err)
		}
		got, err := ReadMessage(&buf)
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("%+v %v", got, err)
		}
		typ, payload, err := ReadFrame(&buf)
		if err != nil || typ != FrameData || !bytes.Equal(payload, raw) {
			t.Fatal("JPEG framing changed")
		}
	}
}
