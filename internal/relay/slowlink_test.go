package relay

import (
	"bytes"
	"context"
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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// These tests carry real HTTP — the same net/http client and server the product
// uses, with only the TCP listen and dial replaced by net.Pipe — so they need no
// listening socket and run in a sandbox that forbids one.
type memoryListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memoryListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *memoryListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80} }

type memoryServer struct {
	URL       string
	Transport *http.Transport
	srv       *http.Server
	listener  *memoryListener
}

func newMemoryServer(t *testing.T, h http.Handler) *memoryServer {
	t.Helper()
	l := &memoryListener{conns: make(chan net.Conn), done: make(chan struct{})}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		a, b := net.Pipe()
		select {
		case l.conns <- b:
			return a, nil
		case <-ctx.Done():
			a.Close()
			b.Close()
			return nil, ctx.Err()
		case <-l.done:
			a.Close()
			b.Close()
			return nil, net.ErrClosed
		}
	}}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	s := &memoryServer{URL: "http://memory.test", Transport: tr, srv: srv, listener: l}
	t.Cleanup(func() { s.Transport.CloseIdleConnections(); s.srv.Close() })
	return s
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// legacyRelay is a relay from before the acknowledged down protocol: it
// dequeues a chunk before it answers and advertises nothing. Dropping ack=
// takes the current handler down exactly that path, and stripping the two
// response headers hides the capability the way an older build would.
func legacyRelay(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		q.Del(httpconn.DownAckParam)
		req.URL.RawQuery = q.Encode()
		h.ServeHTTP(&legacyWriter{ResponseWriter: w}, req)
	})
}

type legacyWriter struct{ http.ResponseWriter }

func (w *legacyWriter) WriteHeader(code int) {
	w.Header().Del(httpconn.DownAckCapabilityHeader)
	w.Header().Del(httpconn.DownSeqHeader)
	w.ResponseWriter.WriteHeader(code)
}

// tunnelSession registers a session on a fresh relay and returns both.
func tunnelSession(t *testing.T) (*Relay, *httpSession, string) {
	t.Helper()
	const sid = "sess-slowlink"
	r := New(EnvTokenStore("tok-alice:alice"))
	newHTTPTunnelSession(t, r, sid, "alice", "home-pc")
	s := r.session(sid)
	t.Cleanup(func() { r.closeHTTPSession(sid, s) })
	return r, s, sid
}

// faultyCarrier injects faults into the Nth /h/down response that actually
// carries bytes (status 200): a body cut short, or a connection that dies
// before the response is delivered at all.
type faultyCarrier struct {
	base       http.RoundTripper
	truncateOn map[int]int  // nth data-bearing down poll -> bytes to deliver first
	dropOn     map[int]bool // nth data-bearing down poll -> fail the request

	mu         sync.Mutex
	downs      int
	truncated  int
	dropped    int
	lastHeader http.Header
}

func (f *faultyCarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	down := strings.HasPrefix(req.URL.Path, "/h/down")
	resp, err := f.base.RoundTrip(req)
	if err != nil || !down || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	f.mu.Lock()
	f.lastHeader = resp.Header.Clone()
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

func (f *faultyCarrier) advertised() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastHeader
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
	srv := newMemoryServer(t, r.Handler())

	const sid = "sess-slowlink"
	newHTTPTunnelSession(t, r, sid, "alice", "home-pc")
	carrier.base = srv.Transport

	cert := testTLSCert(t)
	serverCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	clientCfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}

	agentRaw, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice", &http.Client{Transport: carrier})
	if err != nil {
		t.Fatal(err)
	}
	clientRaw, err := httpconn.DialWith(t.Context(), srv.URL, sid, "client", "tok-alice", &http.Client{Transport: srv.Transport})
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

	carrier := &faultyCarrier{truncateOn: map[int]int{2: 1024, 5: 64 << 10}}
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

	carrier := &faultyCarrier{dropOn: map[int]bool{2: true, 5: true}}
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

	data, seq, closed, ok := q.take(context.Background(), 0, time.Second)
	if string(data) != "firstsecond" || seq != 1 || closed || !ok {
		t.Fatalf("first take = %q seq %d closed %v ok %v, want %q seq 1", data, seq, closed, ok, "firstsecond")
	}
	// An unacked reader is served the same bytes again rather than the queue
	// moving on without it.
	q.push([]byte("third"))
	again, againSeq, _, _ := q.take(context.Background(), 0, time.Second)
	if string(again) != "firstsecond" || againSeq != 1 {
		t.Fatalf("re-send = %q seq %d, want %q seq 1", again, againSeq, "firstsecond")
	}
	next, nextSeq, _, _ := q.take(context.Background(), 1, time.Second)
	if string(next) != "third" || nextSeq != 2 {
		t.Fatalf("after ack = %q seq %d, want %q seq 2", next, nextSeq, "third")
	}
	q.close()
	tail, tailSeq, tailClosed, _ := q.take(context.Background(), 2, 50*time.Millisecond)
	if len(tail) != 0 || tailSeq != 0 || !tailClosed {
		t.Fatalf("drained closed queue = %q seq %d closed %v, want empty and closed", tail, tailSeq, tailClosed)
	}
}

