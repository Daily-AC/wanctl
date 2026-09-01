package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/policy"
)

type notifyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f notifyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func notifyHTTPResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Body: io.NopCloser(strings.NewReader("failure")), Header: make(http.Header),
	}
}

func TestIncludeDetailOffRemovesCommandBeforeAgentReport(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "https://relay.example", Token: "token", Name: "legion"})
	if err != nil {
		t.Fatal(err)
	}
	bodies := make(chan []byte, 1)
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		bodies <- body
		return notifyHTTPResponse(http.StatusAccepted), nil
	})}
	a.notifyApproval(console.Pending{
		ID: "approval-1", Cmd: "deploy --token xxx", Peer: "macbook", Created: time.Now(),
	})
	select {
	case body := <-bodies:
		if strings.Contains(string(body), "deploy") || strings.Contains(string(body), "xxx") || strings.Contains(string(body), "macbook") {
			t.Fatalf("detail-off report leaked device content: %s", body)
		}
		var report agentEventReport
		if err := json.Unmarshal(body, &report); err != nil || report.Event != "approval.pending" {
			t.Fatalf("report = %+v, err = %v", report, err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not report approval")
	}
}

func TestIncludeDetailOnRedactsCommand(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "https://relay.example", Token: "token", Name: "legion"})
	if err != nil {
		t.Fatal(err)
	}
	a.notifyPolicy.IncludeDetail = true
	bodies := make(chan []byte, 1)
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		bodies <- body
		return notifyHTTPResponse(http.StatusAccepted), nil
	})}
	a.notifyApproval(console.Pending{ID: "approval-2", Cmd: "deploy --token xxx", Created: time.Now()})
	select {
	case body := <-bodies:
		if strings.Contains(string(body), "xxx") || !strings.Contains(string(body), `deploy --token [REDACTED]`) {
			t.Fatalf("detail-on report was not redacted: %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not report approval")
	}
}

func TestRelay500DoesNotBlockApprovalDecision(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "https://relay.example", Token: "token", Name: "legion"})
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan struct{}, 3)
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		reports <- struct{}{}
		return notifyHTTPResponse(http.StatusInternalServerError), nil
	})}
	_, unsubscribe := a.console.Subscribe()
	defer unsubscribe()
	decision := make(chan struct {
		ok   bool
		name string
	}, 1)
	go func() {
		ok, name := a.gate(policy.Request{Kind: policy.KindExec, Cmd: "deploy --token xxx"})
		decision <- struct {
			ok   bool
			name string
		}{ok, name}
	}()
	select {
	case <-reports:
	case <-time.After(time.Second):
		t.Fatal("approval event was not reported")
	}
	var id string
	deadline := time.Now().Add(time.Second)
	for id == "" && time.Now().Before(deadline) {
		pending := a.console.State().Pending
		if len(pending) > 0 {
			id = pending[0].ID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" || !a.console.Decide(id, "y") {
		t.Fatal("could not decide pending approval")
	}
	select {
	case got := <-decision:
		if !got.ok || got.name != "approved" {
			t.Fatalf("gate decision = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook 500 blocked the approval flow")
	}
}
