package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestOpenCUCLIResultFileAndRawOutput(t *testing.T) {
	c, err := opencu.Start(context.Background(), exec.Command(ocutest.Build(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.Call(context.Background(), "peer_apply", json.RawMessage(`{"error":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res := &client.DesktopResponse{Result: protocol.DesktopResult{Status: "completed", OpenCU: &protocol.OpenCUResult{Result: raw}}}
	var out, diag bytes.Buffer
	path := filepath.Join(t.TempDir(), "result.json")
	if err := writeDesktopOutput(res, "", path, &out, &diag); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(kept, raw) {
		t.Fatal("full MCP result changed")
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("private result permissions")
	}
	if !strings.Contains(out.String(), `"bytes":`) || strings.Contains(out.String(), `"data":`) || !strings.Contains(out.String(), `"isError":true`) || !strings.Contains(out.String(), "9007199254740993") {
		t.Fatal("summary lost fidelity")
	}
	out.Reset()
	if err := writeDesktopOutput(res, "", "-", &out, &diag); err != nil || !bytes.Equal(bytes.TrimSpace(out.Bytes()), raw) {
		t.Fatal("raw output changed")
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
