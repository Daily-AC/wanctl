package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// The whole path with nothing faked: Postgres-backed relay, a real agent, the
// real HTTP endpoints that issue and revoke, and a real client holding the
// token that came out of them. Skipped without WANCTL_TEST_POSTGRES.
func TestScopedTokenEndToEndThroughPostgresRelay(t *testing.T) {
	dsn := os.Getenv("WANCTL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set WANCTL_TEST_POSTGRES to run the scoped token end-to-end test")
	}
	for _, carrier := range []string{"ws", "http"} {
		t.Run(carrier, func(t *testing.T) { scopedEndToEnd(t, dsn, carrier) })
	}
}

func scopedEndToEnd(t *testing.T, dsn, carrier string) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	schema := "scoped_e2e_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec("DROP SCHEMA " + schema + " CASCADE"); base.Close() })
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	p, err := relay.OpenPG(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	full, err := p.IssueToken("alice", "owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(p)
	r.SetAdmin(p)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	newAgent := func(name string) *Agent {
		// An agent takes its identity and device id from the config directory
		// when it is built, so each one needs its own.
		t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
		a, err := New(Options{RelayURL: wsBase, Token: full, Name: name, Mode: policy.ModeBypass, AutoYes: true, Transport: carrier})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		go a.Run(ctx)
		return a
	}
	target, other := newAgent("target"), newAgent("other")

	cid, err := transport.IdentityFromSeed(bytes.Repeat([]byte{29}, 32), "scoped-e2e-controller")
	if err != nil {
		t.Fatal(err)
	}
	pins := transport.NewMemStore()
	owner := client.NewWith(cid, pins, wsBase, full, carrier)
	owner.SetLabel("owner")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if peers, err := owner.Peers(ctx); err == nil && len(peers) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agents never registered")
		}
	}
	targetRoute, otherRoute := "alice/"+target.DeviceID(), "alice/"+other.DeviceID()
	pins.Pin(targetRoute, target.id.Fingerprint, false)
	pins.Pin(otherRoute, other.id.Fingerprint, false)

	api := func(token, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// Issue through the real endpoint, naming the device by its name.
	command := "printf scoped-ok"
	code, body := api(full, http.MethodPost, "/u/scoped-tokens", map[string]any{"device": "target", "command": command, "days": 1, "label": "e2e"})
	if code != 200 {
		t.Fatalf("issue: %d %s", code, body)
	}
	var issued struct {
		Token string
		Info  relay.ScopedToken
	}
	if err := json.Unmarshal(body, &issued); err != nil || !strings.HasPrefix(issued.Token, "wcs_") {
		t.Fatalf("issue body: %s", body)
	}
	// Bad requests are refused at the door.
	for name, in := range map[string]map[string]any{
		"newline": {"device": "target", "command": "a\nb"}, "days": {"device": "target", "command": "x", "days": 999}, "nothing": {},
	} {
		if code, body := api(full, http.MethodPost, "/u/scoped-tokens", in); code < 400 {
			t.Fatalf("%s accepted: %d %s", name, code, body)
		}
	}

	scoped := client.NewWith(cid, pins, wsBase, issued.Token, carrier)
	scoped.SetLabel("scoped e2e")
	run := func(c *client.Client, route, command string, oneshot bool) (string, error) {
		var out bytes.Buffer
		_, err := c.ExecTo(ctx, client.ExecRequest{Target: route, Command: command, OneShot: oneshot}, &out, io.Discard)
		return out.String(), err
	}

	// 1. Only the allowed command, on the allowed device.
	if out, err := run(scoped, targetRoute, command, true); err != nil || out != "scoped-ok" {
		t.Fatalf("scoped command: %q %v", out, err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, c := range []string{"touch " + marker, command + " x", command + "; touch " + marker, command + " | touch " + marker, command + " && touch " + marker, "", " " + command} {
		if out, err := run(scoped, targetRoute, c, true); err == nil || out != "" {
			t.Fatalf("command %q was not refused: %q %v", c, out, err)
		}
	}
	if out, err := run(scoped, targetRoute, command, false); err == nil || out != "" {
		t.Fatalf("persistent-shell form was not refused: %q %v", out, err)
	}
	if out, err := run(scoped, otherRoute, command, true); err == nil || out != "" {
		t.Fatalf("scoped token reached another device of the same owner: %q %v", out, err)
	}
	if err := scoped.PushBytes(ctx, targetRoute, marker, []byte("x"), 0600); err == nil {
		t.Fatal("scoped token wrote a file")
	}
	if err := scoped.Pull(ctx, targetRoute, marker, filepath.Join(t.TempDir(), "got")); err == nil {
		t.Fatal("scoped token read a file")
	}
	if err := scoped.LogsTo(ctx, targetRoute, "", "", "", 1, io.Discard); err == nil {
		t.Fatal("scoped token read logs")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a refused request ran: %v", err)
	}
	// 2. Relay interfaces beyond dialling.
	for _, probe := range []struct{ method, path string }{
		{"GET", "/u/scoped-tokens"}, {"POST", "/u/scoped-tokens"}, {"GET", "/u/shares"}, {"GET", "/u/friends"}, {"POST", "/u/shares/grant"},
	} {
		if code, _ := api(issued.Token, probe.method, probe.path, map[string]any{}); code != 401 && code != 403 {
			t.Fatalf("scoped token on %s %s: %d", probe.method, probe.path, code)
		}
	}
	if peers, err := scoped.Peers(ctx); err != nil || len(peers) != 1 || peers[0] != target.DeviceID() {
		t.Fatalf("scoped token sees %v (%v)", peers, err)
	}
	// 3. The owner's full token and ordinary commands are unchanged.
	if out, err := run(owner, targetRoute, "printf anything-else", true); err != nil || out != "anything-else" {
		t.Fatalf("full token: %q %v", out, err)
	}
	if err := owner.PushBytes(ctx, otherRoute, filepath.Join(t.TempDir(), "f"), []byte("x"), 0600); err != nil {
		t.Fatalf("full token push: %v", err)
	}
	// The list shows it; the secret is not in it.
	code, body = api(full, http.MethodGet, "/u/scoped-tokens", nil)
	if code != 200 || !strings.Contains(string(body), "printf scoped-ok") || strings.Contains(string(body), issued.Token) {
		t.Fatalf("list: %d %s", code, body)
	}

	// 4. A session already open when the token is revoked: the next request on
	// it is refused. (Hold the connection by running a command that outlives
	// the revoke.)
	started, finished := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "finished")
	slow := "touch " + started + "; sleep 3; touch " + finished
	code, body = api(full, http.MethodPost, "/u/scoped-tokens", map[string]any{"device": "target", "command": slow, "days": 1})
	var slowTok struct{ Token string }
	json.Unmarshal(body, &slowTok)
	if code != 200 {
		t.Fatalf("issue slow: %d %s", code, body)
	}
	slowClient := client.NewWith(cid, pins, wsBase, slowTok.Token, carrier)
	slowClient.SetLabel("scoped e2e")
	done := make(chan error, 1)
	go func() { _, err := run(slowClient, targetRoute, slow, true); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow command never started, so the revoke below proves nothing")
		}
	}
	var slowID struct{ Info relay.ScopedToken }
	json.Unmarshal(body, &slowID)
	if code, b := api(full, http.MethodPost, "/u/scoped-tokens/revoke", map[string]any{"id": slowID.Info.ID}); code != 204 {
		t.Fatalf("revoke slow: %d %s", code, b)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("revoking a token did not end its running session")
	}
	if _, err := run(slowClient, targetRoute, slow, true); err == nil {
		t.Fatal("revoked token ran again")
	}
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatalf("the command kept running after its token was revoked: %v", err)
	}

	// 5. Revoke the main token: the very next request fails.
	if code, b := api(full, http.MethodPost, "/u/scoped-tokens/revoke", map[string]any{"id": issued.Info.ID}); code != 204 {
		t.Fatalf("revoke: %d %s", code, b)
	}
	if out, err := run(scoped, targetRoute, command, true); err == nil || out != "" {
		t.Fatalf("revoked token ran: %q %v", out, err)
	}
	if code, _ := api(full, http.MethodPost, "/u/scoped-tokens/revoke", map[string]any{"id": issued.Info.ID}); code != 404 {
		t.Fatalf("second revoke: %d", code)
	}

	// 6. Expiry: a fresh token whose expiry then passes.
	code, body = api(full, http.MethodPost, "/u/scoped-tokens", map[string]any{"device": "target", "command": command, "days": 1})
	var exp struct {
		Token string
		Info  relay.ScopedToken
	}
	json.Unmarshal(body, &exp)
	expClient := client.NewWith(cid, pins, wsBase, exp.Token, carrier)
	expClient.SetLabel("scoped e2e")
	if out, err := run(expClient, targetRoute, command, true); code != 200 || err != nil || out != "scoped-ok" {
		t.Fatalf("fresh token: %d %q %v", code, out, err)
	}
	if _, err := base.Exec("UPDATE "+schema+".tokens SET expires_at = now() - interval '1 second' WHERE id=$1", exp.Info.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := run(expClient, targetRoute, command, true); err == nil || out != "" {
		t.Fatalf("expired token ran: %q %v", out, err)
	}
}
