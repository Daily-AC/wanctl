package ocutest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// BuildStopPeer builds a peer that announces the person's stop the way the
// Android engine does: an MCP log notification whose data is
// {"event":"stop","error":{"code":"USER_STOPPED",...}}. OPENCU_TEST_STOP picks
// when: "after-call" right after answering a tool call, while the host is idle;
// "on-exit" only once its stdin ends. Every tool call succeeds.
func BuildStopPeer(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "stoppeer.go")
	binary := filepath.Join(dir, "stoppeer.exe")
	if err := os.WriteFile(source, []byte(stopPeerSource), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build stop peer: %v\n%s", err, out)
	}
	return binary
}

const stopPeerSource = `package main
import("bufio";"encoding/json";"os")
func main(){
 scan:=bufio.NewScanner(os.Stdin);enc:=json.NewEncoder(os.Stdout);when:=os.Getenv("OPENCU_TEST_STOP")
 stop:=map[string]any{"jsonrpc":"2.0","method":"notifications/message","params":map[string]any{"level":"warning","logger":"opencu","data":map[string]any{"event":"stop","error":map[string]any{"code":"USER_STOPPED","message":"Three physical volume-down presses. Do not reconnect without explicit permission."}}}}
 for scan.Scan(){
  var req struct{ID json.RawMessage;Method string;Params struct{ProtocolVersion string}}
  if json.Unmarshal(scan.Bytes(),&req)!=nil{os.Exit(2)}
  switch req.Method{
  case "initialize":enc.Encode(map[string]any{"jsonrpc":"2.0","id":req.ID,"result":map[string]any{"protocolVersion":req.Params.ProtocolVersion,"serverInfo":map[string]any{"name":"stop-peer","version":"0"}}})
  case "tools/call":
   enc.Encode(map[string]any{"jsonrpc":"2.0","id":req.ID,"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":"ok"}},"_meta":map[string]any{"pid":os.Getpid()}}})
   if when=="after-call"{enc.Encode(stop)}
  }
 }
 if when=="on-exit"{enc.Encode(stop)}
}
`
