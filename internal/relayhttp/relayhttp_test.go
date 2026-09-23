package relayhttp

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// relay stands in for a relay behind a CDN: HTTP/2 over TLS on a TCP port and,
// optionally, HTTP/3 on the same UDP port, both serving the same handler.
type relay struct {
	host    string
	tlsConf *tls.Config
	h3      *http3.Server

	mu     sync.Mutex
	protos []string // r.Proto of every non-probe request, in arrival order
	bodies map[string]int
}

func newRelay(t *testing.T, withH3 bool) *relay {
	t.Helper()
	r := &relay{bodies: map[string]int{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == probePath {
			w.Write([]byte("ok"))
			return
		}
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.protos = append(r.protos, req.Proto)
		if len(body) > 0 {
			r.bodies[string(body)]++
		}
		r.mu.Unlock()
		w.Write([]byte(req.Proto))
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.host = srv.Listener.Addr().String()
	r.tlsConf = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	if withH3 {
		pc, err := net.ListenPacket("udp", r.host)
		if err != nil {
			t.Fatalf("listen udp on %s: %v", r.host, err)
		}
		r.h3 = &http3.Server{Handler: handler, TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: srv.TLS.Certificates})}
		go r.h3.Serve(pc)
		t.Cleanup(func() { r.h3.Close() })
	}
	return r
}

func (r *relay) lastProto() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.protos) == 0 {
		return ""
	}
	return r.protos[len(r.protos)-1]
}

func get(t *testing.T, tr http.RoundTripper, host string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/h/down", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func newTestTransport(r *relay) *Transport {
	tr := New(r.tlsConf)
	tr.disabled = false // independent of the developer's environment
	tr.proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	return tr
}

// waitH3 issues requests until one arrives over HTTP/3.
func waitH3(t *testing.T, tr *Transport, r *relay) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if get(t, tr, r.host) == "HTTP/3.0" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never moved to HTTP/3; last proto %q", r.lastProto())
}

func TestStartsOnHTTP2AndMovesToHTTP3(t *testing.T) {
	r := newRelay(t, true)
	tr := newTestTransport(r)
	defer tr.CloseIdleConnections()
	// The first request must not wait for QUIC: it goes out on HTTP/2 while
	// the probe runs beside it.
	if got := get(t, tr, r.host); got != "HTTP/2.0" {
		t.Fatalf("first request went over %s, want HTTP/2.0", got)
	}
	waitH3(t, tr, r)
	for i := 0; i < 3; i++ {
		if got := get(t, tr, r.host); got != "HTTP/3.0" {
			t.Fatalf("request after the probe went over %s", got)
		}
	}
}

func TestWithoutUDPEveryRequestStaysFastOnHTTP2(t *testing.T) {
	r := newRelay(t, false)
	tr := newTestTransport(r)
	defer tr.CloseIdleConnections()
	for i := 0; i < 5; i++ {
		start := time.Now()
		if got := get(t, tr, r.host); got != "HTTP/2.0" {
			t.Fatalf("request %d went over %s", i, got)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("request %d took %v: a failing probe must not delay traffic", i, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHTTP3FailureFallsBackWithoutReplayingBodies(t *testing.T) {
	r := newRelay(t, true)
	tr := newTestTransport(r)
	defer tr.CloseIdleConnections()
	waitH3(t, tr, r)
	r.h3.Close()

	// A GET changes nothing on the relay, so it is re-sent over HTTP/2 within
	// the same call and the caller never sees the failure.
	if got := get(t, tr, r.host); got != "HTTP/2.0" {
		t.Fatalf("GET after HTTP/3 died went over %s", got)
	}
	// Later requests, bodies included, go straight to HTTP/2 and arrive once.
	req, _ := http.NewRequest(http.MethodPost, "https://"+r.host+"/h/up", strings.NewReader("chunk-1"))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("POST after fallback: %v", err)
	}
	resp.Body.Close()
	if got := r.lastProto(); got != "HTTP/2.0" {
		t.Fatalf("POST after fallback went over %s", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.bodies["chunk-1"]; n != 1 {
		t.Fatalf("upload body arrived %d times, want exactly once", n)
	}
}

func TestPOSTIsNotResentWhenHTTP3FailsMidRequest(t *testing.T) {
	r := newRelay(t, true)
	tr := newTestTransport(r)
	defer tr.CloseIdleConnections()
	waitH3(t, tr, r)
	r.h3.Close()

	req, _ := http.NewRequest(http.MethodPost, "https://"+r.host+"/h/up", strings.NewReader("chunk-2"))
	resp, err := tr.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
	}
	r.mu.Lock()
	n := r.bodies["chunk-2"]
	r.mu.Unlock()
	if n > 1 {
		t.Fatalf("upload body arrived %d times: an /h/up must never be replayed", n)
	}
	if got := get(t, tr, r.host); got != "HTTP/2.0" {
		t.Fatalf("request after a failed HTTP/3 POST went over %s", got)
	}
}

func TestConfiguredProxyKeepsHTTP2(t *testing.T) {
	r := newRelay(t, true)
	tr := newTestTransport(r)
	defer tr.CloseIdleConnections()
	proxyURL, _ := url.Parse("http://proxy.invalid:3128")
	tr.proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	req, _ := http.NewRequest(http.MethodGet, "https://"+r.host+"/h/down", nil)
	if tr.eligible(req) {
		t.Fatal("HTTP/3 would bypass the configured proxy")
	}
}

func TestEnvironmentCanDisableHTTP3(t *testing.T) {
	t.Setenv("WANCTL_HTTP3", "0")
	r := newRelay(t, true)
	tr := New(r.tlsConf)
	defer tr.CloseIdleConnections()
	tr.proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	for i := 0; i < 10; i++ {
		if got := get(t, tr, r.host); got != "HTTP/2.0" {
			t.Fatalf("WANCTL_HTTP3=0 but request went over %s", got)
		}
		time.Sleep(30 * time.Millisecond)
	}
}
