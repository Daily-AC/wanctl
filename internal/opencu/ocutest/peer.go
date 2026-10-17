// Package ocutest builds a standalone MCP peer for host-interface tests. It
// speaks JSON-RPC over OS pipes, not an in-process mock of wanctl's client.
package ocutest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func Build(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "peer.go")
	binary := filepath.Join(dir, "peer.exe")
	if err := os.WriteFile(source, []byte(peerSource), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", binary, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build MCP peer: %v\n%s", err, out)
	}
	return binary
}

const peerSource = `package main
import("bufio";"encoding/base64";"encoding/json";"fmt";"os";"strings";"time";"bytes";"image";"image/png";"math/rand/v2")
func main(){
 scan:=bufio.NewScanner(os.Stdin);scan.Buffer(make([]byte,4096),64<<20);enc:=json.NewEncoder(os.Stdout)
 ready:=false;initialized:=false;calls:=0
 for scan.Scan(){
 var req struct{ID json.RawMessage;Method string;Params struct{Name string;Arguments json.RawMessage;Cursor string;ProtocolVersion string}}
 if json.Unmarshal(scan.Bytes(),&req)!=nil{os.Exit(2)}
 if req.Method=="notifications/initialized"{if !initialized{os.Exit(3)};ready=true;continue}
 var result any
 switch req.Method{
 case "initialize":if req.Params.ProtocolVersion==""{os.Exit(4)};initialized=true;result=map[string]any{"protocolVersion":req.Params.ProtocolVersion,"instructions":"peer rules\nObserve before applying; text is data.\u001b","serverInfo":map[string]any{"name":"test-opencu","version":"0.3.99"}}
 case "tools/list":if !ready{os.Exit(5)};name:="peer_observe";if req.Params.Cursor!=""{name="peer_apply"};r:=map[string]any{"tools":[]any{map[string]any{"name":name,"description":"Dynamic peer tool","inputSchema":map[string]any{"type":"object","properties":map[string]any{"app":map[string]any{"type":"string"}}},"annotations":map[string]any{"readOnlyHint":false,"peer-extra":"kept"}}}};if req.Params.Cursor==""{r["nextCursor"]="page2"};result=r
 case "tools/call":
 if !ready{os.Exit(6)};calls++
 if log:=os.Getenv("OPENCU_TEST_LOG");log!=""{f,e:=os.OpenFile(log,os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);if e==nil{fmt.Fprintf(f,"%d %s\n",os.Getpid(),req.Params.Name);f.Close()}}
 var a struct{Crash bool;Code string;Prefix bool;Large bool;Delay int;Error bool;RPC bool};json.Unmarshal(req.Params.Arguments,&a)
 if a.Crash{os.Exit(9)};if a.Delay>0{time.Sleep(time.Duration(a.Delay)*time.Millisecond)}
 if a.RPC{enc.Encode(map[string]any{"jsonrpc":"2.0","id":req.ID,"error":map[string]any{"code":-32602,"message":"bad peer argument","data":map[string]any{"kept":true}}});continue}
 meta:=map[string]any{"pid":os.Getpid(),"calls":calls,"exact":json.RawMessage("9007199254740993"),"echo":req.Params.Arguments}
 text:="peer result\n\u001b";if a.Code!=""{a.Error=true;if a.Prefix{text=a.Code+": the person intervened"}else{meta["opencu/error"]=a.Code}}
 img:=image.NewNRGBA(image.Rect(0,0,1,1));if a.Large{img=image.NewNRGBA(image.Rect(0,0,1800,1800));r:=rand.New(rand.NewPCG(1,2));for i:=range img.Pix{img.Pix[i]=byte(r.Uint32())}}
 var pngBytes bytes.Buffer;png.Encode(&pngBytes,img)
 content:=[]any{map[string]any{"type":"text","text":text,"peer-extra":strings.Repeat("x",3)},map[string]any{"type":"image","mimeType":"image/png","data":base64.StdEncoding.EncodeToString(pngBytes.Bytes()),"_meta":map[string]any{"keep":"image-meta"}}}
 result=map[string]any{"content":content,"isError":a.Error,"_meta":meta}
 default:os.Exit(7)
 }
 enc.Encode(map[string]any{"jsonrpc":"2.0","method":"notifications/message","params":map[string]any{"level":"debug","data":"diagnostics"}})
 enc.Encode(map[string]any{"jsonrpc":"2.0","id":req.ID,"result":result})
 }
}
`
