package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/httpconn"
	"wanctl/internal/sessionauth"
)

// The HTTP transport carries an end-to-end TLS stream over finite request /
// response pairs. A down-poll response that the controller or agent only
// receives part of therefore punches a hole in the middle of that stream, and
// TLS reports the hole as "bad record MAC" (issue #57). On a slow link the
// truncation comes from the client-side timeout firing while the body is still
// downloading; here it is injected deterministically.

// truncatedBody hands back the first `remaining` bytes of a response and then
// fails, exactly the way a body read aborted by http.Client.Timeout does.
type truncatedBody struct {
	inner     io.ReadCloser
	remaining int
	spent     bool
}

var errTruncated = errors.New("carrier: response body truncated mid-download")

func (b *truncatedBody) Read(p []byte) (int, error) {
	if b.spent {
		return 0, errTruncated
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	if len(p) == 0 {
		b.spent = true
		return 0, errTruncated
	}
	n, err := b.inner.Read(p)
	b.remaining -= n
	if b.remaining == 0 {
		b.spent = true
	}
	return n, err
}

func (b *truncatedBody) Close() error { return b.inner.Close() }

// faultyCarrier injects faults into the Nth /h/down response that actually
// carries bytes (status 200): a body cut short, or a connection that dies
// before the response is delivered at all.
type faultyCarrier struct {
	base       http.RoundTripper
	truncateOn map[int]int  // nth data-bearing down poll -> bytes to deliver first
	dropOn     map[int]bool // nth data-bearing down poll -> fail the request

	mu        sync.Mutex
	downs     int
	truncated int
	dropped   int
}

func (f *faultyCarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	down := strings.HasPrefix(req.URL.Path, "/h/down")
	resp, err := f.base.RoundTrip(req)
	if err != nil || !down || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	f.mu.Lock()
	f.downs++
	n := f.downs
	cut, truncate := f.truncateOn[n]
	drop := f.dropOn[n]
	if truncate {
		f.truncated++
	}
	if drop {
		f.dropped++
	}
	f.mu.Unlock()
	if drop {
		resp.Body.Close()
		return nil, fmt.Errorf("carrier: connection reset before response was read")
	}
	if truncate {
		resp.Body = &truncatedBody{inner: resp.Body, remaining: cut}
	}
	return resp, nil
}

func (f *faultyCarrier) counts() (downs, truncated, dropped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downs, f.truncated, f.dropped
}

func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wanctl-slowlink-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"wanctl-slowlink-test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// newHTTPTunnelSession registers a session on the relay directly, which is what
// /h/dial does once the target agent has been found.
func newHTTPTunnelSession(t *testing.T, r *Relay, sid, ns, device string) {
	t.Helper()
	auth := sessionauth.Open{
		Session:         sid,
		Device:          device,
		CallerNamespace: ns,
		OwnerNamespace:  ns,
	}
	r.newHTTPSession(sid, auth, delegation.Access{Namespace: ns}, "tok-alice")
}

// pushOverFaultyCarrier runs a payload-sized push from a controller to an agent
// over the relay's HTTP transport, with faults injected into the agent's down
// polls. It returns the SHA-256 the agent computed over what it received.
func pushOverFaultyCarrier(t *testing.T, payload []byte, carrier *faultyCarrier) (sum string, err error) {
	t.Helper()
	r := New(EnvTokenStore("tok-alice:alice"))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	const sid = "sess-slowlink"
	newHTTPTunnelSession(t, r, sid, "alice", "home-pc")

	cert := testTLSCert(t)
	serverCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	clientCfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}

	agentRaw, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice", &http.Client{Transport: carrier})
	if err != nil {
		t.Fatal(err)
	}
	clientRaw, err := httpconn.Dial(t.Context(), srv.URL, sid, "client", "tok-alice")
	if err != nil {
		t.Fatal(err)
	}

	type agentResult struct {
		sum string
		err error
	}
	done := make(chan agentResult, 1)
	go func() {
		defer agentRaw.Close()
		tlsAgent := tls.Server(agentRaw, serverCfg)
		h := sha256.New()
		if _, copyErr := io.Copy(h, tlsAgent); copyErr != nil {
			done <- agentResult{err: fmt.Errorf("agent read: %w", copyErr)}
			// Still answer so the controller is not left blocked.
			fmt.Fprintf(tlsAgent, "%x\n", h.Sum(nil))
			return
		}
		digest := fmt.Sprintf("%x", h.Sum(nil))
		fmt.Fprintf(tlsAgent, "%s\n", digest)
		tlsAgent.CloseWrite()
		done <- agentResult{sum: digest}
	}()

	tlsClient := tls.Client(clientRaw, clientCfg)
	defer clientRaw.Close()
	var ctrlErr error
	if _, writeErr := tlsClient.Write(payload); writeErr != nil {
		ctrlErr = fmt.Errorf("controller write: %w", writeErr)
	}
	if ctrlErr == nil {
		if closeErr := tlsClient.CloseWrite(); closeErr != nil {
			ctrlErr = fmt.Errorf("controller close-write: %w", closeErr)
		}
	}
	reply := make([]byte, 96)
	var n int
	if ctrlErr == nil {
		var readErr error
		n, readErr = io.ReadFull(tlsClient, reply[:65])
		if readErr != nil {
			ctrlErr = fmt.Errorf("controller read back: %w", readErr)
		}
	}
	select {
	case res := <-done:
		// The agent's TLS error is the interesting one: it is what the user
		// sees as "tls: bad record MAC". The controller usually only learns
		// that the session went away.
		if res.err != nil {
			if ctrlErr != nil {
				return "", fmt.Errorf("%w (controller saw: %v)", res.err, ctrlErr)
			}
			return "", res.err
		}
		if ctrlErr != nil {
			return "", ctrlErr
		}
		return strings.TrimSpace(string(reply[:n])), nil
	case <-time.After(60 * time.Second):
		if ctrlErr != nil {
			return "", fmt.Errorf("timed out waiting for the agent (controller saw: %v)", ctrlErr)
		}
		return "", errors.New("timed out waiting for the agent")
	}
}

