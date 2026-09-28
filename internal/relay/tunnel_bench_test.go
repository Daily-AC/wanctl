package relay

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/httpconn"
	"wanctl/internal/sessionauth"
)

// delayedCarrier adds a fixed round trip to every request, which is what makes
// the in-flight windows (eight uploads, four numbered downloads) and the bytes
// the relay may hold for a direction matter at all: over loopback with no delay
// a transfer is bound by CPU, not by how much is allowed in flight.
type delayedCarrier struct {
	base http.RoundTripper
	rtt  time.Duration
}

func (d delayedCarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	if d.rtt > 0 {
		time.Sleep(d.rtt)
	}
	return d.base.RoundTrip(req)
}

// BenchmarkTunnelPush moves 64 MiB one way through a session end to end: a
// controller writing through httpconn, the relay's /h/up and /h/down handlers,
// and an agent reading through httpconn, over loopback HTTP. Run it with
//
//	go test ./internal/relay -run '^$' -bench TunnelPush -benchtime 5x
func BenchmarkTunnelPush(b *testing.B) {
	const total = 64 << 20
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	for _, rtt := range []time.Duration{0, 50 * time.Millisecond} {
		b.Run(fmt.Sprintf("rtt=%s", rtt), func(b *testing.B) {
			b.SetBytes(total)
			b.StopTimer() // each run times only its transfer, not the setup
			for range b.N {
				benchPushOnce(b, payload, rtt)
			}
		})
	}
}

func benchPushOnce(b *testing.B, payload []byte, rtt time.Duration) {
	b.Helper()
	const sid = "sess-bench"
	r := New(EnvTokenStore("tok-alice:alice"))
	auth := sessionauth.Open{Session: sid, Device: "home-pc", CallerNamespace: "alice", OwnerNamespace: "alice"}
	s := r.newHTTPSession(sid, auth, delegation.Access{Namespace: "alice"}, "tok-alice")
	defer r.closeHTTPSession(sid, s)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: delayedCarrier{base: tr, rtt: rtt}}
	dial := func(role string) net.Conn {
		nc, err := httpconn.DialWith(b.Context(), srv.URL, sid, role, "tok-alice", hc)
		if err != nil {
			b.Fatal(err)
		}
		// What /h/dial and /h/poll tell a real client before its first byte.
		httpconn.MarkOrdered(nc)
		httpconn.MarkWindow(nc)
		return nc
	}
	client, agent := dial("client"), dial("agent")
	defer agent.Close()

	b.StartTimer()
	wrote := make(chan error, 1)
	go func() {
		_, err := client.Write(payload)
		if err == nil {
			err = client.Close()
		}
		wrote <- err
	}()
	if _, err := io.CopyN(io.Discard, agent, int64(len(payload))); err != nil {
		b.Fatalf("agent read: %v", err)
	}
	if err := <-wrote; err != nil {
		b.Fatalf("controller write: %v", err)
	}
	b.StopTimer()
}