// Overlapping polls on one direction — a reader whose request was abandoned
// while parked on an empty queue, plus the retry it sent afterwards — used to
// each take a chunk, and the second overwrote the first's unacked chunk. Every
// poll here reports the same ack, so every poll that gets data must get the
// same data: anything else means the queue advanced past bytes nobody has
// confirmed receiving.
func TestOverlappingPollsCannotTakeDifferentChunks(t *testing.T) {
	q := newSideQueue()
	const pollers = 8
	got := make(chan []byte, pollers)
	var wg sync.WaitGroup
	for range pollers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, _, _, _ := q.take(context.Background(), 0, 2*time.Second)
			got <- data
		}()
	}
	// Let every poller reach the queue, then feed distinct chunks slowly
	// enough that they do not coalesce into one drain.
	time.Sleep(50 * time.Millisecond)
	for i := range pollers {
		q.push(bytes.Repeat([]byte{byte('a' + i)}, 64))
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()
	close(got)

	var first []byte
	for data := range got {
		if len(data) == 0 {
			continue
		}
		if first == nil {
			first = data
			continue
		}
		if !bytes.Equal(data, first) {
			t.Fatalf("two polls that had acknowledged nothing were served different chunks, %q and %q: the queue advanced past an unacknowledged chunk",
				first[:8], data[:8])
		}
	}
	if first == nil {
		t.Fatal("no poll was served any data")
	}
	q.ackMu.Lock()
	seq := q.seq
	q.ackMu.Unlock()
	if seq != 1 {
		t.Fatalf("queue reached sequence %d while nothing had been acknowledged, want 1", seq)
	}
}

// pendingDrains counts how many goroutines are parked inside sideQueue.drain.
// A poll the carrier abandoned keeps running on the relay, and that is the
// state this has to observe from the outside.
func pendingDrains() int {
	buf := make([]byte, 1<<20)
	return bytes.Count(buf[:runtime.Stack(buf, true)], []byte("(*sideQueue).drain("))
}