// TestPushSurvivesTruncatedDownPoll is the reproduction for issue #57: on a slow
// link a down-poll response is cut short, and before the fix the receiving side
// swallowed the read error and fed the truncated bytes straight into TLS.
func TestPushSurvivesTruncatedDownPoll(t *testing.T) {
	payload := make([]byte, 20<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)

	carrier := &faultyCarrier{
		base:       http.DefaultTransport,
		truncateOn: map[int]int{3: 1024, 11: 64 << 10},
	}
	got, err := pushOverFaultyCarrier(t, payload, carrier)
	downs, truncated, dropped := carrier.counts()
	if err != nil {
		t.Fatalf("push failed after %d data-bearing down polls (%d truncated, %d dropped): %v",
			downs, truncated, dropped, err)
	}
	if truncated != len(carrier.truncateOn) {
		t.Fatalf("carrier truncated %d responses, want %d (the fault never fired)", truncated, len(carrier.truncateOn))
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("agent received a different %d-byte stream: sha256 %s, want %s", len(payload), got, hex.EncodeToString(want[:]))
	}
}

// TestPushSurvivesDroppedDownPoll covers the other half of a flaky carrier: the
// down poll itself fails after the relay has already dequeued the bytes.
func TestPushSurvivesDroppedDownPoll(t *testing.T) {
	payload := make([]byte, 8<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)

	carrier := &faultyCarrier{
		base:   http.DefaultTransport,
		dropOn: map[int]bool{2: true, 7: true},
	}
	got, err := pushOverFaultyCarrier(t, payload, carrier)
	downs, truncated, dropped := carrier.counts()
	if err != nil {
		t.Fatalf("push failed after %d data-bearing down polls (%d truncated, %d dropped): %v",
			downs, truncated, dropped, err)
	}
	if dropped != len(carrier.dropOn) {
		t.Fatalf("carrier dropped %d responses, want %d (the fault never fired)", dropped, len(carrier.dropOn))
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("agent received a different %d-byte stream: sha256 %s, want %s", len(payload), got, hex.EncodeToString(want[:]))
	}
}

func TestSideQueueResendsUntilAcked(t *testing.T) {
	q := newSideQueue()
	q.push([]byte("first"))
	q.push([]byte("second"))

	data, seq, closed := q.take(0, time.Second)
	if string(data) != "firstsecond" || seq != 1 || closed {
		t.Fatalf("first take = %q seq %d closed %v, want %q seq 1", data, seq, closed, "firstsecond")
	}
	// An unacked reader is served the same bytes again rather than the queue
	// moving on without it.
	q.push([]byte("third"))
	again, againSeq, _ := q.take(0, time.Second)
	if string(again) != "firstsecond" || againSeq != 1 {
		t.Fatalf("re-send = %q seq %d, want %q seq 1", again, againSeq, "firstsecond")
	}
	next, nextSeq, _ := q.take(1, time.Second)
	if string(next) != "third" || nextSeq != 2 {
		t.Fatalf("after ack = %q seq %d, want %q seq 2", next, nextSeq, "third")
	}
	q.close()
	tail, tailSeq, tailClosed := q.take(2, 50*time.Millisecond)
	if len(tail) != 0 || tailSeq != 0 || !tailClosed {
		t.Fatalf("drained closed queue = %q seq %d closed %v, want empty and closed", tail, tailSeq, tailClosed)
	}
}

func TestDrainCoalescingIsBounded(t *testing.T) {
	q := newSideQueue()
	chunk := make([]byte, 64<<10)
	for range 16 { // 1 MiB queued, four times the cap
		q.push(chunk)
	}
	data, _ := q.drain(time.Second)
	if len(data) > maxDrainBytes {
		t.Fatalf("one drain returned %d bytes, want at most %d", len(data), maxDrainBytes)
	}
	if len(data) != maxDrainBytes {
		t.Fatalf("one drain returned %d bytes, want it to fill the %d-byte cap", len(data), maxDrainBytes)
	}
}

// A controller or agent built before the acknowledged down protocol sends no
// ack parameter. It must keep working against an updated relay.
func TestDownPollWithoutAckIsFireAndForget(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	const sid = "sess-legacy"
	newHTTPTunnelSession(t, r, sid, "alice", "home-pc")
	r.session(sid).toClient.push([]byte("hello"))

	get := func(query string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/h/down?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok-alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get("session=" + sid + "&role=client")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("legacy down poll = %d %q, want 200 %q", resp.StatusCode, body, "hello")
	}
	if got := resp.Header.Get(httpconn.DownSeqHeader); got != "" {
		t.Fatalf("legacy down poll carried a sequence header %q, want none", got)
	}
	// Without an ack the relay must not hold the chunk, or the legacy reader
	// would be served the same bytes forever.
	q := r.session(sid).toClient
	q.ackMu.Lock()
	held := len(q.unacked)
	q.ackMu.Unlock()
	if held != 0 {
		t.Fatalf("relay held %d bytes for a client that cannot ack them", held)
	}
}
