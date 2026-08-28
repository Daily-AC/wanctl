package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/limits"
	"wanctl/internal/sessionauth"
)

type pollResult struct {
	status int
	open   sessionauth.Open
	err    error
}

func startHTTPPoll(t *testing.T, h http.Handler, query string) <-chan pollResult {
	t.Helper()
	ch := make(chan pollResult, 1)
	go func() {
		req := httptest.NewRequest("GET", "/h/poll?"+query, nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		var open sessionauth.Open
		_ = json.NewDecoder(resp.Body).Decode(&open)
		ch <- pollResult{status: resp.Code, open: open}
	}()
	return ch
}

func waitHTTPAgentInst(t *testing.T, r *Relay, key, inst string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.hmu.Lock()
		a := r.hagents[key]
		got := ""
		exists := a != nil
		if a != nil {
			got = a.inst
		}
		r.hmu.Unlock()
		if exists && got == inst {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %s inst did not become %q", key, inst)
}

func dialHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/h/dial?token=tok-alice&target=alice/home-pc", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("dial status = %d, want 200; body=%s", resp.Code, strings.TrimSpace(resp.Body.String()))
	}
	var msg struct{ Session string }
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode dial: %v", err)
	}
	if msg.Session == "" {
		t.Fatal("dial returned empty session")
	}
	return msg.Session
}

func deregisterHTTP(t *testing.T, h http.Handler, query string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/h/deregister?"+query, nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("deregister status = %d, want 200; body=%s", resp.Code, strings.TrimSpace(resp.Body.String()))
	}
}

func TestHTTPPollNewInstanceReplacesOldAndKeepsJobs(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	h := r.Handler()

	aPoll := startHTTPPoll(t, h, "token=tok-alice&device=home-pc&fp=fp-a&inst=A")
	waitHTTPAgentInst(t, r, "alice/home-pc", "A")

	bPoll := startHTTPPoll(t, h, "token=tok-alice&device=home-pc&fp=fp-b&inst=B")
	waitHTTPAgentInst(t, r, "alice/home-pc", "B")

	select {
	case got := <-aPoll:
		if got.err != nil {
			t.Fatalf("A poll error: %v", got.err)
		}
		if got.status != http.StatusConflict {
			t.Fatalf("A status = %d, want 409", got.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A poll was not rejected after B registered")
	}

	wantSession := dialHTTP(t, h)
	select {
	case got := <-bPoll:
		if got.err != nil {
			t.Fatalf("B poll error: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("B status = %d, want 200", got.status)
		}
		if got.open.Session != wantSession {
			t.Fatalf("B session = %q, want %q", got.open.Session, wantSession)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B did not receive queued job")
	}

	aAgain := startHTTPPoll(t, h, "token=tok-alice&device=home-pc&fp=fp-a&inst=A")
	select {
	case got := <-aAgain:
		if got.err != nil {
			t.Fatalf("A second poll error: %v", got.err)
		}
		if got.status != http.StatusConflict {
			t.Fatalf("A second status = %d, want 409", got.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A second poll was not rejected")
	}

	deregisterHTTP(t, h, "token=tok-alice&device=home-pc&inst=A")
	r.hmu.Lock()
	current := r.hagents["alice/home-pc"]
	r.hmu.Unlock()
	if current == nil || current.inst != "B" {
		t.Fatalf("old instance deregister removed current agent: %+v", current)
	}
}

func TestHTTPPollLegacyEmptyInstanceKeepsExistingBehavior(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	h := r.Handler()

	poll := startHTTPPoll(t, h, "token=tok-alice&device=home-pc&fp=fp-legacy")
	waitHTTPAgentInst(t, r, "alice/home-pc", "")
	wantSession := dialHTTP(t, h)

	select {
	case got := <-poll:
		if got.err != nil {
			t.Fatalf("legacy poll error: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("legacy status = %d, want 200", got.status)
		}
		if got.open.Session != wantSession {
			t.Fatalf("legacy session = %q, want %q", got.open.Session, wantSession)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy poll did not receive queued job")
	}
}

func TestHTTPPollReceivesRelayIssuedCapabilities(t *testing.T) {
	r := New(EnvTokenStore("owner-token:owner,reader-token:reader"))
	r.SetACL(staticACL("read"))
	h := r.Handler()
	poll := startHTTPPoll(t, h, "token=owner-token&device=home-pc&inst=A")
	waitHTTPAgentInst(t, r, "owner/home-pc", "A")

	req := httptest.NewRequest("GET", "/h/dial?token=reader-token&target=owner/home-pc", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("dial status = %d, body=%s", resp.Code, resp.Body.String())
	}

	select {
	case got := <-poll:
		if got.status != http.StatusOK || got.open.Capabilities != sessionauth.Read {
			t.Fatalf("poll result = %+v", got)
		}
		if got.open.CallerNamespace != "reader" || got.open.OwnerNamespace != "owner" || got.open.Device != "home-pc" {
			t.Fatalf("poll authorization metadata = %+v", got.open)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive session authorization")
	}
}

func TestHTTPUploadRejectsOversizedBody(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	r.hsess["session"] = &httpSession{toClient: newSideQueue(), toAgent: newSideQueue(), callerNS: "alice", ownerNS: "alice"}
	req := httptest.NewRequest("POST", "/h/up?token=tok-alice&session=session&role=client",
		bytes.NewReader(make([]byte, limits.RelayHTTPUploadBytes+1)))
	resp := httptest.NewRecorder()
	r.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status = %d, want %d", resp.Code, http.StatusRequestEntityTooLarge)
	}
	select {
	case queued := <-r.hsess["session"].toAgent.ch:
		t.Fatalf("oversized body was queued: %d bytes", len(queued))
	default:
	}
}