func awaitPendingDrains(n int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pendingDrains() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestOverlappingDownPollsOnTheWireDoNotLoseAChunk is the overlap from the far
// side of the wire, which the per-poll fault carrier cannot reach: it fails a
// poll at the client while deliberately leaving that same request running on
// the relay, so the retry arrives with the abandoned poll still parked on the
// queue. Two chunks are then fed one at a time. Before the fix the abandoned
// poll took the first and the retry took the second, overwriting the first's
// unacknowledged chunk, and the reader silently resumed at the second.
func TestOverlappingDownPollsOnTheWireDoNotLoseAChunk(t *testing.T) {
	r, s, sid := tunnelSession(t)
	var entered atomic.Int32
	counted := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/h/down" {
			entered.Add(1)
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := newMemoryServer(t, counted)

	var abandonOnce sync.Once
	carrier := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/h/down" || req.URL.Query().Get(httpconn.DownAckParam) == "0" {
			return srv.Transport.RoundTrip(req) // the warm-up poll goes through
		}
		abandoned := false
		abandonOnce.Do(func() {
			abandoned = true
			// Detached from the client's context on purpose: the relay never
			// learns this reader went away and keeps the poll parked.
			detached := req.Clone(context.Background())
			go func() {
				if resp, err := srv.Transport.RoundTrip(detached); err == nil {
					resp.Body.Close()
				}
			}()
			awaitPendingDrains(1) // it is on the queue before the retry is sent
		})
		if abandoned {
			return nil, errors.New("carrier: connection reset before the response was read")
		}
		return srv.Transport.RoundTrip(req)
	})

	c, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice", &http.Client{Transport: carrier})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// One clean exchange, so the reader knows the relay holds unacked chunks.
	s.toAgent.push([]byte("WARMUP"))
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "WARMUP" {
		t.Fatalf("warm-up read = %q %v, want %q", buf[:n], err, "WARMUP")
	}

	first := bytes.Repeat([]byte("A"), 4096)
	second := bytes.Repeat([]byte("B"), 4096)
	// The reader hands back each chunk as it arrives, so a stream that resumed
	// at the wrong one is reported as that rather than as a stall.
	arrived := make(chan []byte, 2)
	read := make(chan error, 1)
	go func() {
		for range 2 {
			chunk := make([]byte, 4096)
			if _, err := io.ReadFull(c, chunk); err != nil {
				read <- err
				return
			}
			arrived <- chunk
		}
		read <- nil
	}()
	nextChunk := func(want []byte, which string) {
		t.Helper()
		select {
		case got := <-arrived:
			if !bytes.Equal(got, want) {
				t.Fatalf("the %s chunk read back as %q, want %q: a chunk the reader never acknowledged was dropped",
					which, got[:8], want[:8])
			}
		case err := <-read:
			t.Fatalf("reading the %s chunk after an abandoned poll overlapped its retry: %v", which, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out on the %s chunk: the stream stalled after an abandoned poll overlapped its retry", which)
		}
	}

	if !awaitPendingDrains(1) {
		t.Fatal("the abandoned poll never reached the queue")
	}
	// Wait for the retry to reach the relay as well: parked in drain before the
	// fix, waiting its turn after it.
	deadline := time.Now().Add(3 * time.Second)
	for entered.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if entered.Load() < 3 {
		t.Fatalf("only %d down polls reached the relay, want the retry as well", entered.Load())
	}
	time.Sleep(20 * time.Millisecond) // let the retry settle wherever it waits

	// Feed the chunks one at a time, so they cannot coalesce into one drain.
	seqAfter := func(n uint64) bool {
		for time.Now().Before(deadline) {
			s.toAgent.ackMu.Lock()
			seq := s.toAgent.seq
			s.toAgent.ackMu.Unlock()
			if seq >= n {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}
	s.toAgent.push(first)
	if !seqAfter(2) {
		t.Fatal("no poll took the first chunk")
	}
	s.toAgent.push(second)

	nextChunk(first, "first")
	nextChunk(second, "second")
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("read after an abandoned poll overlapped its retry: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out reading after an abandoned poll overlapped its retry")
	}
}

// A poll the reader abandoned after the relay had already drained it must leave
// the bytes as the unacked chunk, not drop them: the next poll re-serves them
// under the same sequence.
func TestAbandonedPollKeepsItsChunkForTheNextPoll(t *testing.T) {
	r, s, sid := tunnelSession(t)
	s.toAgent.push([]byte("FINAL"))

	poll := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/h/down?session="+sid+"&role=agent&ack=0", nil)
		req.Header.Set("Authorization", "Bearer tok-alice")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel() // the reader is already gone by the time the relay answers
	first := poll(gone)
	if first.Code != http.StatusOK || first.Body.String() != "FINAL" {
		t.Fatalf("abandoned poll = %d %q, want 200 %q", first.Code, first.Body.String(), "FINAL")
	}
	s.toAgent.ackMu.Lock()
	held := string(s.toAgent.unacked)
	s.toAgent.ackMu.Unlock()
	if held != "FINAL" {
		t.Fatalf("relay held %q after the poll was abandoned, want %q", held, "FINAL")
	}
	second := poll(context.Background())
	if second.Code != http.StatusOK || second.Body.String() != "FINAL" {
		t.Fatalf("next poll = %d %q, want the same 200 %q", second.Code, second.Body.String(), "FINAL")
	}
	if first.Header().Get(httpconn.DownSeqHeader) != second.Header().Get(httpconn.DownSeqHeader) {
		t.Fatalf("re-send was renumbered: %q then %q",
			first.Header().Get(httpconn.DownSeqHeader), second.Header().Get(httpconn.DownSeqHeader))
	}
}

// A poll may only be retried against a relay that holds the chunk until it is
// acknowledged. An older relay dequeues before it answers, so retrying there
// resumes at the chunk after the bytes that were lost — a silent hole in the
// stream. The read has to fail instead.
func TestDownPollRetriesOnlyAgainstAnAcknowledgingRelay(t *testing.T) {
	// The fault lands on the second data-bearing poll, after one has already
	// been answered: mid-stream, which is where issue #57 bites.
	faults := map[string]*faultyCarrier{
		"response dropped": {dropOn: map[int]bool{2: true}},
		"body cut short":   {truncateOn: map[int]int{2: 2}},
	}
	for name, fault := range faults {
		for _, legacy := range []bool{true, false} {
			relayKind := "current relay"
			if legacy {
				relayKind = "pre-acknowledgement relay"
			}
			t.Run(name+"/"+relayKind, func(t *testing.T) {
				r, s, sid := tunnelSession(t)
				handler := r.Handler()
				if legacy {
					handler = legacyRelay(handler)
				}
				srv := newMemoryServer(t, handler)
				carrier := &faultyCarrier{base: srv.Transport, dropOn: fault.dropOn, truncateOn: fault.truncateOn}
				c, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice",
					&http.Client{Transport: carrier, Timeout: 5 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()

				// One clean exchange first. Against the current relay this is
				// where the reader learns the relay holds unacknowledged
				// chunks; against the older one there is nothing to learn.
				s.toAgent.push([]byte("WARMUP"))
				buf := make([]byte, 32)
				n, err := c.Read(buf)
				if err != nil || string(buf[:n]) != "WARMUP" {
					t.Fatalf("warm-up read = %q %v, want %q", buf[:n], err, "WARMUP")
				}
				// Check the relay under test really is the one this case
				// claims, rather than trusting the wrapper to have hidden the
				// protocol.
				advertised := carrier.advertised()
				gotCap := advertised.Get(httpconn.DownAckCapabilityHeader)
				gotSeq := advertised.Get(httpconn.DownSeqHeader)
				if legacy && (gotCap != "" || gotSeq != "") {
					t.Fatalf("the pre-acknowledgement relay advertised %s=%q %s=%q, want neither",
						httpconn.DownAckCapabilityHeader, gotCap, httpconn.DownSeqHeader, gotSeq)
				}
				if !legacy && (gotCap != "1" || gotSeq == "") {
					t.Fatalf("the current relay advertised %s=%q %s=%q, want both",
						httpconn.DownAckCapabilityHeader, gotCap, httpconn.DownSeqHeader, gotSeq)
				}

				s.toAgent.push([]byte("FIRST"))
				go func() {
					time.Sleep(100 * time.Millisecond)
					s.toAgent.push([]byte("SECOND"))
				}()

				n, readErr := c.Read(buf)
				if legacy {
					if readErr == nil {
						t.Fatalf("an unrecoverable failure against a relay that cannot re-send was retried and the read continued with %q", buf[:n])
					}
					return
				}
				if readErr != nil {
					t.Fatalf("read against an acknowledging relay = %v, want the chunk re-sent", readErr)
				}
				if string(buf[:n]) != "FIRST" {
					t.Fatalf("read = %q, want the re-sent %q", buf[:n], "FIRST")
				}
			})
		}
	}
}

// A peer closing gracefully means "no more bytes", not "discard what is
// queued". With a drain cap a backlog needs several polls to come out, so the
// session has to stay reachable until the reader has taken all of it.
func TestGracefulCloseStaysDrainable(t *testing.T) {
	r, s, sid := tunnelSession(t)
	srv := newMemoryServer(t, r.Handler())

	// The peer closes while the first response is in flight, with most of the
	// backlog still queued behind the drain cap.
	var once sync.Once
	carrier := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := srv.Transport.RoundTrip(req)
		if err == nil && req.URL.Path == "/h/down" && resp.StatusCode == http.StatusOK {
			once.Do(func() {
				closeReq := httptest.NewRequest("POST", "/h/close?session="+sid, nil)
				closeReq.Header.Set("Authorization", "Bearer tok-alice")
				rec := httptest.NewRecorder()
				r.Handler().ServeHTTP(rec, closeReq)
				if rec.Code != http.StatusOK {
					t.Errorf("close = %d, want 200", rec.Code)
				}
			})
		}
		return resp, err
	})
	c, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice", &http.Client{Transport: carrier})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const chunks = 4
	want := make([]byte, 0, chunks*maxDrainBytes)
	for i := range chunks {
		chunk := bytes.Repeat([]byte{byte('A' + i)}, maxDrainBytes)
		s.toAgent.push(chunk)
		want = append(want, chunk...)
	}

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read after a graceful close: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes after a graceful close, want all %d that were queued", len(got), len(want))
	}
	if r.session(sid) != nil {
		t.Fatal("a fully drained closed session was left in the registry")
	}
}

