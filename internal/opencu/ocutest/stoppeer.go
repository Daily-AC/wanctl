package ocutest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// BuildStopPeer builds a peer that announces the person's stop the way the
// Android engine does: an MCP log notification whose data is
// {"event":"stop","error":{"code":"USER_STOPPED",...}}. Every tool call it
// answers succeeds, and with OPENCU_TEST_LOG set it appends each request's
// method to that file. OPENCU_TEST_STOP picks the behaviour:
//
//	after-call           stop right after answering a call (host idle)
//	delayed              stop 300 ms after answering a call
//	hang                 stop on a call and never answer it
//	garbage              a non-JSON line after answering, then a stop
//	exit-after-call      exit right after answering a call
//	on-exit              stop once stdin ends
//	late-stop-on-exit    once stdin ends, exit; a child still holding stdout
//	                     writes a 1 MiB log notification and a stop 200 ms later
//	late-output-on-exit  the same, without the stop
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
import("bufio";"encoding/json";"os";"os/exec";"strings";"sync";"time")
func main(){
 when:=os.Getenv("OPENCU_TEST_STOP");var mu sync.Mutex;enc:=json.NewEncoder(os.Stdout)
 write:=func(v any){mu.Lock();enc.Encode(v);mu.Unlock()}
 stop:=map[string]any{"jsonrpc":"2.0","method":"notifications/message","params":map[string]any{"level":"warning","logger":"opencu","data":map[string]any{"event":"stop","error":map[string]any{"code":"USER_STOPPED","message":"Three physical volume-down presses. Do not reconnect without explicit permission."}}}}
 if strings.HasPrefix(when,"child-"){
  time.Sleep(200*time.Millisecond)
  write(map[string]any{"jsonrpc":"2.0","method":"notifications/message","params":map[string]any{"level":"debug","logger":"opencu","data":strings.Repeat("x",1<<20)}})
  if when=="child-stop"{write(stop)}
  return
 }
 logf:=func(m string){if p:=os.Getenv("OPENCU_TEST_LOG");p!=""{f,err:=os.OpenFile(p,os.O_APPEND|os.O_CREATE|os.O_WRONLY,0600);if err==nil{f.WriteString(m+"\n");f.Close()}}}
 scan:=bufio.NewScanner(os.Stdin)
 for scan.Scan(){
  var req struct{ID json.RawMessage;Method string;Params struct{ProtocolVersion string}}
  if json.Unmarshal(scan.Bytes(),&req)!=nil{os.Exit(2)}
  logf(req.Method)
  switch req.Method{
  case "initialize":write(map[string]any{"jsonrpc":"2.0","id":req.ID,"result":map[string]any{"protocolVersion":req.Params.ProtocolVersion,"serverInfo":map[string]any{"name":"stop-peer","version":"0"}}})
  case "tools/call":
   if when=="hang"{write(stop);continue}
   write(map[string]any{"jsonrpc":"2.0","id":req.ID,"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":"ok"}},"_meta":map[string]any{"pid":os.Getpid()}}})
   switch when{
   case "after-call":write(stop)
   case "delayed":go func(){time.Sleep(300*time.Millisecond);write(stop)}()
   case "garbage":mu.Lock();os.Stdout.WriteString("not json\n");mu.Unlock();go func(){time.Sleep(100*time.Millisecond);write(stop)}()
   case "exit-after-call":os.Exit(0)
   }
  }
 }
 switch when{
 case "on-exit":write(stop)
 case "late-stop-on-exit","late-output-on-exit":
  child:="child-quiet";if when=="late-stop-on-exit"{child="child-stop"}
  self,_:=os.Executable();cmd:=exec.Command(self);cmd.Env=append(os.Environ(),"OPENCU_TEST_STOP="+child);cmd.Stdout=os.Stdout;cmd.Start()
 }
}
`
