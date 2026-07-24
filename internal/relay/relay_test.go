package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/sessionauth"
	"wanctl/internal/wsconn"
)

// startAgent connects to /agent, registers a device, and for each "open"
// control message dials the session and echoes a fixed greeting.
func startAgent(t *testing.T, base, token, device string) {
	t.Helper()
	ctx := context.Background()
	ctrl, _, err := wsconn.Dial(ctx, base+"/agent?token="+token, nil)
	if err != nil {
		t.Fatalf("agent ctrl dial: %v", err)
	}
	enc := json.NewEncoder(ctrl)
	dec := json.NewDecoder(bufio.NewReader(ctrl))
	if err := enc.Encode(map[string]string{"op": "register", "device": device}); err != nil {
		t.Fatalf("register: %v", err)
	}
	go func() {
		defer ctrl.Close()
		for {
			var msg map[string]string
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg["op"] != "open" {
				continue
			}
			sess, _, err := wsconn.Dial(ctx, base+msg["url"], admission.Header(token))
			if err != nil {
				return
			}
			go func() {
				defer sess.Close()
				buf := make([]byte, 16)
				n, _ := sess.Read(buf)
				sess.Write([]byte("echo:" + string(buf[:n])))
				time.Sleep(50 * time.Millisecond)
			}()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let registration land
}

func TestRelayPipesSession(t *testing.T) {
	ts := EnvTokenStore("tok-alice:alice")
	srv := httptest.NewServer(New(ts).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, "tok-alice", "home-pc")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := wsconn.Dial(ctx, base+"/dial?token=tok-alice&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("got %q", got)
	}
}

func TestRelayPipesRepeatedSessions(t *testing.T) {
	ts := EnvTokenStore("tok-alice:alice")
	srv := httptest.NewServer(New(ts).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, "tok-alice", "home-pc")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 50; i++ {
		conn, _, err := wsconn.Dial(ctx, base+"/dial?token=tok-alice&target=alice/home-pc", nil)
		if err != nil {
			t.Fatalf("session %d dial: %v", i, err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			conn.Close()
			t.Fatalf("session %d write: %v", i, err)
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		conn.Close()
		if err != nil {
			t.Fatalf("session %d read: %v", i, err)
		}
		if got := string(buf[:n]); got != "echo:ping" {
			t.Fatalf("session %d got %q", i, got)
		}
	}
}

func TestRelayRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(New(EnvTokenStore("tok-alice:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := wsconn.Dial(ctx, base+"/dial?token=bad&target=alice/home-pc", nil)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestLegacyQueryAuthIsDeprecatedAndBearerIsPreferred(t *testing.T) {
	r := New(EnvTokenStore("secret-token:alice"))
	h := r.Handler()

	legacyReq := httptest.NewRequest("GET", "/peers?token=secret-token", nil)
	legacyResp := httptest.NewRecorder()
	h.ServeHTTP(legacyResp, legacyReq)
	if legacyResp.Code != http.StatusOK || legacyResp.Header().Get("Deprecation") != "true" {
		t.Fatalf("legacy response: status=%d deprecation=%q", legacyResp.Code, legacyResp.Header().Get("Deprecation"))
	}
	if warning := legacyResp.Header().Get("Warning"); warning == "" || strings.Contains(warning, "secret-token") {
		t.Fatal("legacy response must carry a credential-free deprecation warning")
	}

	bearerReq := httptest.NewRequest("GET", "/peers", nil)
	bearerReq.Header.Set("Authorization", "Bearer secret-token")
	bearerResp := httptest.NewRecorder()
	h.ServeHTTP(bearerResp, bearerReq)
	if bearerResp.Code != http.StatusOK || bearerResp.Header().Get("Deprecation") != "" {
		t.Fatalf("bearer response: status=%d deprecation=%q", bearerResp.Code, bearerResp.Header().Get("Deprecation"))
	}

	malformedReq := httptest.NewRequest("GET", "/peers?token=secret-token", nil)
	malformedReq.Header.Set("Authorization", "invalid secret-header")
	malformedResp := httptest.NewRecorder()
	h.ServeHTTP(malformedResp, malformedReq)
	if malformedResp.Code != http.StatusUnauthorized {
		t.Fatalf("malformed bearer status = %d, want 401", malformedResp.Code)
	}
	if body := malformedResp.Body.String(); strings.Contains(body, "secret-token") || strings.Contains(body, "secret-header") {
		t.Fatal("authentication error reflected a credential")
	}
}

func TestDialAllowedPortal(t *testing.T) {
	r := New(EnvTokenStore("ptok:portal,utok:alice"))
	r.SetPortalNS("portal")
	// portal may dial any namespace's device
	if _, auth, ok := r.dialAllowed("portal", "alice/legion"); !ok || auth.Capabilities != sessionauth.FullCapabilities {
		t.Fatal("portal should be allowed to dial alice/legion")
	}
	if _, auth, ok := r.dialAllowed("alice", "alice/legion"); !ok || auth.Capabilities != sessionauth.FullCapabilities {
		t.Fatal("device owner should receive full capabilities")
	}
	// a normal user still cannot cross namespaces without ACL
	if _, _, ok := r.dialAllowed("alice", "bob/box"); ok {
		t.Fatal("alice should not dial bob without ACL")
	}

	// unset portalNS: the portal path must be entirely inert (no bypass)
	r2 := New(EnvTokenStore("ptok:portal,utok:alice"))
	// SetPortalNS intentionally NOT called
	if _, _, ok := r2.dialAllowed("portal", "alice/legion"); ok {
		t.Fatal("unset portalNS must not grant any dial bypass")
	}
}

type staticACL string

func (p staticACL) ACLPerms(_, _, _ string) (string, bool) { return string(p), true }

func TestDialAllowedParsesACLPermissionsStrictly(t *testing.T) {
	r := New(EnvTokenStore("tok:reader"))
	r.SetACL(staticACL("read,exec"))
	_, auth, ok := r.dialAllowed("reader", "owner/home-pc")
	if !ok || auth.Capabilities != sessionauth.Read|sessionauth.Exec {
		t.Fatalf("authorization = %#v, ok=%v", auth, ok)
	}

	r.SetACL(staticACL("read,unknown"))
	if _, _, ok := r.dialAllowed("reader", "owner/home-pc"); ok {
		t.Fatal("unknown ACL permission must fail closed")
	}
	r.SetACL(staticACL("read,console"))
	if _, _, ok := r.dialAllowed("reader", "owner/home-pc"); ok {
		t.Fatal("owner-only capability must fail closed")
	}
}