// downPoll drives one /h/down straight through the handler, with no server or
// carrier in the way, so a test can interleave the two directions by hand.
func downPoll(t *testing.T, r *Relay, sid, role string, ack uint64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", fmt.Sprintf("/h/down?session=%s&role=%s&ack=%d", sid, role, ack), nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func upPost(t *testing.T, r *Relay, sid, role string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/h/up?session="+sid+"&role="+role, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

// Reaching the end of one direction says nothing about the other. The reader
// that finished still leaves its peer holding an unacknowledged chunk and the
// tail of a split one, and retiring the session on that first EOF takes both
// away: the peer's next poll gets a 404 for bytes the relay had promised to
// keep. Covered for both ways a session ends gracefully, and for a poll that
// was already parked on the empty direction when the close landed.
func TestGracefulCloseWaitsForBothDirections(t *testing.T) {
	closers := map[string]func(t *testing.T, r *Relay, s *httpSession, sid string){
		"peer posts /h/close": func(t *testing.T, r *Relay, s *httpSession, sid string) {
			req := httptest.NewRequest("POST", "/h/close?session="+sid, nil)
			req.Header.Set("Authorization", "Bearer tok-alice")
			rec := httptest.NewRecorder()
			r.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("close = %d, want 200", rec.Code)
			}
		},
		"websocket leg ends": func(t *testing.T, r *Relay, s *httpSession, sid string) {
			if err := r.httpSessionConn(sid, s, "client").(io.Closer).Close(); err != nil {
				t.Fatalf("bridge close: %v", err)
			}
		},
	}
	for name, closeSession := range closers {
		for _, parked := range []bool{false, true} {
			label := name
			if parked {
				label += "/with a poll already parked"
			}
			t.Run(label, func(t *testing.T) {
				r, s, sid := tunnelSession(t)

				// The agent is mid-stream: one chunk delivered but not yet
				// acknowledged, and a tail left over from splitting at the cap.
				const tail = 5
				payload := bytes.Repeat([]byte("A"), maxDrainBytes+tail)
				s.toAgent.push(payload)
				first := downPoll(t, r, sid, "agent", 0)
				if first.Code != http.StatusOK || first.Body.Len() != maxDrainBytes {
					t.Fatalf("first agent poll = %d with %d bytes, want 200 with %d", first.Code, first.Body.Len(), maxDrainBytes)
				}

				// The controller has read everything it was sent and parks on
				// an empty queue, either before or after the close.
				empty := make(chan *httptest.ResponseRecorder, 1)
				if parked {
					go func() { empty <- downPoll(t, r, sid, "client", 0) }()
					if !awaitPendingDrains(1) {
						t.Fatal("the controller poll never reached the queue")
					}
				}
				closeSession(t, r, s, sid)
				if !parked {
					empty <- downPoll(t, r, sid, "client", 0)
				}
				select {
				case rec := <-empty:
					if rec.Code != http.StatusGone {
						t.Fatalf("poll on the drained direction = %d, want 410", rec.Code)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("the parked controller poll never woke")
				}

				// None of that is the agent's business: its chunk and the tail
				// behind it must still be there.
				resend := downPoll(t, r, sid, "agent", 0)
				if resend.Code != http.StatusOK {
					t.Fatalf("agent re-poll = %d, want the unacknowledged chunk re-sent; the other direction draining retired the session",
						resend.Code)
				}
				if !bytes.Equal(resend.Body.Bytes(), payload[:maxDrainBytes]) {
					t.Fatalf("agent re-poll returned %d bytes, want the same %d", resend.Body.Len(), maxDrainBytes)
				}
				seq, err := strconv.ParseUint(resend.Header().Get(httpconn.DownSeqHeader), 10, 64)
				if err != nil {
					t.Fatalf("re-send carried no sequence: %v", err)
				}
				rest := downPoll(t, r, sid, "agent", seq)
				if rest.Code != http.StatusOK || !bytes.Equal(rest.Body.Bytes(), payload[maxDrainBytes:]) {
					t.Fatalf("split tail = %d %q, want 200 with the last %d bytes", rest.Code, rest.Body.Bytes(), tail)
				}
				tailSeq, err := strconv.ParseUint(rest.Header().Get(httpconn.DownSeqHeader), 10, 64)
				if err != nil {
					t.Fatalf("tail carried no sequence: %v", err)
				}
				// Now both directions really are empty, so the session goes.
				if done := downPoll(t, r, sid, "agent", tailSeq); done.Code != http.StatusGone {
					t.Fatalf("poll after the last byte = %d, want 410", done.Code)
				}
				if r.session(sid) != nil {
					t.Fatal("a session both sides had drained was left in the registry")
				}
			})
		}
	}
}

// awaitInflight waits until a reader is inside the queue, holding it.
func awaitInflight(t *testing.T, q *sideQueue) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		q.ackMu.Lock()
		held := q.inflight
		q.ackMu.Unlock()
		if held {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("no reader ever took hold of the queue")
}

// A reader that was already parked when the close arrived is still holding its
// direction while the close walks the two queues, so whoever looks first looks
// too early: a poll that wakes on the first queue finds the second still open,
// and a poll that wakes on the second finds the first still held. Retirement
// has to be attempted by every site that could have completed the last
// condition, because a check that came too early is only harmless if someone
// checks again. Otherwise nothing retires the session and it lingers until the
// idle sweeper, a minute later. The in-process bridge reader is one of those
// sites and used to attempt nothing at all.
func TestGracefulCloseRetiresASessionAReaderWasParkedOn(t *testing.T) {
	// One processor is what CI had when this surfaced: a parked reader does not
	// get to run again before the close and the polls after it do.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	cases := map[string]func(t *testing.T, r *Relay, s *httpSession, sid string){
		"peer posts /h/close with both directions parked": func(t *testing.T, r *Relay, s *httpSession, sid string) {
			go downPoll(t, r, sid, "client", 0)
			go downPoll(t, r, sid, "agent", 0)
			awaitInflight(t, s.toClient)
			awaitInflight(t, s.toAgent)
			req := httptest.NewRequest("POST", "/h/close?session="+sid, nil)
			req.Header.Set("Authorization", "Bearer tok-alice")
			r.Handler().ServeHTTP(httptest.NewRecorder(), req)
		},
		"websocket leg ends with the bridge read parked": func(t *testing.T, r *Relay, s *httpSession, sid string) {
			go r.httpSessionConn(sid, s, "client").Read(make([]byte, 64))
			awaitInflight(t, s.toClient)
			if err := r.httpSessionConn(sid, s, "client").(io.Closer).Close(); err != nil {
				t.Fatalf("bridge close: %v", err)
			}
			if code := downPoll(t, r, sid, "agent", 0).Code; code != http.StatusGone {
				t.Fatalf("poll after the close = %d, want 410", code)
			}
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			for attempt := 1; attempt <= 30; attempt++ {
				r, s, sid := tunnelSession(t)
				run(t, r, s, sid)
				gone := false
				for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
					if r.session(sid) == nil {
						gone = true
						break
					}
					runtime.Gosched()
				}
				if !gone {
					t.Fatalf("attempt %d: the session was still registered two seconds after both sides were done with it; nothing retried the check that ran while a parked reader still held a direction",
						attempt)
				}
			}
		})
	}
}

