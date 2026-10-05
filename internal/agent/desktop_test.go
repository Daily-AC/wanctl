package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/desktop"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/sessionauth"
)

func desktopExchange(t *testing.T, a *Agent, m protocol.Message, supported bool, run desktop.Runner) protocol.Message {
	t.Helper()
	dev, controller := net.Pipe()
	defer dev.Close()
	defer controller.Close()
	_ = controller.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pending := a.doDesktopUsing(dev, "owner", "owner's agent", m, sessionAudit{}, supported, run)
		if pending != nil {
			<-pending
		}
	}()
	reply, err := protocol.ReadMessage(controller)
	if err != nil {
		t.Fatal(err)
	}
	controller.Close()
	<-done
	return reply
}
func TestDesktopPolicyApprovalAuditAndNotificationNeverContainTypedText(t *testing.T) {
	secret := "SYNTHETIC_TYPED_敏感_🚀"
	for _, mode := range []policy.Mode{policy.ModeNormal, policy.ModeBypass} {
		t.Run(string(mode), func(t *testing.T) {
			a := newOptsAgent(t, Options{Mode: mode})
			ap := &recordingApprover{give: policy.Decision{Allow: true}}
			a.setApprover(ap)
			a.notifyPolicy.IncludeDetail = true
			notifications := make(chan []byte, 2)
			a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				notifications <- body
				return notifyHTTPResponse(http.StatusAccepted), nil
			})}
			snap, _ := a.desktop.Store.Put("owner", protocol.DesktopSnapshot{}, time.Now())
			req := &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "type", Text: secret}, {Type: "click", X: 34, Y: 56}}}
			calls := 0
			reply := desktopExchange(t, a, protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: desktop.NewID(), Desktop: req}, true, func(ctx context.Context, job desktop.Job) (protocol.DesktopResult, []byte, error) {
				calls++
				if !a.Busy() {
					t.Error("auto-update did not see active desktop call")
				}
				if job.Request.Actions[0].Text != secret {
					t.Fatal("private helper input lost text")
				}
				return protocol.DesktopResult{Status: "completed", Completed: 2, FailedIndex: -1}, nil, nil
			})
			if reply.Kind != protocol.KindDesktopResult || calls != 1 {
				t.Fatalf("%+v calls=%d", reply, calls)
			}
			if mode == policy.ModeNormal {
				asked := ap.asked()
				if len(asked) != 1 || asked[0].Kind != policy.KindExec || asked[0].Cmd != "act type,click" {
					t.Fatalf("%+v", asked)
				}
			} else if len(ap.asked()) != 0 {
				t.Fatal("bypass prompted")
			}
			events, err := a.log.Read(eventlog.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), secret) || !strings.Contains(string(encoded), "characters") || !strings.Contains(string(encoded), "34,56") {
				t.Fatalf("invalid action audit: %s", encoded)
			}
			select {
			case body := <-notifications:
				if strings.Contains(string(body), secret) || !strings.Contains(string(body), "characters") {
					t.Fatalf("invalid notification: %s", body)
				}
			case <-time.After(time.Second):
				t.Fatal("no completion notification")
			}
		})
	}
	if requiredCapability(protocol.KindDesktop) != sessionauth.Exec {
		t.Fatal("desktop must need exec")
	}
	bad := rejectedRequestEvent("fp", "name", protocol.Message{Kind: protocol.KindDesktop, Action: "act", Command: secret, Desktop: &protocol.DesktopRequest{Actions: []protocol.DesktopAction{{Type: "type", Text: secret}}}}, "no exec capability")
	raw, _ := json.Marshal(bad)
	if strings.Contains(string(raw), secret) {
		t.Fatal("early rejection leaked text")
	}
}
func TestDesktopScreenshotUsesExistingRuleAndWindowsOnlyKeepsConnection(t *testing.T) {
	a := newOptsAgent(t, Options{})
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(*http.Request) (*http.Response, error) { return notifyHTTPResponse(http.StatusAccepted), nil })}
	a.setApprover(policy.DenyApprover{})
	if err := a.engine.Add(policy.Rule{Kind: policy.KindExec, Pattern: "screenshot", Scope: policy.ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	req := protocol.Message{Kind: protocol.KindDesktop, Action: "screenshot", RequestID: desktop.NewID(), Desktop: &protocol.DesktopRequest{}}
	reply := desktopExchange(t, a, req, true, func(context.Context, desktop.Job) (protocol.DesktopResult, []byte, error) {
		return protocol.DesktopResult{Status: "completed", FailedIndex: -1}, nil, nil
	})
	if reply.Kind != protocol.KindDesktopResult {
		t.Fatalf("existing screenshot rule lost: %+v", reply)
	}
	dev, controller := net.Pipe()
	defer dev.Close()
	defer controller.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.doDesktopUsing(dev, "fp", "name", req, sessionAudit{}, false, nil)
		_ = protocol.WriteMessage(dev, protocol.Message{Kind: protocol.KindOK})
	}()
	m, err := protocol.ReadMessage(controller)
	if err != nil || m.Reason != protocol.DesktopWindowsOnly {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = protocol.ReadMessage(controller)
	if err != nil || m.Kind != protocol.KindOK {
		t.Fatal("connection closed on Windows-only error")
	}
	<-done
}
func TestDesktopDisconnectCancelsHelperAndConsumesReference(t *testing.T) {
	a := newOptsAgent(t, Options{Mode: policy.ModeBypass})
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(*http.Request) (*http.Response, error) { return notifyHTTPResponse(http.StatusAccepted), nil })}
	snap, _ := a.desktop.Store.Put("owner", protocol.DesktopSnapshot{}, time.Now())
	req := protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: desktop.NewID(), Desktop: &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "wait", Millis: 10000}}}}
	dev, controller := net.Pipe()
	defer dev.Close()
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		a.doDesktopUsing(dev, "owner", "name", req, sessionAudit{}, true, func(ctx context.Context, job desktop.Job) (protocol.DesktopResult, []byte, error) {
			close(entered)
			<-ctx.Done()
			return protocol.DesktopResult{Status: "partial", Error: "controller disconnected"}, nil, nil
		})
	}()
	<-entered
	controller.Close()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("disconnect did not reach helper")
	}
	if _, err := a.desktop.Store.Get("owner", snap.ID, true, time.Now()); err == nil {
		t.Fatal("disconnect permitted replay")
	}
}
func TestAndroidPermissionHintCrossesChunks(t *testing.T) {
	for _, tc := range []struct {
		command string
		chunks  []string
	}{{"input tap 2 3", []string{"requires INJECT_", "EVENTS permission"}}, {"screencap -p", []string{"Permission ", "denied"}}, {"sh script", []string{"screencap: Permission failure"}}} {
		var output strings.Builder
		w := androidPermissionWriter{out: &output, command: tc.command}
		for _, part := range tc.chunks {
			_, _ = w.Write([]byte(part))
		}
		if !w.denied || output.String() != strings.Join(tc.chunks, "") {
			t.Fatal("permission detection altered output or missed refusal")
		}
	}
	for _, want := range []string{"--elevate", "wanctl screenshot", "wanctl_screenshot"} {
		if !strings.Contains(androidPermissionHint, want) {
			t.Fatalf("missing %s", want)
		}
	}
	w := androidPermissionWriter{out: io.Discard, command: "ls"}
	_, _ = w.Write([]byte("Permission denied"))
	if w.denied {
		t.Fatal("unrelated permission refusal matched")
	}
}

func TestDesktopAuditRecordsSessionWithoutTypedText(t *testing.T) {
	for _, status := range []string{"completed", "rejected", "unknown"} {
		t.Run(status, func(t *testing.T) {
			a := newOptsAgent(t, Options{Mode: policy.ModeBypass})
			snap, _ := a.desktop.Store.Put("owner", protocol.DesktopSnapshot{}, time.Now())
			secret := "PRIVATE_S25_TEXT"
			req := protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: desktop.NewID(), Desktop: &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "type", Text: secret}}}}
			desktopExchange(t, a, req, true, func(ctx context.Context, job desktop.Job) (protocol.DesktopResult, []byte, error) {
				desktop.RecordSession(ctx, 17)
				// No snapshot on locked/crashed helpers; audit cannot rely on one.
				return protocol.DesktopResult{Status: status}, nil, nil
			})
			events, err := a.log.Read(eventlog.Filter{Type: "exec"})
			if err != nil || len(events) != 1 || events[0].DesktopSession == nil || *events[0].DesktopSession != 17 {
				t.Fatalf("%+v %v", events, err)
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("typed text leaked: %s", encoded)
			}
		})
	}
}
