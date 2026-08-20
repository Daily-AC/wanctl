package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/sessionauth"
	"wanctl/internal/wsconn"
)

type pipeRelayServer struct {
	httpBase string
	wsBase   string
	client   *http.Client
}

type auditRecord struct {
	namespace string
	device    string
	event     string
}

type channelAuditor chan<- auditRecord

func (a channelAuditor) Audit(namespace, device, event string) {
	a <- auditRecord{namespace: namespace, device: device, event: event}
}

type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) DialContext(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	case <-l.done:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return testAddr("pipe-listener") }

type testAddr string

func (a testAddr) Network() string { return "pipe" }
func (a testAddr) String() string  { return string(a) }

func newPipeRelayServer(t *testing.T, h http.Handler) *pipeRelayServer {
	t.Helper()
	ln := newPipeListener()
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		},
	}
	t.Cleanup(func() {
		_ = srv.Close()
		transport.CloseIdleConnections()
	})
	return &pipeRelayServer{
		httpBase: "http://relay.test",
		wsBase:   "ws://relay.test",
		client:   &http.Client{Transport: transport, Timeout: 20 * time.Second},
	}
}

func (s *pipeRelayServer) request(method, path, token string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, s.httpBase+path, body)
	if err != nil {
		return nil, err
	}
	admission.SetBearer(req, token)
	return s.client.Do(req)
}

func (s *pipeRelayServer) wsDial(ctx context.Context, path, token string) (net.Conn, *http.Response, error) {
	return wsconn.DialWith(ctx, s.wsBase+path, admission.Header(token), s.client)
}

func registerWSAgent(t *testing.T, r *Relay, s *pipeRelayServer, token, device string) net.Conn {
	t.Helper()
	ctrl, _, err := s.wsDial(context.Background(), "/agent", token)
	if err != nil {
		t.Fatalf("dial WS agent control: %v", err)
	}
	if err := json.NewEncoder(ctrl).Encode(map[string]string{"op": "register", "device": device}); err != nil {
		ctrl.Close()
		t.Fatalf("register WS agent: %v", err)
	}
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.agents["alice/"+device] != nil
	}, "WS agent registration")
	return ctrl
}

func startPipeHTTPPoll(s *pipeRelayServer, token, device string) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() {
		resp, err := s.request(http.MethodGet, "/h/poll?device="+device+"&inst=test", token, nil)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		defer resp.Body.Close()
		var open sessionauth.Open
		err = json.NewDecoder(resp.Body).Decode(&open)
		ch <- pollResult{status: resp.StatusCode, open: open, err: err}
	}()
	return ch
}