// The queue looks empty for as long as a poll has a chunk in its hands but has
// not yet recorded it as unacknowledged: it has left the channel and reached no
// field. A poll on the other direction that checks both sides during that
// window used to find the session finished and retire it, and the chunk the
// first poll then recorded could never be re-sent — its retry got a 404.
//
// This is a logical race, not a data race, so it is provoked rather than
// injected: the empty direction is polled in a tight loop while the other one
// works through two megabytes. The reviewer's probe hit it on the first
// attempt for both ways a session ends.
func TestRetirementWaitsForAPollHoldingAChunk(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	for _, viaBridge := range []bool{false, true} {
		name := "peer posts /h/close"
		if viaBridge {
			name = "websocket leg ends"
		}
		t.Run(name, func(t *testing.T) {
			for attempt := 1; attempt <= 100; attempt++ {
				r, s, sid := tunnelSession(t)
				s.toAgent.push(bytes.Repeat([]byte("A"), 1<<20))
				s.toAgent.push(bytes.Repeat([]byte("B"), 1<<20))
				if viaBridge {
					r.httpSessionConn(sid, s, "client").(io.Closer).Close()
				} else {
					closeReq := httptest.NewRequest("POST", "/h/close?session="+sid, nil)
					closeReq.Header.Set("Authorization", "Bearer tok-alice")
					r.Handler().ServeHTTP(httptest.NewRecorder(), closeReq)
				}

				// The agent takes the backlog while the controller, which has
				// nothing left to read, keeps asking.
				agentPoll := make(chan *httptest.ResponseRecorder, 1)
				go func() { agentPoll <- downPoll(t, r, sid, "agent", 0) }()
				var first *httptest.ResponseRecorder
				for first == nil {
					select {
					case first = <-agentPoll:
					default:
						downPoll(t, r, sid, "client", 0)
					}
				}

				// Treat the agent's response as lost: it was never acknowledged,
				// so the relay owes it again.
				if r.session(sid) == nil {
					s.toAgent.ackMu.Lock()
					held, seq := len(s.toAgent.unacked), s.toAgent.seq
					s.toAgent.ackMu.Unlock()
					t.Fatalf("attempt %d: the session was retired while a poll held a chunk (its answer was %d with %d bytes, seq %d, now recorded as %d unacknowledged bytes that can never be re-sent)",
						attempt, first.Code, first.Body.Len(), seq, held)
				}
				retry := downPoll(t, r, sid, "agent", 0)
				if retry.Code != http.StatusOK || retry.Body.Len() != first.Body.Len() {
					t.Fatalf("attempt %d: re-poll for the unacknowledged chunk = %d with %d bytes, want 200 with %d",
						attempt, retry.Code, retry.Body.Len(), first.Body.Len())
				}
				r.closeHTTPSession(sid, s)
			}
		})
	}
}

