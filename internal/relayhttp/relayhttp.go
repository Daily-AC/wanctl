// Package relayhttp is the HTTP transport for traffic between a wanctl node and
// its relay: the controller's /resolve and /h/dial, both ends' /h/up and
// /h/down, and the agent's /h/poll.
//
// It prefers HTTP/3 because some access networks shape TLS over TCP to the
// relay's CDN per connection. Measured on 2026-09-23 from a China Mobile home
// line to Cloudflare's HKG edge: TLS over TCP ran at 20-190 KB/s on every port
// tried, with and without ECH, and eight parallel connections only added up;
// plain HTTP on the same IP ran at 12-25 MB/s and HTTP/3 at 9-42 MB/s. A push
// that should take seconds took minutes, and WebSocket is no way out because
// the CDN carries it over TCP too.
//
// HTTP/3 is never on the critical path. A host starts on HTTP/2 while one
// background request checks whether QUIC reaches it; requests move to HTTP/3
// only after that succeeds. A network that drops UDP therefore costs nothing
// beyond the probe, instead of a handshake timeout on every command. An HTTP/3
// failure moves the host back to HTTP/2 and the probe is retried later.
package relayhttp

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	// probePath is answered by every relay. Any HTTP response proves the path
	// works; the status is irrelevant.
	probePath    = "/healthz"
	probeTimeout = 5 * time.Second
	// retryAfter is how long a host stays on HTTP/2 after a failed probe or an
	// HTTP/3 error, so a network without UDP is not probed on every request.
	retryAfter = 5 * time.Minute

	// A down poll parks on the relay for its whole poll window before
	// answering, so only the wait for response *headers* can be bounded
	// tightly. A blanket request timeout on a slow link fired while a response
	// body was still downloading and cut the body in half (issue #57).
	responseHeaderTimeout = 45 * time.Second
)

// Transport is an http.RoundTripper that sends each request over HTTP/3 when
// the relay is known to be reachable that way and over HTTP/2 otherwise.
type Transport struct {
	h2 *http.Transport
	h3 *http3.Transport

	// disabled turns HTTP/3 off entirely (WANCTL_HTTP3=0). Fallback covers a
	// network that blocks UDP; this covers one that passes it but shapes it
	// worse than TCP, which no probe can tell apart from a working path.
	disabled bool
	proxy    func(*http.Request) (*url.URL, error)
	now      func() time.Time

	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	h3      bool
	probing bool
	retryAt time.Time
}

var shared = sync.OnceValue(func() *Transport { return New(nil) })

// Shared returns the process-wide transport. Sharing it is what lets a CLI
// command's session reuse the connection its /resolve and /h/dial opened, and
// an agent's sessions reuse the one its /h/poll loop keeps warm.
func Shared() *Transport { return shared() }

// New builds a transport. tlsConf is nil in production; tests pass one that
// trusts their own certificate.
func New(tlsConf *tls.Config) *Transport {
	h2 := http.DefaultTransport.(*http.Transport).Clone()
	h2.ResponseHeaderTimeout = responseHeaderTimeout
	if tlsConf != nil {
		h2.TLSClientConfig = tlsConf.Clone()
	}
	h3 := &http3.Transport{
		QUICConfig: &quic.Config{
			// Long polls sit idle on the connection for up to 25 s; keep-alives
			// hold the NAT mapping open underneath them.
			KeepAlivePeriod:      10 * time.Second,
			MaxIdleTimeout:       60 * time.Second,
			HandshakeIdleTimeout: probeTimeout,
		},
	}
	if tlsConf != nil {
		h3.TLSClientConfig = tlsConf.Clone()
	}
	return &Transport{
		h2:       h2,
		h3:       h3,
		disabled: os.Getenv("WANCTL_HTTP3") == "0",
		proxy:    http.ProxyFromEnvironment,
		now:      time.Now,
		hosts:    map[string]*hostState{},
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.eligible(req) || !t.useH3(req.URL.Host) {
		return t.h2.RoundTrip(req)
	}
	resp, err := t.h3.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if req.Context().Err() != nil {
		return nil, err
	}
	t.demote(req.URL.Host)
	// Only a request that cannot have changed anything is sent again. An
	// /h/up body may already sit in the relay's queue, and repeating it would
	// duplicate bytes in the middle of the end-to-end TLS stream.
	if (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Body == nil {
		return t.h2.RoundTrip(req)
	}
	return nil, err
}

// eligible reports whether HTTP/3 may be tried at all. A configured proxy is
// the user routing relay traffic somewhere on purpose; QUIC would go around it.
func (t *Transport) eligible(req *http.Request) bool {
	if t.disabled || req.URL.Scheme != "https" {
		return false
	}
	if proxy, err := t.proxy(req); err != nil || proxy != nil {
		return false
	}
	return true
}

// useH3 reports whether host is currently served over HTTP/3, starting a probe
// when the answer is unknown or due to be re-checked.
func (t *Transport) useH3(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[host]
	if st == nil {
		st = &hostState{}
		t.hosts[host] = st
	}
	if !st.h3 && !st.probing && !t.now().Before(st.retryAt) {
		st.probing = true
		go t.probe(host)
	}
	return st.h3
}

func (t *Transport) probe(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	ok := false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+probePath, nil)
	if err == nil {
		if resp, err := t.h3.RoundTrip(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			ok = true
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[host]
	st.probing = false
	st.h3 = ok
	if !ok {
		st.retryAt = t.now().Add(retryAfter)
	}
}

func (t *Transport) demote(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.hosts[host]; st != nil {
		st.h3 = false
		st.retryAt = t.now().Add(retryAfter)
	}
}

// CloseIdleConnections releases pooled connections on both protocols.
func (t *Transport) CloseIdleConnections() {
	t.h2.CloseIdleConnections()
	t.h3.CloseIdleConnections()
}
