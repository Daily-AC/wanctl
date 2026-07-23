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
			sess, _, err := wsconn.Dial(ctx, base+msg["url"], nil)
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