// A gracefully closed session stays reachable so the far side can finish
// reading it. That is not permission to write to it again: keeping the session
// registered used to leave /h/up wide open, and push picking between a writable
// queue and a closed one let some of those uploads land.
func TestUploadsAfterGracefulCloseAreRejected(t *testing.T) {
	// With a backlog still to come out, the session is deliberately kept in the
	// registry, which is exactly when /h/up can still reach it.
	r, s, sid := tunnelSession(t)
	s.toAgent.push([]byte("backlog"))
	closeReq := httptest.NewRequest("POST", "/h/close?session="+sid, nil)
	closeReq.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, closeReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("close = %d, want 200", rec.Code)
	}
	if r.session(sid) == nil {
		t.Fatal("a session with a backlog was retired at the close, so /h/up was never reachable to test")
	}
	accepted := 0
	for range 64 {
		if upPost(t, r, sid, "client", []byte("late")).Code == http.StatusOK {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d of 64 uploads were accepted after the session was closed", accepted)
	}
	// An empty body never reaches the queue, so it has to be refused by the
	// same check rather than falling through to a 200.
	if code := upPost(t, r, sid, "client", nil).Code; code != http.StatusGone {
		t.Fatalf("empty upload after close = %d, want 410", code)
	}
	if queued := len(s.toAgent.ch); queued != 1 {
		t.Fatalf("the queue holds %d chunks, want only the one chunk from before the close", queued)
	}
	// Once the backlog is out the session goes, and a late upload finds nothing.
	if code := downPoll(t, r, sid, "agent", 0).Code; code != http.StatusOK {
		t.Fatal("the backlog did not come out")
	}
	if code := downPoll(t, r, sid, "agent", 1).Code; code != http.StatusGone {
		t.Fatal("the drained direction did not report EOF")
	}
	if code := upPost(t, r, sid, "client", []byte("later still")).Code; code == http.StatusOK {
		t.Fatalf("an upload was accepted after the session was retired")
	}
}

