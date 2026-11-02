package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"wanctl/internal/protocol"
)

func TestOpenCUAndroidJPEGOutputPreservesBytesAndMetadata(t *testing.T) {
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "image", "mimeType": "image/jpeg", "data": base64.StdEncoding.EncodeToString(jpegBytes.Bytes()), "_meta": map[string]any{"android": true}}}, "_meta": map[string]any{"exact": json.RawMessage("9007199254740993")}})
	res := protocol.DesktopResult{OpenCU: &protocol.OpenCUResult{Result: raw}}
	var stdout bytes.Buffer
	if err := writeOpenCUOutput(res, t.TempDir(), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Content []struct {
			Path, MimeType string
			Meta           json.RawMessage `json:"_meta"`
		}
		ResultFile string `json:"result_file"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 || filepath.Ext(got.Content[0].Path) != ".jpg" || got.Content[0].MimeType != "image/jpeg" || string(got.Content[0].Meta) != `{"android":true}` {
		t.Fatalf("%s", stdout.Bytes())
	}
	data, err := os.ReadFile(got.Content[0].Path)
	if err != nil || !bytes.Equal(data, jpegBytes.Bytes()) {
		t.Fatal("JPEG bytes changed", err)
	}
	st, _ := os.Stat(got.Content[0].Path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("image permissions")
	}
	saved, err := os.ReadFile(got.ResultFile)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatal("raw JSON changed")
	}
	if !strings.Contains(stdout.String(), "9007199254740993") {
		t.Fatal("integer lost precision")
	}
}
