package webfetch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/delegation"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"

	"github.com/google/uuid"
)

type liveFixture struct {
	h        *Handler
	pg       *relay.PGStore
	db       *sql.DB
	web      *httptest.Server
	root     string
	agentDir string
	device   delegation.Device
}

func testTicket(letter string) string {
	return strconv.FormatInt(time.Now().Unix(), 10) + "-" + strings.Repeat(letter, 48)
}

func liveWebFetch(t *testing.T) *liveFixture {
	t.Helper()
	dsn := os.Getenv("WANCTL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set WANCTL_TEST_POSTGRES for real delegation/controller/file E2E")
	}
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "webfetch_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec("DROP SCHEMA " + schema + " CASCADE"); base.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pg, err := relay.OpenPG(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ownerToken, err := pg.IssueToken("owner", "test-device", 1)
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(pg)
	r.SetAdmin(pg)
	r.SetACL(pg)
	r.SetAuditor(pg)
	r.SetAdminSecret(strings.Repeat("a", 32))
	relayServer := httptest.NewServer(r.Handler())
	t.Cleanup(relayServer.Close)
	webServer := httptest.NewUnstartedServer(nil)
	webOrigin := "http://" + webServer.Listener.Addr().String()
	h, err := New(Config{Store: pg, Jobs: pg, Seed: bytes.Repeat([]byte{9}, 32), RelayURL: relayServer.URL, PublicOrigin: webOrigin, PortalOrigin: "http://127.0.0.1:9999"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	webServer.Config.Handler = h
	webServer.Start()
	t.Cleanup(webServer.Close)
	agentDir, root := t.TempDir(), t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", agentDir)
	t.Setenv("WANCTL_PORTAL", "http://127.0.0.1:9999")
	identity, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	rules, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []policy.Kind{policy.KindRead, policy.KindWrite} {
		if err := rules.Add(policy.Rule{Kind: kind, Pattern: root, Scope: policy.ScopeDir}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rules.Add(policy.Rule{Kind: policy.KindExec, Pattern: "printf webfetch-ok", Scope: policy.ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{RelayURL: relayServer.URL, Token: ownerToken, Name: "webfetch-e2e", Transport: "http", Mode: policy.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go ag.Run(ctx)
	t.Cleanup(func() { cancel(); ag.Close() })
	device := delegation.Device{Namespace: "owner", ID: ag.DeviceID(), Fingerprint: identity.Fingerprint}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var exists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM devices WHERE device_id=$1 AND fingerprint=$2)", device.ID, device.Fingerprint).Scan(&exists)
		if err == nil && exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device never registered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &liveFixture{h: h, pg: pg, db: db, web: webServer, root: root, agentDir: agentDir, device: device}
}

func fetchDoc(t *testing.T, raw string) (int, map[string]any) {
	t.Helper()
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	resp, err := http.Get(raw + sep + "format=json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode %d: %v", resp.StatusCode, err)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatal("missing no-store")
	}
	return resp.StatusCode, data
}

func (f *liveFixture) approve(t *testing.T, ticket string, pair bool) (string, delegation.Request) {
	t.Helper()
	session := f.web.URL + "/webfetch/s/" + ticket
	status, pending := fetchDoc(t, session)
	if status != 200 || pending["status"] != "pending" {
		t.Fatalf("pending=%v %v", status, pending)
	}
	id, _, identity, err := f.h.credentials(ticket)
	if err != nil {
		t.Fatal(err)
	}
	approval := delegation.Approval{RequestID: id, Namespace: "owner", Devices: []string{f.device.ID}, Minutes: 1, ControllerFingerprint: identity.Fingerprint, DeviceFingerprints: map[string]string{f.device.ID: f.device.Fingerprint}}
	request, err := f.pg.ApproveDelegation(context.Background(), approval)
	if err != nil {
		t.Fatal(err)
	}
	if pair {
		t.Setenv("WANCTL_CONFIG_DIR", f.agentDir)
		known, err := transport.OpenStore("known_clients.json")
		if err != nil {
			t.Fatal(err)
		}
		if err = known.Add(identity.Fingerprint, "explicit-test-pairing"); err != nil {
			t.Fatal(err)
		}
	}
	return session, request
}

func awaitJob(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for job["status"] == "running" || job["status"] == "queued" {
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %v", job)
		}
		time.Sleep(20 * time.Millisecond)
		status, latest := fetchDoc(t, job["result_url"].(string))
		if status != 200 {
			t.Fatalf("poll=%v %v", status, latest)
		}
		job = latest
	}
	return job
}

func TestWebFetchRealFileExecPolicyAndRevocation(t *testing.T) {
	f := liveWebFetch(t)
	ticket := testTicket("b")
	session, request := f.approve(t, ticket, true)
	text := "真实 wanctl 文件\n特殊字符 & + % # ?\nend"
	remote := filepath.Join(f.root, "qwen.txt")
	values := url.Values{"rid": {"write-1"}, "tool": {"write_text"}, "target": {f.device.Target()}, "path": {remote}, "content": {text}}
	call := session + "/call?" + values.Encode()
	status, job := fetchDoc(t, call)
	if status != 200 {
		t.Fatalf("submit=%v %v", status, job)
	}
	job = awaitJob(t, job)
	if job["status"] != "done" {
		t.Fatalf("write failed: %v", job)
	}
	data, err := os.ReadFile(remote)
	if err != nil || string(data) != text {
		t.Fatalf("actual file=%q %v", data, err)
	}
	before, _ := os.Stat(remote)
	_, duplicate := fetchDoc(t, call)
	if duplicate["job_id"] != job["job_id"] || duplicate["duplicate_request"] != true {
		t.Fatalf("duplicate=%v", duplicate)
	}
	after, _ := os.Stat(remote)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("duplicate rewrote file")
	}
	values = url.Values{"rid": {"read-1"}, "tool": {"read_text"}, "target": {f.device.Target()}, "path": {remote}}
	_, read := fetchDoc(t, session+"/call?"+values.Encode())
	read = awaitJob(t, read)
	if read["status"] != "done" || read["result"].(map[string]any)["content"] != text {
		t.Fatalf("read=%v", read)
	}
	values = url.Values{"rid": {"exec-1"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}}
	_, exec := fetchDoc(t, session+"/call?"+values.Encode())
	exec = awaitJob(t, exec)
	if exec["status"] != "done" || exec["result"].(map[string]any)["stdout"] != "webfetch-ok" {
		t.Fatalf("exec=%v", exec)
	}
	values.Set("rid", "denied")
	values.Set("command", "echo not-authorized")
	_, denied := fetchDoc(t, session+"/call?"+values.Encode())
	denied = awaitJob(t, denied)
	if denied["status"] != "failed" || denied["result"].(map[string]any)["ok"] != false {
		t.Fatalf("device policy not preserved: %v", denied)
	}
	values.Set("rid", "outside")
	values.Set("target", "owner/"+uuid.NewString())
	if status, _ := fetchDoc(t, session+"/call?"+values.Encode()); status != 403 {
		t.Fatalf("outside scope=%d", status)
	}
	if err := f.pg.RevokeToken("owner", request.TokenID); err != nil {
		t.Fatal(err)
	}
	if status, _ := fetchDoc(t, read["result_url"].(string)); status != 403 {
		t.Fatalf("revoked result visible: %d", status)
	}
	if status, _ := fetchDoc(t, call); status != 403 {
		t.Fatalf("revoked repeat callable: %d", status)
	}
}

func TestWebFetchPairingNotBypassedAndInputBoundaries(t *testing.T) {
	f := liveWebFetch(t)
	ticket := testTicket("c")
	session := f.web.URL + "/webfetch/s/" + ticket
	if status, _ := fetchDoc(t, session+"/call?rid=a&tool=exec&target=x&command=echo"); status != 404 {
		t.Fatalf("missing grant=%d", status)
	}
	session, _ = f.approve(t, ticket, false)
	values := url.Values{"rid": {"unpaired"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}}
	_, job := fetchDoc(t, session+"/call?"+values.Encode())
	job = awaitJob(t, job)
	if job["status"] != "failed" {
		t.Fatalf("unpaired execution=%v", job)
	}
	result := job["result"].(map[string]any)
	if result["error_code"] != "pairing_required" || result["execution_started"] != false || !strings.Contains(result["instruction"].(string), "NEW rid") {
		t.Fatalf("pairing recovery is ambiguous: %v", result)
	}
	if !strings.Contains(fmt.Sprint(job["result"]), "approve") && !strings.Contains(fmt.Sprint(job["result"]), "paired") {
		t.Fatalf("missing pairing refusal: %v", job)
	}
	if status, _ := fetchDoc(t, session+"/call?rid=1&rid=2"); status != 400 {
		t.Fatalf("duplicate query=%d", status)
	}
	resp, err := http.Head(session + "/call?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("HEAD=%d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, session, nil)
	req.Header.Set("Origin", "https://untrusted.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("Origin=%d", resp.StatusCode)
	}
	oldTicket := strconv.FormatInt(time.Now().Add(-71*time.Minute).Unix(), 10) + "-" + strings.Repeat("d", 48)
	if status, _ := fetchDoc(t, f.web.URL+"/webfetch/s/"+oldTicket); status != 403 {
		t.Fatalf("expired browser ticket resurrected=%d", status)
	}
	var count int
	id, _, _, _ := f.h.credentials(oldTicket)
	if err := f.db.QueryRow("SELECT count(*) FROM delegation_requests WHERE id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired ticket created record: %d %v", count, err)
	}
}