// An upload genuinely racing the close may land or be refused, but the answer
// and the queue have to agree: a 200 means those bytes are there to be read.
func TestUploadRacingGracefulCloseIsAllOrNothing(t *testing.T) {
	for i := range 64 {
		r, s, sid := tunnelSession(t)
		chunk := []byte("racing")
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-start
			r.closeHTTPSessionDrainable(sid, s)
			close(done)
		}()
		close(start)
		code := upPost(t, r, sid, "client", chunk).Code
		<-done
		queued := len(s.toAgent.ch)
		switch {
		case code == http.StatusOK && queued != 1:
			t.Fatalf("run %d: upload answered 200 but %d chunks are queued", i, queued)
		case code != http.StatusOK && queued != 0:
			t.Fatalf("run %d: upload answered %d but %d chunks were queued anyway", i, code, queued)
		}
	}
}

// Revoking the controller's credential is not a graceful close: it tears the
// session down at once and the queued bytes go with it.
func TestRevokedCredentialTearsDownImmediately(t *testing.T) {
	a := testTransportGrant()
	store := &transportGrantStore{grants: map[string]delegation.Access{"delegate": a, "owner": {Namespace: "alice"}}}
	r := New(store)
	s := r.newHTTPSession("revoked", sessionauth.Open{
		Session: "revoked", Device: "allowed", CallerNamespace: "alice", OwnerNamespace: "alice",
	}, a, "delegate")
	defer r.closeHTTPSession("revoked", s)
	s.toAgent.push([]byte("SECRET"))
	h := r.Handler()
	if first := grantRequest(h, "GET", "/h/down?session=revoked&role=agent&ack=0", "owner"); first.Code != http.StatusOK {
		t.Fatalf("first poll = %d, want 200", first.Code)
	}
	store.revoke("delegate")
	second := grantRequest(h, "GET", "/h/down?session=revoked&role=agent&ack=0", "owner")
	if second.Code != http.StatusGone || strings.Contains(second.Body.String(), "SECRET") {
		t.Fatalf("poll after revocation = %d %q, want 410 with nothing held", second.Code, second.Body.String())
	}
	if r.session("revoked") != nil {
		t.Fatal("a revoked session stayed drainable")
	}
}

