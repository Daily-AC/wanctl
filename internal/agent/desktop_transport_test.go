package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/desktop"
	"wanctl/internal/httpconn"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/sessionauth"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// The only fake is the desktop runner. Registration, both carriers, relay
// queues/bridges, mutual TLS, framing, policy, reference consumption and the
// production desktop disconnect watcher all run normally over loopback TCP.
func TestDesktopDisconnectOverRelayCarriers(t *testing.T) {
	for _, agentCarrier := range []string{"http", "ws"} {
		for _, controllerCarrier := range []string{"http", "ws"} {
			for _, how := range []string{"close", "interrupt-exit", "drop"} {
				t.Run("agent="+agentCarrier+"/controller="+controllerCarrier+"/"+how, func(t *testing.T) {
					rig := newDesktopCarrierRig(t, agentCarrier)
					network := newDesktopTestNetwork()
					// On Ctrl-C the CLI returns as soon as its pending read is unblocked.
					// Its CloseOnCancel goroutine need not have posted /h/close yet. Park
					// that POST until process exit drops its sockets, making this ordering
					// deterministic instead of relying on LAN request scheduling.
					network.delayClose = how == "interrupt-exit"
					t.Cleanup(network.drop)
					conn := rig.connect(t, controllerCarrier, network)
					snap, err := rig.agent.desktop.Store.Put(rig.identity.Fingerprint, protocol.DesktopSnapshot{}, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					request := protocol.Message{Kind: protocol.KindDesktop, Action: "act", RequestID: desktop.NewID(), Desktop: &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "wait", Millis: 8000}, {Type: "click", X: 64, Y: 80}}}}
					if err = protocol.WriteMessage(conn, request); err != nil {
						t.Fatal(err)
					}
					readDone := make(chan error, 1)
					go func() { _, err := protocol.ReadMessage(conn); readDone <- err }()
					select {
					case <-rig.entered:
					case err := <-rig.errors:
						t.Fatal(err)
					case <-time.After(5 * time.Second):
						t.Fatal("desktop runner never started")
					}
					if controllerCarrier == "http" {
						waitDesktopCondition(t, func() bool { return rig.clientPolls.Load() > 0 }, "controller has no pending /h/down")
					}
					started := time.Now()
					switch how {
					case "close":
						go conn.Close()
					case "interrupt-exit":
						ctx, cancel := context.WithCancel(context.Background())
						stop := wsconn.CloseOnCancel(ctx, conn)
						defer stop()
						cancel()
						select {
						case <-readDone:
						case <-time.After(2 * time.Second):
							t.Fatal("CLI read did not unblock on interrupt")
						}
						network.drop() // what process exit does to the carrier's TCP sockets
					case "drop":
						network.drop() // no tls.Conn/httpconn Close, no /h/close POST
					}
					select {
					case cancelled := <-rig.cancelled:
						if elapsed := cancelled.Sub(started); elapsed > 2*time.Second {
							t.Fatalf("runner cancellation took %v", elapsed)
						} else {
							t.Logf("runner cancelled after %v", elapsed)
						}
					case <-time.After(2 * time.Second):
						t.Fatalf("runner context not cancelled within 2s after controller %s (calls=%d)", how, rig.calls.Load())
					}
					// Check without relying on the controller receiving a result after it
					// left. Then redeliver the same request through a NEW real session.
					if _, err := rig.agent.desktop.Store.Get(rig.identity.Fingerprint, snap.ID, true, time.Now()); err == nil || !strings.Contains(err.Error(), "already consumed") {
						t.Fatalf("reference was not consumed: %v", err)
					}
					waitDesktopCondition(t, func() bool { return !rig.agent.desktop.Busy() }, "cancelled desktop stayed busy")
					againNetwork := newDesktopTestNetwork()
					t.Cleanup(againNetwork.drop)
					again := rig.connect(t, controllerCarrier, againNetwork)
					defer again.Close()
					if err := protocol.WriteMessage(again, request); err != nil {
						t.Fatal(err)
					}
					result, err := protocol.ReadMessage(again)
					if err != nil || result.DesktopResult == nil || result.DesktopResult.Status != "unknown" {
						t.Fatalf("redelivery = %+v, %v", result, err)
					}
					if rig.calls.Load() != 1 {
						t.Fatalf("act replayed: runner called %d times", rig.calls.Load())
					}
				})
			}
		}
	}
}

