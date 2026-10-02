package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

func TestDesktopCLIOutputPreservesImageAndMetadata(t *testing.T) {
	raw := []byte{0xff, 0xd8, 0xff, 0xd9}
	res := &client.DesktopResponse{Result: protocol.DesktopResult{Status: "partial", Completed: 1, FailedIndex: 1, Snapshot: &protocol.DesktopSnapshot{ID: "screen"}}, Image: raw}
	var out, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "shot.jpg")
	if err := writeDesktopOutput(res, "pc", path, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, raw) {
		t.Fatal("JPEG changed")
	}
	var metadata map[string]any
	if json.Unmarshal(out.Bytes(), &metadata) != nil || metadata["path"] != path || metadata["completed"] != float64(1) {
		t.Fatalf("%s", out.Bytes())
	}
	out.Reset()
	stderr.Reset()
	if err := writeDesktopOutput(res, "pc", "-", &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), raw) || !strings.Contains(stderr.String(), "screen") {
		t.Fatal("binary output mixed with JSON")
	}
}
func TestDesktopCLIRejectsMalformedActionsWithoutLeakingText(t *testing.T) {
	var actions []protocol.DesktopAction
	secret := "SYNTHETIC_PRIVATE"
	for _, raw := range []string{`[{"type":"type","text":"` + secret + `","force":true}]`, `[{"type":"click","x":"` + secret + `"}]`, `[] []`} {
		err := decodeActions([]byte(raw), &actions)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("malformed request accepted or leaked")
		}
	}
	r, err := parseDesktopRegion("1,2,30,40")
	if err != nil || r.Width != 30 || r.Height != 40 {
		t.Fatalf("%+v %v", r, err)
	}
}
