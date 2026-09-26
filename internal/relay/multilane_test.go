package relay

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wanctl/internal/httpconn"
)

type windowFaultTransport struct {
	base                      http.RoundTripper
	stripWindow               bool
	fault                     bool
	mu                        sync.Mutex
	dropped, truncated, wants int
}

func (f *windowFaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/h/down" && req.URL.Query().Has(httpconn.DownWantParam) {
		f.mu.Lock()
		f.wants++
		f.mu.Unlock()
	}
	resp, err := f.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if f.stripWindow {
		resp.Header.Del(httpconn.DownWindowCapabilityHeader)
	}
	if !f.fault {
		return resp, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Path == "/h/up" && req.URL.Query().Get(httpconn.UpSeqParam) == "3" && f.dropped == 0 {
		f.dropped++
		resp.Body.Close()
		return nil, errors.New("injected lost upload response")
	}
	if req.URL.Path == "/h/down" && resp.StatusCode == http.StatusOK && req.URL.Query().Get(httpconn.DownWantParam) == "3" && f.truncated == 0 {
		f.truncated++
		resp.Body = &truncatedBody{inner: resp.Body, remaining: 1024}
	}
	return resp, nil
}

func TestMultiLane32MiBBidirectional(t *testing.T) {
	for _, mode := range []struct {
		name                       string
		oldReader, oldRelay, fault bool
	}{
		{"new", false, false, false},
		{"faults", false, false, true},
		{"old reader", true, false, false},
		{"old relay advertisement", false, true, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			r, s, sid := tunnelSession(t)
			srv := httptest.NewServer(r.Handler())
			defer srv.Close()
			makeConn := func(role string, strip bool) (net.Conn, *windowFaultTransport) {
				if !strip && !mode.fault {
					nc, err := httpconn.Dial(t.Context(), srv.URL, sid, role, "tok-alice")
					if err != nil {
						t.Fatal(err)
					}
					httpconn.MarkOrdered(nc)
					httpconn.MarkWindow(nc)
					return nc, nil
				}
				tr := &windowFaultTransport{base: http.DefaultTransport, stripWindow: strip, fault: mode.fault}
				nc, err := httpconn.DialWith(t.Context(), srv.URL, sid, role, "tok-alice", &http.Client{Transport: tr})
				if err != nil {
					t.Fatal(err)
				}
				httpconn.MarkOrdered(nc)
				if !strip {
					httpconn.MarkWindow(nc)
				}
				return nc, tr
			}
			client, ct := makeConn("client", mode.oldRelay || mode.oldReader)
			agent, at := makeConn("agent", mode.oldRelay)
			defer client.Close()
			defer agent.Close()
			payloadC := bytes.Repeat([]byte("controller-012345"), (32<<20)/len("controller-012345"))
			payloadA := bytes.Repeat([]byte("device-abcdefghijkl"), (32<<20)/len("device-abcdefghijkl"))
			check := func(src []byte, dst net.Conn) error {
				got := make([]byte, len(src))
				if _, err := io.ReadFull(dst, got); err != nil {
					return err
				}
				if sha256.Sum256(src) != sha256.Sum256(got) {
					return errors.New("SHA-256 mismatch")
				}
				return nil
			}
			errs := make(chan error, 4)
			go func() { _, err := client.Write(payloadC); errs <- err }()
			go func() { _, err := agent.Write(payloadA); errs <- err }()
			for range 2 {
				select {
				case err := <-errs:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(45 * time.Second):
					t.Fatal("32 MiB transfer timed out")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				s.toAgent.seqMu.Lock()
				upA := s.toAgent.seqNext
				s.toAgent.seqMu.Unlock()
				s.toClient.seqMu.Lock()
				upC := s.toClient.seqNext
				s.toClient.seqMu.Unlock()
				if upA >= 5 && upC >= 5 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			go func() { errs <- check(payloadC, agent) }()
			go func() { errs <- check(payloadA, client) }()
			for range 2 {
				select {
				case err := <-errs:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(45 * time.Second):
					t.Fatal("32 MiB read timed out")
				}
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			if err := agent.Close(); err != nil {
				t.Fatal(err)
			}
			if mode.fault {
				for _, tr := range []*windowFaultTransport{ct, at} {
					tr.mu.Lock()
					dropped, truncated := tr.dropped, tr.truncated
					tr.mu.Unlock()
					if dropped != 1 || truncated != 1 {
						t.Errorf("faults fired: dropped=%d truncated=%d", dropped, truncated)
					}
				}
			}
			if mode.oldReader && ct.wants != 0 {
				t.Fatalf("old reader sent %d want polls", ct.wants)
			}
			if mode.oldRelay && (ct.wants != 0 || at.wants != 0) {
				t.Fatalf("old relay saw window polls: client=%d agent=%d", ct.wants, at.wants)
			}
		})
	}
}