type desktopTestNetwork struct {
	mu         sync.Mutex
	sockets    []net.Conn
	dead       bool
	stopped    chan struct{}
	transport  *http.Transport
	delayClose bool
}

func newDesktopTestNetwork() *desktopTestNetwork {
	n := &desktopTestNetwork{stopped: make(chan struct{})}
	n.transport = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.dead {
			return nil, net.ErrClosed
		}
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			n.sockets = append(n.sockets, c)
		}
		return c, err
	}}
	return n
}
func (n *desktopTestNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	if n.delayClose && req.URL.Path == "/h/close" {
		<-n.stopped
		return nil, net.ErrClosed
	}
	return n.transport.RoundTrip(req)
}
func (n *desktopTestNetwork) client() *http.Client {
	return &http.Client{Transport: n, Timeout: 5 * time.Second}
}
func (n *desktopTestNetwork) drop() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dead {
		return
	}
	n.dead = true
	close(n.stopped)
	for _, c := range n.sockets {
		_ = c.Close()
	}
	n.transport.CloseIdleConnections()
}

type desktopCarrierRig struct {
	agent        *Agent
	identity     *transport.Identity
	server       *httptest.Server
	ctx          context.Context
	agentCarrier string
	agentNetwork *desktopTestNetwork
	control      net.Conn
	entered      chan struct{}
	cancelled    chan time.Time
	errors       chan error
	calls        atomic.Int32
	clientPolls  atomic.Int32
	wg           sync.WaitGroup
}

func newDesktopCarrierRig(t *testing.T, carrier string) *desktopCarrierRig {
	t.Helper()
	a := newOptsAgent(t, Options{Mode: policy.ModeBypass})
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(*http.Request) (*http.Response, error) { return notifyHTTPResponse(http.StatusAccepted), nil })}
	identity, err := transport.IdentityFromSeed(bytes.Repeat([]byte{37}, 32), "desktop-disconnect-controller")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rig := &desktopCarrierRig{agent: a, identity: identity, ctx: ctx, agentCarrier: carrier, agentNetwork: newDesktopTestNetwork(), entered: make(chan struct{}, 1), cancelled: make(chan time.Time, 1), errors: make(chan error, 4)}
	h := relay.New(relay.EnvTokenStore("desktop-test-token:alice")).Handler()
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/h/down" && r.URL.Query().Get("role") == "client" {
			rig.clientPolls.Add(1)
			defer rig.clientPolls.Add(-1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		cancel()
		a.stop()
		rig.agentNetwork.drop()
		if rig.control != nil {
			_ = rig.control.Close()
		}
		rig.wg.Wait()
		rig.server.Close()
	})
	if carrier == "ws" {
		control, _, err := wsconn.DialWith(ctx, rig.wsURL("/agent"), admission.Header("desktop-test-token"), rig.agentNetwork.client())
		if err != nil {
			t.Fatal(err)
		}
		rig.control = control
		if err = json.NewEncoder(control).Encode(map[string]string{"op": "register", "device": a.DeviceID(), "device_id": a.DeviceID(), "name": "desktop test", "fingerprint": a.id.Fingerprint}); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}
