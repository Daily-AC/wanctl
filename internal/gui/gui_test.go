package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
)

func newEngine(t *testing.T) *policy.Engine {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	e, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAskResolvedByDecideRecordsRule(t *testing.T) {
	eng := newEngine(t)
	s := New(eng, nil, Info{Device: "dev"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Ask blocks; resolve it from the "browser" with verdict g (remember global).
	done := make(chan policy.Decision, 1)
	go func() {
		done <- s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "make build", Peer: "fp"})
	}()

	// Wait until the pending request shows up in state.
	var id string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := http.Get(srv.URL + "/api/state")
		var st struct {
			Pending []struct{ ID string } `json:"pending"`
		}
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if len(st.Pending) == 1 {
			id = st.Pending[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("pending request never appeared in state")
	}

	body, _ := json.Marshal(map[string]string{"id": id, "verdict": "g"})
	http.Post(srv.URL+"/api/decide", "application/json", strings.NewReader(string(body)))

	select {
	case d := <-done:
		// The GUI returns the decision; the caller (agent gate) records the rule.
		if !d.Allow || !d.Remember || d.Scope != policy.ScopeGlobal {
			t.Fatalf("expected allow+remember+global, got %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after decide")
	}
}

func TestAskTimesOutToDeny(t *testing.T) {
	eng := newEngine(t)
	s := New(eng, nil, Info{Device: "dev"})
	s.timeout = 150 * time.Millisecond // shorten for the test
	d := s.Ask(policy.Request{Kind: policy.KindWrite, Path: "/x"})
	if d.Allow {
		t.Fatal("expected deny on timeout")
	}
}

func TestStateServesInfoAndMode(t *testing.T) {
	eng := newEngine(t)
	s := New(eng, nil, Info{Device: "dev", Relay: "wss://r"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["mode"] != "normal" {
		t.Fatalf("mode=%v", st["mode"])
	}
	if _, ok := st["info"]; !ok {
		t.Fatal("missing info")
	}
}

var _ = context.Background
