package relay

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/httpconn"
)

// Reset a REAL idle request's TCP connection, not a mock response body or
// context. downPoll must perform its existing retry and keep both directions
// usable. A fresh TCP connection per request prevents net/http's transparent
// retry on a reused connection from hiding the carrier's own retry path.
func TestIdleControllerPollSurvivesConnectionReset(t *testing.T) {
	r, s, sid := tunnelSession(t)
	handler := r.Handler()
	var polls atomic.Int32
	cancelled, retried := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := int32(0)
		if req.URL.Path == "/h/down" && req.URL.Query().Get("role") == "client" {
			n = polls.Add(1)
			if req.URL.Query().Has(httpconn.DownWantParam) {
				t.Error("expected an unnumbered idle poll")
			}
			if n == 3 {
				close(retried)
			}
		}
		handler.ServeHTTP(w, req)
		if n == 2 && req.Context().Err() != nil {
			close(cancelled)
		}
	}))
	defer server.Close()
	sockets := make(chan *net.TCPConn, 16)
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			sockets <- c.(*net.TCPConn)
		}
		return c, err
	}}
	defer tr.CloseIdleConnections()
	controller, err := httpconn.DialWith(t.Context(), server.URL, sid, "client", "tok-alice", &http.Client{Transport: tr, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	agent, err := httpconn.DialWith(t.Context(), server.URL, sid, "agent", "tok-alice", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	// Receiving one real response establishes DownAck capability, exactly what
	// permits downPoll to retry a failed carrier request in production.
	if _, err = agent.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(controller, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	<-sockets // the completed bootstrap request, already closed by HTTP
	type readResult struct {
		data []byte
		err  error
	}
	read := func(c net.Conn, n int) <-chan readResult {
		done := make(chan readResult, 1)
		go func() { data := make([]byte, n); _, err := io.ReadFull(c, data); done <- readResult{data, err} }()
		return done
	}
	const payload = "agent output after the controller reconnects"
	controllerRead := read(controller, len(payload))
	agentRead := read(agent, 1)
	var idleSocket *net.TCPConn
	select {
	case idleSocket = <-sockets:
	case <-time.After(2 * time.Second):
		t.Fatal("controller never opened its idle poll")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		serving, _ := s.toClient.undelivered()
		if polls.Load() == 2 && serving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller poll did not park on the empty queue")
		}
		time.Sleep(time.Millisecond)
	}
	resetAt := time.Now()
	if err = idleSocket.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err = idleSocket.Close(); err != nil {
		t.Fatal(err)
	} // TCP RST, with no carrier Close
	select {
	case <-cancelled:
		t.Log("relay observed the real idle request cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not observe the request cancellation; no teardown regression was established")
	}
	select {
	case <-retried:
		t.Logf("downPoll retried after %v", time.Since(resetAt))
	case <-time.After(2 * time.Second):
		t.Fatal("downPoll did not retry the failed idle request")
	}
	// A retry must clear the pending disconnect, not merely race some output
	// ahead of its expiry. Keep the replacement poll idle beyond the 2 s grace.
	select {
	case got := <-controllerRead:
		t.Fatalf("controller read after reset: %v (relay observed cancellation and downPoll retried)", got.err)
	case <-time.After(2200 * time.Millisecond):
	}
	select {
	case got := <-agentRead:
		t.Fatalf("agent saw EOF/error after controller retry: %v", got.err)
	default:
	}
	if _, err = agent.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-controllerRead:
		if got.err != nil || !bytes.Equal(got.data, []byte(payload)) {
			t.Fatalf("controller read after retry: %q, %v", got.data, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent output did not reach the reconnected controller")
	}
	if _, err = controller.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-agentRead:
		if got.err != nil || string(got.data) != "x" {
			t.Fatalf("agent read after retry: %q, %v", got.data, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent side did not remain readable")
	}
}