func waitFor(t *testing.T, ready func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func dialHTTPSession(t *testing.T, s *pipeRelayServer, token, target string) string {
	t.Helper()
	resp, err := s.request(http.MethodGet, "/h/dial?target="+target, token, nil)
	if err != nil {
		t.Fatalf("HTTP dial: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP dial status = %d, body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct{ Session string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode HTTP dial: %v", err)
	}
	if out.Session == "" {
		t.Fatal("HTTP dial returned an empty session")
	}
	return out.Session
}

func uploadHTTP(t *testing.T, s *pipeRelayServer, token, sid, role string, data []byte) {
	t.Helper()
	resp, err := s.request(http.MethodPost, fmt.Sprintf("/h/up?session=%s&role=%s", sid, role), token, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("HTTP upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP upload status = %d, body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func downloadHTTP(t *testing.T, s *pipeRelayServer, token, sid, role string) (int, []byte) {
	t.Helper()
	resp, err := s.request(http.MethodGet, fmt.Sprintf("/h/down?session=%s&role=%s", sid, role), token, nil)
	if err != nil {
		t.Fatalf("HTTP download: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read HTTP download: %v", err)
	}
	return resp.StatusCode, body
}

func closeHTTPSession(t *testing.T, s *pipeRelayServer, token, sid string) {
	t.Helper()
	resp, err := s.request(http.MethodPost, "/h/close?session="+sid, token, nil)
	if err != nil {
		t.Fatalf("HTTP close: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP close status = %d", resp.StatusCode)
	}
}

func expectDialAudit(t *testing.T, audits <-chan auditRecord, device string) {
	t.Helper()
	select {
	case got := <-audits:
		want := (auditRecord{namespace: "alice", device: device, event: "dial"})
		if got != want {
			t.Fatalf("audit = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial was not audited")
	}
}

func TestHTTPControllerBridgesToWSAgent(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	audits := make(chan auditRecord, 1)
	r.SetAuditor(channelAuditor(audits))
	s := newPipeRelayServer(t, r.Handler())
	ctrl := registerWSAgent(t, r, s, "tok-alice", "ws-box")
	defer ctrl.Close()

	agentSession := make(chan net.Conn, 1)
	agentErr := make(chan error, 1)
	go func() {
		var open sessionauth.Open
		if err := json.NewDecoder(bufio.NewReader(ctrl)).Decode(&open); err != nil {
			agentErr <- err
			return
		}
		if open.Op != "open" || open.URL != "/session/"+open.Session {
			agentErr <- fmt.Errorf("invalid WS open message: %+v", open)
			return
		}
		nc, _, err := s.wsDial(context.Background(), open.URL, "tok-alice")
		if err != nil {
			agentErr <- err
			return
		}
		agentSession <- nc
	}()

	sid := dialHTTPSession(t, s, "tok-alice", "alice/ws-box")
	expectDialAudit(t, audits, "ws-box")
	var agent net.Conn
	select {
	case agent = <-agentSession:
	case err := <-agentErr:
		t.Fatalf("WS agent session: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("WS agent did not open the session")
	}
	defer agent.Close()

	uploadHTTP(t, s, "tok-alice", sid, "client", []byte("ping"))
	buf := make([]byte, 16)
	n, err := agent.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("WS agent read = %q, %v", buf[:n], err)
	}
	if _, err := agent.Write([]byte("echo:ping")); err != nil {
		t.Fatalf("WS agent write: %v", err)
	}
	status, body := downloadHTTP(t, s, "tok-alice", sid, "client")
	if status != http.StatusOK || string(body) != "echo:ping" {
		t.Fatalf("HTTP controller download = %d %q", status, body)
	}

	closeHTTPSession(t, s, "tok-alice", sid)
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := agent.Read(buf); err == nil {
		t.Fatal("WS agent remained open after HTTP controller closed the session")
	}
}

func TestWSControllerBridgesToHTTPAgent(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	audits := make(chan auditRecord, 1)
	r.SetAuditor(channelAuditor(audits))
	s := newPipeRelayServer(t, r.Handler())
	poll := startPipeHTTPPoll(s, "tok-alice", "http-box")
	waitHTTPAgentInst(t, r, "alice/http-box", "test")

	controller, _, err := s.wsDial(context.Background(), "/dial?target=alice/http-box", "tok-alice")
	if err != nil {
		t.Fatalf("WS controller dial: %v", err)
	}
	defer controller.Close()
	expectDialAudit(t, audits, "http-box")
	select {
	case result := <-poll:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("HTTP agent poll = %+v", result)
		}
		if result.open.Session == "" {
			t.Fatal("HTTP agent received an empty session")
		}
		if _, err := controller.Write([]byte("ping")); err != nil {
			t.Fatalf("WS controller write: %v", err)
		}
		status, body := downloadHTTP(t, s, "tok-alice", result.open.Session, "agent")
		if status != http.StatusOK || string(body) != "ping" {
			t.Fatalf("HTTP agent download = %d %q", status, body)
		}
		uploadHTTP(t, s, "tok-alice", result.open.Session, "agent", []byte("echo:ping"))
		buf := make([]byte, 16)
		n, err := controller.Read(buf)
		if err != nil || string(buf[:n]) != "echo:ping" {
			t.Fatalf("WS controller read = %q, %v", buf[:n], err)
		}
		r.hmu.Lock()
		session := r.hsess[result.open.Session]
		r.hmu.Unlock()
		if session == nil {
			t.Fatal("HTTP session disappeared before WS controller closed")
		}
		if err := controller.Close(); err != nil {
			t.Fatalf("close WS controller: %v", err)
		}
		status, _ = downloadHTTP(t, s, "tok-alice", result.open.Session, "agent")
		if status != http.StatusGone && status != http.StatusNotFound {
			t.Fatalf("HTTP agent download after WS close = %d, want 404 or 410", status)
		}
		waitFor(t, func() bool {
			r.hmu.Lock()
			defer r.hmu.Unlock()
			_, exists := r.hsess[result.open.Session]
			return !exists
		}, "HTTP session cleanup")
		for name, done := range map[string]<-chan struct{}{
			"toClient": session.toClient.done,
			"toAgent":  session.toAgent.done,
		} {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s queue remained open after WS controller closed", name)
			}
		}
	case <-time.After(2 * time.Second):
		controller.Close()
		t.Fatal("HTTP agent did not receive the session")
	}
}

func TestCrossTransportDialOfflineReturnsNotFound(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	s := newPipeRelayServer(t, r.Handler())

	resp, err := s.request(http.MethodGet, "/h/dial?target=alice/missing", "tok-alice", nil)
	if err != nil {
		t.Fatalf("HTTP offline dial: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HTTP offline dial status = %d, want 404", resp.StatusCode)
	}

	_, resp, err = s.wsDial(context.Background(), "/dial?target=alice/missing", "tok-alice")
	if err == nil {
		t.Fatal("WS offline dial unexpectedly succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("WS offline dial response = %+v, want 404", resp)
	}
}

func TestPeerEndpointsReturnTransportUnion(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	s := newPipeRelayServer(t, r.Handler())
	ctrl := registerWSAgent(t, r, s, "tok-alice", "ws-box")
	defer ctrl.Close()
	duplicateCtrl := registerWSAgent(t, r, s, "tok-alice", "http-box")
	defer duplicateCtrl.Close()
	_ = startPipeHTTPPoll(s, "tok-alice", "http-box")
	waitHTTPAgentInst(t, r, "alice/http-box", "test")

	for _, path := range []string{"/peers", "/h/peers"} {
		resp, err := s.request(http.MethodGet, path, "tok-alice", nil)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var got struct {
			Namespace string   `json:"namespace"`
			Devices   []string `json:"devices"`
		}
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, decode=%v", path, resp.StatusCode, err)
		}
		sort.Strings(got.Devices)
		if got.Namespace != "alice" || strings.Join(got.Devices, ",") != "http-box,ws-box" {
			t.Fatalf("GET %s = %+v", path, got)
		}
	}
}

func TestHTTPControllerSessionClosesWhenWSAgentDoesNotConnect(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	s := newPipeRelayServer(t, r.Handler())
	ctrl := registerWSAgent(t, r, s, "tok-alice", "silent-box")
	defer ctrl.Close()

	openReceived := make(chan sessionauth.Open, 1)
	go func() {
		var open sessionauth.Open
		if json.NewDecoder(bufio.NewReader(ctrl)).Decode(&open) == nil {
			openReceived <- open
		}
	}()
	sid := dialHTTPSession(t, s, "tok-alice", "alice/silent-box")
	select {
	case open := <-openReceived:
		if open.Session != sid {
			t.Fatalf("WS open session = %q, want %q", open.Session, sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("silent WS agent did not receive the open request")
	}

	started := time.Now()
	status, _ := downloadHTTP(t, s, "tok-alice", sid, "client")
	if status != http.StatusGone {
		t.Fatalf("HTTP controller download status = %d, want 410", status)
	}
	if elapsed := time.Since(started); elapsed < 14*time.Second || elapsed > 17*time.Second {
		t.Fatalf("session closed after %v, want approximately 15s", elapsed)
	}
}
