package agent

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/desktop"
	"wanctl/internal/eventlog"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

type agentOpenCUPeer struct {
	*opencu.Client
	closed *atomic.Int32
}

func (p *agentOpenCUPeer) Close() { p.Client.Close(); p.closed.Add(1) }
func (p *agentOpenCUPeer) Do(ctx context.Context, r protocol.DesktopRequest) (protocol.OpenCUResult, error) {
	var out protocol.OpenCUResult
	var err error
	if r.Discover {
		out.Discovery, err = p.Discover(ctx)
	} else {
		out.Result, err = p.Call(ctx, r.Tool, r.Arguments)
	}
	return out, err
}
func openCUExchange(t *testing.T, a *Agent, fp string, req protocol.DesktopRequest, start desktop.OpenCUStart, checks ...func() bool) protocol.DesktopResult {
	t.Helper()
	dev, ctl := net.Pipe()
	defer dev.Close()
	defer ctl.Close()
	ctl.SetDeadline(time.Now().Add(15 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pending := a.doOpenCU(dev, fp, "test agent", protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: desktop.NewID(), Desktop: &req}, sessionAudit{}, start, checks...)
		if pending != nil {
			<-pending
		}
	}()
	res, _, err := desktop.ReadResult(ctl)
	if err != nil {
		t.Fatal(err)
	}
	ctl.Close()
	<-done
	return res
}
func TestOpenCUFreshApprovalAuditOwnershipAndClose(t *testing.T) {
	binary := ocutest.Build(t)
	for _, mode := range []policy.Mode{policy.ModeNormal, policy.ModeBypass} {
		for _, code := range []string{"USER_STOPPED", "STOPPED_BY_PERSON"} {
			t.Run(string(mode)+"/"+code, func(t *testing.T) {
				a := newOptsAgent(t, Options{Mode: mode})
				ap := &recordingApprover{give: policy.Decision{Allow: true}}
				a.setApprover(ap)
				id := "w-" + desktop.NewID()
				w, err := a.openWorkspace("owner", protocol.Message{WorkspaceID: id, Path: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				var starts, closed atomic.Int32
				start := func(ctx context.Context) (desktop.OpenCUConnection, error) {
					c, e := opencu.Start(ctx, exec.Command(binary))
					if e != nil {
						return nil, e
					}
					starts.Add(1)
					return &agentOpenCUPeer{c, &closed}, nil
				}
				summary := "opencu peer_apply Disposable"
				a.engine.Add(policy.Rule{Kind: policy.KindExec, Pattern: summary, Scope: policy.ScopeGlobal})
				req := protocol.DesktopRequest{WorkspaceID: id, Tool: "peer_apply", Arguments: json.RawMessage(`{"app":"Disposable","text":"PRIVATE_TYPED_TEXT","paste":"PRIVATE_PASTED_TEXT","code":"` + code + `"}`)}
				first := openCUExchange(t, a, "owner", req, start)
				if first.OpenCU == nil || opencu.ErrorCode(first.OpenCU.Result) != code || starts.Load() != 1 {
					t.Fatalf("%+v", first)
				}
				if len(ap.asked()) != 0 {
					t.Fatal("ordinary saved/bypass policy ignored")
				}
				a.setApprover(policy.DenyApprover{})
				req.Arguments = json.RawMessage(`{"app":"Disposable","text":"PRIVATE_TYPED_TEXT"}`)
				deny := openCUExchange(t, a, "owner", req, start)
				if deny.OpenCU == nil || deny.OpenCU.Code != "OPENCU_STOPPED" || starts.Load() != 1 {
					t.Fatalf("stop bypassed: %+v", deny)
				}
				a.setApprover(ap)
				resumed := openCUExchange(t, a, "owner", req, start)
				if resumed.Status != "completed" || starts.Load() != 2 || len(ap.asked()) != 1 || ap.asked()[0].Cmd != summary {
					t.Fatalf("fresh approval missing: %+v", resumed)
				}
				// A different authenticated controller cannot inherit this connection.
				foreign := openCUExchange(t, a, "other", req, start)
				if foreign.Status != "rejected" || starts.Load() != 2 {
					t.Fatal("workspace ownership bypassed")
				}
				if a.Busy() {
					t.Fatal("idle workspace/OpenCU blocks update")
				}
				release := a.PrepareDesktopUpdate()
				release()
				if closed.Load() < 2 {
					t.Fatal("update did not close idle peer")
				}
				again := openCUExchange(t, a, "owner", req, start)
				if again.Status != "completed" || starts.Load() != 3 {
					t.Fatal("failed update recovery")
				}
				events, err := a.log.Read(eventlog.Filter{Type: "exec"})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(events)
				if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "image/png") || !strings.Contains(string(raw), summary) || !strings.Contains(string(raw), "resumed by person") {
					t.Fatalf("audit: %s", raw)
				}
				w.stop()
				a.desktop.CloseOpenCUSession("owner:" + id)
				ended := openCUExchange(t, a, "owner", req, start)
				if ended.Status != "rejected" || starts.Load() != 3 {
					t.Fatal("closed workspace relaunched peer")
				}
			})
		}
	}
}
func TestOpenCUPendingApprovalCannotOutliveWorkspace(t *testing.T) {
	a := newOptsAgent(t, Options{})
	id := "w-" + desktop.NewID()
	w, err := a.openWorkspace("owner", protocol.Message{WorkspaceID: id, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a.setApprover(openCUApproverFunc(func(policy.Request) policy.Decision { w.stop(); return policy.Decision{Allow: true} }))
	req := protocol.DesktopRequest{WorkspaceID: id, Tool: "anything", Arguments: json.RawMessage(`{}`)}
	res := openCUExchange(t, a, "owner", req, func(context.Context) (desktop.OpenCUConnection, error) {
		t.Error("launched after workspace ended")
		return nil, nil
	})
	if res.Status != "rejected" {
		t.Fatalf("%+v", res)
	}
}

type openCUApproverFunc func(policy.Request) policy.Decision

func (f openCUApproverFunc) Ask(r policy.Request) policy.Decision { return f(r) }
