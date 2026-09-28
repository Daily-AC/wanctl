package relay

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A reader never acknowledges the last chunk it reads: it closes instead of
// polling again. Once both ends have closed nobody will read the session
// again, so it has to leave then, with what it held, rather than stay
// registered — and counted against its namespace — until the sweeper's
// retention for unacknowledged chunks runs out.
func TestSessionLeavesOnceBothEndsHaveClosed(t *testing.T) {
	r, s, sid := tunnelSession(t)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	client, agent := dialPair(t, srv.URL, sid, &http.Client{Transport: &http.Transport{}})
	cert := testTLSCert(t)
	answered := make(chan error, 1)
	go func() {
		device := tls.Server(agent, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		request := make([]byte, 7)
		if _, err := io.ReadFull(device, request); err != nil {
			answered <- err
			return
		}
		_, err := device.Write(marked(3, 256<<10))
		device.Close()
		answered <- err
	}()
	controller := tls.Client(client, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if _, err := controller.Write([]byte("command")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(controller, make([]byte, 256<<10)); err != nil {
		t.Fatal(err)
	}
	controller.Close()
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.session(sid) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.session(sid) != nil {
		t.Fatalf("the session was still registered with both ends closed (unacknowledged: %d bytes to the controller, %d to the device)",
			residentOf(s.toClient), residentOf(s.toAgent))
	}
	if got := r.resident.resident(); got != 0 {
		t.Fatalf("the relay still counts %d bytes for a session both ends closed", got)
	}
}

// Closing is the reader saying it will not read again, so what is left for it
// goes at once; the other direction is untouched, and its reader still gets
// every byte.
func TestClosingDropsOnlyWhatWasLeftForTheCloser(t *testing.T) {
	for _, viaBridge := range []bool{false, true} {
		name := "peer posts /h/close"
		if viaBridge {
			name = "websocket leg ends"
		}
		t.Run(name, func(t *testing.T) {
			r, s, sid := tunnelSession(t)
			s.toClient.push(marked(1, 3*mebibyte/2)) // the controller will not read this
			s.toAgent.push(marked(2, 3*mebibyte/2))  // the device reads all of this
			if viaBridge {
				r.httpSessionConn(sid, s, "client").(io.Closer).Close()
			} else {
				req := httptest.NewRequest(http.MethodPost, "/h/close?session="+sid+"&role=client", nil)
				req.Header.Set("Authorization", "Bearer tok-alice")
				rec := httptest.NewRecorder()
				r.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("close = %d", rec.Code)
				}
			}
			if got := residentOf(s.toClient); got != 0 {
				t.Fatalf("%d bytes are still held for a controller that closed", got)
			}
			got, last := readAcked(t, r, sid, "agent", s.toAgent, 3*mebibyte/2, maxResidentPerDirection)
			if string(got) != string(marked(2, 3*mebibyte/2)) {
				t.Fatalf("the device read %d bytes, want all it was sent", len(got))
			}
			if end := downPoll(t, r, sid, "agent", last); end.Code != http.StatusGone {
				t.Fatalf("poll past the end = %d, want 410", end.Code)
			}
			if r.session(sid) != nil {
				t.Fatal("the session stayed registered after its last reader finished")
			}
			if got := r.resident.resident(); got != 0 {
				t.Fatalf("the relay still counts %d bytes", got)
			}
		})
	}
}