// The cap is a bound on the response, not a threshold it is tested against
// before appending: a chunk that would overshoot is split, and its tail is
// served first next time so the stream keeps its order.
func TestDrainCapSplitsRatherThanOvershoots(t *testing.T) {
	cases := map[string][]int{
		"one chunk over the cap":   {maxDrainBytes + 4096},
		"two chunks straddling it": {maxDrainBytes - 1, maxDrainBytes},
		"irregular sizes":          {1, 7777, maxDrainBytes - 3, 999, maxDrainBytes * 2},
		"a full queue of big ones": {maxDrainBytes, maxDrainBytes, maxDrainBytes},
	}
	for name, sizes := range cases {
		t.Run(name, func(t *testing.T) {
			q := newSideQueue()
			var want []byte
			for i, n := range sizes {
				chunk := bytes.Repeat([]byte{byte('a' + i)}, n)
				q.push(chunk)
				want = append(want, chunk...)
			}
			var got []byte
			for ack := uint64(0); len(got) < len(want); {
				data, seq, _, ok := q.take(context.Background(), ack, 250*time.Millisecond)
				if !ok || len(data) == 0 {
					t.Fatalf("queue ran dry after %d of %d bytes", len(got), len(want))
				}
				if len(data) > maxDrainBytes {
					t.Fatalf("one response carried %d bytes, %d over the %d-byte cap", len(data), len(data)-maxDrainBytes, maxDrainBytes)
				}
				if cap(data) > maxDrainBytes {
					t.Fatalf("one response was backed by %d bytes of memory, over the %d-byte cap", cap(data), maxDrainBytes)
				}
				got = append(got, data...)
				ack = seq
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the split stream did not come back in order: %d bytes read, %d queued", len(got), len(want))
			}
		})
	}
}

func TestDrainCoalescingIsBounded(t *testing.T) {
	q := newSideQueue()
	chunk := make([]byte, 64<<10)
	for range 4 * (maxDrainBytes / len(chunk)) { // four times the cap, queued
		q.push(chunk)
	}
	data, _ := q.drain(context.Background(), time.Second)
	if len(data) != maxDrainBytes {
		t.Fatalf("one drain returned %d bytes, want exactly the %d-byte cap", len(data), maxDrainBytes)
	}
}

// A controller or agent built before the acknowledged down protocol sends no
// ack parameter. It must keep working against an updated relay.
func TestDownPollWithoutAckIsFireAndForget(t *testing.T) {
	r, s, sid := tunnelSession(t)
	s.toClient.push([]byte("hello"))
	srv := newMemoryServer(t, r.Handler())
	client := &http.Client{Transport: srv.Transport}

	req, err := http.NewRequest("GET", srv.URL+"/h/down?session="+sid+"&role=client", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-alice")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	q := s.toClient
	q.ackMu.Lock()
	held := len(q.unacked)
	q.ackMu.Unlock()
	if held != 0 {
		t.Fatalf("relay held %d bytes for a client that cannot ack them", held)
	}
}

// TestDownPollThroughputAtRTT is a measuring stick, not an assertion: serial
// polling cannot carry more than maxDrainBytes per round trip, so the cap sets
// the ceiling on a high-latency link. Run it with
// WANCTL_TRANSPORT_THROUGHPUT=1 go test ./internal/relay -run ThroughputAtRTT -v
func TestDownPollThroughputAtRTT(t *testing.T) {
	if os.Getenv("WANCTL_TRANSPORT_THROUGHPUT") == "" {
		t.Skip("set WANCTL_TRANSPORT_THROUGHPUT=1 to measure down-poll throughput")
	}
	r, s, sid := tunnelSession(t)
	srv := newMemoryServer(t, r.Handler())
	polls := 0
	carrier := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/h/down" {
			polls++
			time.Sleep(100 * time.Millisecond) // a 100 ms round trip
		}
		return srv.Transport.RoundTrip(req)
	})
	c, err := httpconn.DialWith(t.Context(), srv.URL, sid, "agent", "tok-alice", &http.Client{Transport: carrier})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const total = 32 << 20
	for range total / (256 << 10) {
		s.toAgent.push(make([]byte, 256<<10))
	}
	start := time.Now()
	if _, err := io.ReadFull(c, make([]byte, total)); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("%d MiB at a 100 ms per-poll round trip: polls=%d elapsed=%s throughput=%.2f MiB/s (cap %d KiB)",
		total>>20, polls, elapsed, float64(total>>20)/elapsed.Seconds(), maxDrainBytes>>10)
}
