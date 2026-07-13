package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// TestUpstreamTokenStore verifies delegation to the upstream admin endpoint,
// secret gating, and that hits are served from cache.
func TestUpstreamTokenStore(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tokens/resolve" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Admin-Secret") != "sekrit" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		atomic.AddInt32(&calls, 1)
		var body struct{ Token string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Token == "tok-good" {
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
			return
		}
		http.Error(w, "unknown token", http.StatusNotFound)
	}))
	defer up.Close()

	ts := NewUpstreamTokenStore(up.URL, "sekrit")
	if ns, ok := ts.Resolve("tok-good"); !ok || ns != "alice" {
		t.Fatalf("want alice/true, got %q/%v", ns, ok)
	}
	if _, ok := ts.Resolve("tok-bad"); ok {
		t.Fatal("bad token resolved")
	}
	// Second hit must come from cache (no new upstream call).
	before := atomic.LoadInt32(&calls)
	if ns, ok := ts.Resolve("tok-good"); !ok || ns != "alice" {
		t.Fatalf("cached resolve failed: %q/%v", ns, ok)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Fatal("cache miss: upstream called again for a cached token")
	}

	// Wrong secret must fail closed and not cache.
	bad := NewUpstreamTokenStore(up.URL, "wrong")
	if _, ok := bad.Resolve("tok-good"); ok {
		t.Fatal("resolved with wrong secret")
	}
}

// TestChainTokenStore verifies first-hit-wins across stores.
func TestChainTokenStore(t *testing.T) {
	c := ChainTokenStore{EnvTokenStore("a:ns1"), EnvTokenStore("b:ns2")}
	if ns, ok := c.Resolve("b"); !ok || ns != "ns2" {
		t.Fatalf("want ns2, got %q/%v", ns, ok)
	}
	if _, ok := c.Resolve("nope"); ok {
		t.Fatal("unknown token resolved")
	}
}

// TestAdminTokenResolveEndpoint verifies the relay-side endpoint that satellite
// relays call: secret-gated, resolves via the relay's own token store.
func TestAdminTokenResolveEndpoint(t *testing.T) {
	r := New(EnvTokenStore("tok-x:***REMOVED***"))
	r.SetAdminSecret("s3")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	post := func(secret, token string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/admin/tokens/resolve", jsonBody(map[string]string{"token": token}))
		req.Header.Set("X-Admin-Secret", secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("s3", "tok-x")
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct{ Namespace string }
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Namespace != "***REMOVED***" {
		t.Fatalf("want ***REMOVED***, got %q", out.Namespace)
	}
	if resp := post("s3", "nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown token: want 404, got %d", resp.StatusCode)
	}
	if resp := post("bad", "tok-x"); resp.StatusCode != 403 {
		t.Fatalf("bad secret: want 403, got %d", resp.StatusCode)
	}
}
