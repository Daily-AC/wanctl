package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

const captureToken = "secret-in-uri"

type capturedAdmissionRequest struct {
	path, requestURI, rawQuery, authorization string
}

type admissionCapture struct {
	next http.Handler
	mu   sync.Mutex
	reqs []capturedAdmissionRequest
}

func (c *admissionCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, capturedAdmissionRequest{
		path: r.URL.Path, requestURI: r.RequestURI, rawQuery: r.URL.RawQuery,
		authorization: r.Header.Get("Authorization"),
	})
	c.mu.Unlock()
	c.next.ServeHTTP(w, r)
}

func (c *admissionCapture) assertBearerOnly(t *testing.T, requiredPaths ...string) {
	t.Helper()
	c.mu.Lock()
	reqs := append([]capturedAdmissionRequest(nil), c.reqs...)
	c.mu.Unlock()
	for _, req := range reqs {
		if strings.Contains(req.requestURI, captureToken) || strings.Contains(req.rawQuery, "token=") {
			t.Errorf("credential appeared in request URI for %s", req.path)
		}
		if req.authorization != "Bearer "+captureToken {
			t.Errorf("%s did not carry the expected bearer credential", req.path)
		}
	}
	for _, path := range requiredPaths {
		found := false
		for _, req := range reqs {
			if strings.HasPrefix(req.path, path) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("did not capture %s", path)
		}
	}
}

func TestWebSocketTransportUsesBearerHeaderWithoutTokenURL(t *testing.T) {
	capture, srv := startCapturedRelay(t)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := startCapturedAgentAndClient(t, base, "ws")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Peers(ctx); err != nil {
		t.Fatal(err)
	}
	if code, err := c.Exec(ctx, "home-pc", "true", true, ""); err != nil || code != 0 {
		t.Fatalf("exec: code=%d err=%v", code, err)
	}
	capture.assertBearerOnly(t, "/agent", "/peers", "/dial", "/session/")
}

func TestHTTPTransportUsesBearerHeaderWithoutTokenURL(t *testing.T) {
	capture, srv := startCapturedRelay(t)
	c := startCapturedAgentAndClient(t, srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Peers(ctx); err != nil {
		t.Fatal(err)
	}
	if code, err := c.Exec(ctx, "home-pc", "true", true, ""); err != nil || code != 0 {
		t.Fatalf("exec: code=%d err=%v", code, err)
	}
	capture.assertBearerOnly(t, "/h/poll", "/h/peers", "/h/dial", "/h/up", "/h/down", "/h/close")
}

func startCapturedRelay(t *testing.T) (*admissionCapture, *httptest.Server) {
	t.Helper()
	r := relay.New(relay.EnvTokenStore(captureToken + ":alice"))
	capture := &admissionCapture{next: r.Handler()}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)
	return capture, srv
}

func startCapturedAgentAndClient(t *testing.T, relayURL, transportName string) *Client {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	agentID, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		RelayURL: relayURL, Token: captureToken, Name: "home-pc", Transport: transportName,
		AutoYes: true, Mode: policy.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)
	if transportName == "http" {
		time.Sleep(300 * time.Millisecond)
	} else {
		time.Sleep(200 * time.Millisecond)
	}

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	c := NewWith(id, known, relayURL, captureToken, transportName)
	if _, err := c.PinServer(context.Background(), "alice/home-pc", agentID.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	return c
}
