package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
)

func TestOpenCUCLIImagesAndRawOutput(t *testing.T) {
	c, err := opencu.Start(context.Background(), exec.Command(ocutest.Build(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peerRaw, err := c.Call(context.Background(), "peer_apply", json.RawMessage(`{"error":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var peer map[string]json.RawMessage
	_ = json.Unmarshal(peerRaw, &peer)
	var items []json.RawMessage
	_ = json.Unmarshal(peer["content"], &items)
	// Model two image content items. Both must remain independently readable.
	items = append(items, items[1])
	peer["content"], _ = json.Marshal(items)
	raw, _ := json.Marshal(peer)
	var sourceImage struct{ Data string }
	_ = json.Unmarshal(items[1], &sourceImage)
	expectedImage, _ := base64.StdEncoding.DecodeString(sourceImage.Data)
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			dir := ""
			if explicit {
				dir = filepath.Join(t.TempDir(), "screens")
			}
			var out, diag bytes.Buffer
			res := &client.DesktopResponse{Result: protocol.DesktopResult{Status: "completed", OpenCU: &protocol.OpenCUResult{Result: raw}}}
			if err := writeDesktopOutput(res, "", dir, &out, &diag); err != nil {
				t.Fatal(err)
			}
			var summary struct {
				ResultFile string `json:"result_file"`
				Content    []map[string]json.RawMessage
				Meta       json.RawMessage `json:"_meta"`
				IsError    bool
			}
			if json.Unmarshal(out.Bytes(), &summary) != nil {
				t.Fatal("invalid output")
			}
			if !explicit {
				defer os.RemoveAll(filepath.Dir(summary.ResultFile))
			}
			kept, err := os.ReadFile(summary.ResultFile)
			if err != nil || !bytes.Equal(kept, raw) {
				t.Fatal("full MCP result changed")
			}
			info, _ := os.Stat(summary.ResultFile)
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("private result permissions")
			}
			if !summary.IsError || !bytes.Equal(summary.Meta, peer["_meta"]) {
				t.Fatal("metadata changed")
			}
			gotText, _ := json.Marshal(summary.Content[0])
			var sourceText map[string]json.RawMessage
			_ = json.Unmarshal(items[0], &sourceText)
			wantText, _ := json.Marshal(sourceText)
			if !bytes.Equal(gotText, wantText) {
				t.Fatal("text item changed")
			}
			paths := map[string]bool{}
			for _, item := range summary.Content[1:] {
				var path string
				_ = json.Unmarshal(item["path"], &path)
				if !filepath.IsAbs(path) || filepath.Ext(path) != ".png" || paths[path] {
					t.Fatalf("invalid/distinct image path %q", path)
				}
				paths[path] = true
				if explicit && filepath.Dir(path) != dir {
					t.Fatal("ignored -o directory")
				}
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, expectedImage) {
					t.Fatal("decoded image changed")
				}
				if _, err := png.Decode(bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				info, _ := os.Stat(path)
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
					t.Fatal("private PNG permissions")
				}
				if _, ok := item["data"]; ok {
					t.Fatal("base64 leaked into summary")
				}
			}
			if len(paths) != 2 {
				t.Fatal("did not save every image")
			}
			out.Reset()
			if err := writeDesktopOutput(res, "", "-", &out, &diag); err != nil || !bytes.Equal(bytes.TrimSpace(out.Bytes()), raw) {
				t.Fatal("raw output changed")
			}
		})
	}
}

func TestOpenCUCLIRawPersonActiveKeepsNoteOnStderr(t *testing.T) {
	var out, diag bytes.Buffer
	raw := json.RawMessage(`{"content":[{"type":"text","text":"PERSON_ACTIVE: stop"}],"isError":true}`)
	err := writeOpenCUOutput(protocol.DesktopResult{OpenCU: &protocol.OpenCUResult{Result: raw}}, "-", &out, &diag)
	if err != nil || !bytes.Equal(bytes.TrimSpace(out.Bytes()), raw) || !strings.Contains(diag.String(), "never retry automatically") {
		t.Fatal("raw output lost human activity note")
	}
}