func (r *desktopCarrierRig) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http") + path
}
func desktopHTTPRequest(ctx context.Context, hc *http.Client, address string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	admission.SetBearer(req, "desktop-test-token")
	return hc.Do(req)
}
func waitDesktopCondition(t *testing.T, ready func() bool, reason string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal(reason)
		}
		time.Sleep(time.Millisecond)
	}
}
func (r *desktopCarrierRig) connect(t *testing.T, carrier string, n *desktopTestNetwork) *tls.Conn {
	t.Helper()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := r.serveOne(); err != nil && r.ctx.Err() == nil {
			r.errors <- err
		}
	}()
	// Registration occurs inside the real /h/poll or WebSocket handler.
	waitDesktopCondition(t, func() bool {
		resp, err := desktopHTTPRequest(r.ctx, r.agentNetwork.client(), r.server.URL+"/peers")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return bytes.Contains(body, []byte(r.agent.DeviceID()))
	}, "test agent did not register")
	var nc net.Conn
	var err error
	if carrier == "http" {
		resp, e := desktopHTTPRequest(r.ctx, n.client(), r.server.URL+"/h/dial?target="+url.QueryEscape("alice/"+r.agent.DeviceID()))
		if e != nil {
			t.Fatal(e)
		}
		var open sessionauth.Open
		err = json.NewDecoder(resp.Body).Decode(&open)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || open.Session == "" {
			t.Fatalf("dial: %+v status=%d err=%v", open, resp.StatusCode, err)
		}
		nc, err = httpconn.DialWith(r.ctx, r.server.URL, open.Session, "client", "desktop-test-token", n.client())
		httpconn.MarkOrdered(nc)
		httpconn.MarkWindow(nc)
	} else {
		nc, _, err = wsconn.DialWith(r.ctx, r.wsURL("/dial?target="+url.QueryEscape("alice/"+r.agent.DeviceID())), admission.Header("desktop-test-token"), n.client())
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	result, err := transport.ClientHandshake(ctx, nc, "", r.identity, transport.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if err = protocol.WriteMessage(result.Conn, protocol.Message{Kind: protocol.KindHello, Name: "desktop test"}); err != nil {
		t.Fatal(err)
	}
	hello, err := protocol.ReadMessage(result.Conn)
	if err != nil || hello.Kind != protocol.KindOK {
		t.Fatalf("hello: %+v %v", hello, err)
	}
	return result.Conn
}
func (r *desktopCarrierRig) serveOne() error {
	var open sessionauth.Open
	var nc net.Conn
	var err error
	if r.agentCarrier == "http" {
		resp, e := desktopHTTPRequest(r.ctx, r.agentNetwork.client(), r.server.URL+"/h/poll?"+url.Values{"device": {r.agent.DeviceID()}, "device_id": {r.agent.DeviceID()}, "name": {"desktop test"}, "fp": {r.agent.id.Fingerprint}, "inst": {"desktop-test"}}.Encode())
		if e != nil {
			return e
		}
		err = json.NewDecoder(resp.Body).Decode(&open)
		resp.Body.Close()
		if err != nil {
			return err
		}
		nc, err = httpconn.DialWith(r.ctx, r.server.URL, open.Session, "agent", "desktop-test-token", r.agentNetwork.client())
		httpconn.MarkOrdered(nc)
		httpconn.MarkWindow(nc)
	} else {
		if err = json.NewDecoder(r.control).Decode(&open); err != nil {
			return err
		}
		nc, _, err = wsconn.DialWith(r.ctx, r.wsURL(open.URL), admission.Header("desktop-test-token"), r.agentNetwork.client())
	}
	if err != nil {
		return err
	}
	defer nc.Close()
	conn, fp, err := transport.ServerHandshake(r.ctx, nc, r.agent.id)
	if err != nil {
		return err
	}
	hello, err := protocol.ReadMessage(conn)
	if err != nil || hello.Kind != protocol.KindHello {
		return fmt.Errorf("hello: %+v %v", hello, err)
	}
	if err = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
		return err
	}
	req, err := protocol.ReadMessage(conn)
	if err != nil || req.Kind != protocol.KindDesktop {
		return fmt.Errorf("desktop request: %+v %v", req, err)
	}
	pending := r.agent.doDesktopUsing(conn, fp, "desktop test", req, sessionAudit{}, true, func(ctx context.Context, _ desktop.Job) (protocol.DesktopResult, []byte, error) {
		r.calls.Add(1)
		r.entered <- struct{}{}
		<-ctx.Done()
		r.cancelled <- time.Now()
		return protocol.DesktopResult{Status: "partial", Error: "controller disconnected", FailedIndex: 0}, nil, nil
	})
	if pending != nil {
		<-pending
	}
	return nil
}
