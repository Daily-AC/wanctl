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
	// A command that outlives a single poll, so a long job can be observed
	// running while another grant's short job finishes.
	for _, command := range []string{"printf webfetch-ok", "sleep 2"} {
		if err := rules.Add(policy.Rule{Kind: policy.KindExec, Pattern: command, Scope: policy.ScopeGlobal}); err != nil {
			t.Fatal(err)
		}
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
		f.pair(t, ticket)
	}
	return session, request
}

// pair is the second human checkpoint: the owner approving this controller on
// the device itself, which approving device access deliberately does not do.
func (f *liveFixture) pair(t *testing.T, ticket string) {
	t.Helper()
	_, _, identity, err := f.h.credentials(ticket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_CONFIG_DIR", f.agentDir)
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = known.Add(identity.Fingerprint, "explicit-test-pairing"); err != nil {
		t.Fatal(err)
	}
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

// The whole path one prompt has to survive: public discovery, a request the
// client creates itself, the owner approving device access, the first exec
// bouncing off pairing, and the retry that finally runs on the device.
func TestWebFetchOnePromptFlowThroughBothHumanCheckpoints(t *testing.T) {
	f := liveWebFetch(t)
	_, entry := fetchDoc(t, f.web.URL+"/webfetch/v1")
	template, ok := entry["start_url_template"].(string)
	if !ok || !strings.HasSuffix(template, "/webfetch/new/{client_nonce}") {
		t.Fatalf("discovery lost its start template: %v", entry)
	}
	_, pending := fetchDoc(t, strings.ReplaceAll(template, "{client_nonce}", strings.Repeat("9", 48)))
	checkpoint, _ := pending["human_checkpoint"].(map[string]any)
	if pending["status"] != "pending" || checkpoint["id"] != "device_access" || !strings.Contains(fmt.Sprint(checkpoint["name"]), "Step 1 of 2") {
		t.Fatalf("first checkpoint is unnamed: %v", pending)
	}
	if approval, _ := pending["approval_url"].(string); !strings.Contains(approval, "/webfetch/approve?request=") {
		t.Fatalf("no link to give the human: %v", pending)
	}
	parsed, err := url.Parse(pending["status_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	ticket := strings.TrimPrefix(parsed.Path, "/webfetch/s/")

	// Checkpoint 1: the owner approves device access, but not pairing.
	session, _ := f.approve(t, ticket, false)
	_, manifest := fetchDoc(t, session)
	if manifest["status"] != "approved" || manifest["call_endpoint"] == nil {
		t.Fatalf("manifest after approval=%v", manifest)
	}
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
	// Checkpoint 2 has to arrive as a link the model can hand over, named as
	// the second of two steps, or the conversation stalls here.
	pairingURL, _ := result["pairing_url"].(string)
	if !strings.HasPrefix(pairingURL, "http://127.0.0.1:9999") {
		t.Fatalf("no pairing link to give the human: %v", result)
	}
	second, _ := result["human_checkpoint"].(map[string]any)
	if second["id"] != "pairing" || !strings.Contains(fmt.Sprint(second["name"]), "Step 2 of 2") || fmt.Sprint(second["summary_zh"]) == "" {
		t.Fatalf("second checkpoint is unnamed: %v", result)
	}
	if !strings.Contains(fmt.Sprint(job["result"]), "approve") && !strings.Contains(fmt.Sprint(job["result"]), "paired") {
		t.Fatalf("missing pairing refusal: %v", job)
	}

	// Checkpoint 2: the owner pairs, the model retries under a new rid.
	f.pair(t, ticket)
	values.Set("rid", "paired-retry")
	_, retry := fetchDoc(t, session+"/call?"+values.Encode())
	retry = awaitJob(t, retry)
	if retry["status"] != "done" {
		t.Fatalf("retry after pairing=%v", retry)
	}
	done := retry["result"].(map[string]any)
	if done["stdout"] != "webfetch-ok" || done["exit_code"] != float64(0) {
		t.Fatalf("retry produced no real result: %v", done)
	}
	// The failed attempt stays failed; only a new rid ever runs again.
	_, stale := fetchDoc(t, job["result_url"].(string))
	if stale["status"] != "failed" {
		t.Fatalf("the refused job was rewritten: %v", stale)
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

// A build or a render outlives the old sixty-second ceiling. It has to finish
// with a real exit code, and it must not park the adapter while it runs.
func TestWebFetchLongExecFinishesAndDoesNotBlockAnotherGrant(t *testing.T) {
	f := liveWebFetch(t)
	slow, _ := f.approve(t, testTicket("f"), true)
	quick, _ := f.approve(t, testTicket("a"), true)

	long := url.Values{"rid": {"long-1"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"sleep 2"}, "timeout_seconds": {"900"}}
	status, started := fetchDoc(t, slow+"/call?"+long.Encode())
	if status != 200 || started["status"] == "failed" {
		t.Fatalf("a 900-second exec was refused: %d %v", status, started)
	}
	if started["status"] == "running" && started["poll_after_seconds"] == nil {
		t.Fatalf("a running job does not say when to look again: %v", started)
	}

	// While that one runs, an unrelated grant's operation must still complete.
	short := url.Values{"rid": {"short-1"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}}
	_, other := fetchDoc(t, quick+"/call?"+short.Encode())
	other = awaitJob(t, other)
	if other["status"] != "done" || other["result"].(map[string]any)["stdout"] != "webfetch-ok" {
		t.Fatalf("a long job on one grant blocked another: %v", other)
	}
	_, midway := fetchDoc(t, started["result_url"].(string))
	if midway["status"] == "unknown" {
		t.Fatalf("a healthy long job was reported unknown: %v", midway)
	}

	finished := awaitJob(t, started)
	if finished["status"] != "done" {
		t.Fatalf("long exec did not finish: %v", finished)
	}
	if code := finished["result"].(map[string]any)["exit_code"]; code != float64(0) {
		t.Fatalf("long exec lost its exit code: %v", finished["result"])
	}
}

// The ceiling is per tool: a file transfer of at most 32 KiB is stuck, not slow.
func TestWebFetchTimeoutCeilingIsPerTool(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("8"), true)
	file := url.Values{"rid": {"slow-read"}, "tool": {"read_text"}, "target": {f.device.Target()}, "path": {filepath.Join(f.root, "absent.txt")}, "timeout_seconds": {"900"}}
	status, refused := fetchDoc(t, session+"/call?"+file.Encode())
	if status != 400 || refused["error_code"] != "invalid_parameters" {
		t.Fatalf("read_text accepted an exec-sized timeout: %d %v", status, refused)
	}
	if !strings.Contains(refused["error"].(string), "60") {
		t.Fatalf("the refusal does not state the file ceiling: %v", refused)
	}
}

// A refusal must not be written down. Spending a ledger slot on an operation
// that never ran would let a busy adapter burn the caller's 64-job allowance.
func TestWebFetchBusyAdapterRefusesWithoutTouchingTheLedger(t *testing.T) {
	f := liveWebFetch(t)
	previous := maxOperationsPerGrant
	maxOperationsPerGrant = 1
	t.Cleanup(func() { maxOperationsPerGrant = previous })

	session, _ := f.approve(t, testTicket("7"), true)
	long := url.Values{"rid": {"holds-the-slot"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"sleep 2"}, "timeout_seconds": {"900"}}
	_, held := fetchDoc(t, session+"/call?"+long.Encode())
	if held["status"] != "running" && held["status"] != "queued" {
		t.Fatalf("the first job did not take the slot: %v", held)
	}
	busy := url.Values{"rid": {"refused"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}}
	status, refused := fetchDoc(t, session+"/call?"+busy.Encode())
	if status != 429 || refused["error_code"] != "adapter_busy" || refused["execution_started"] != false {
		t.Fatalf("a full adapter did not refuse cleanly: %d %v", status, refused)
	}
	if refused["job_id"] != nil {
		t.Fatalf("a refusal created a job: %v", refused)
	}
	var jobs int
	f.db.QueryRow("SELECT count(*) FROM delegation_jobs").Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("the refused call consumed a ledger slot: %d jobs", jobs)
	}
	// Once the long job ends its slot comes back and the same rid works.
	if finished := awaitJob(t, held); finished["status"] != "done" {
		t.Fatalf("the long job did not finish: %v", finished)
	}
	_, retried := fetchDoc(t, session+"/call?"+busy.Encode())
	if retried = awaitJob(t, retried); retried["status"] != "done" {
		t.Fatalf("the slot was not released after a terminal state: %v", retried)
	}
}

// A lost response is recovered by fetching the identical URL. That must keep
// working across a deploy that changes a default the caller never sent.
func TestWebFetchIdenticalURLReplaysTheSameJobAcrossDefaultChanges(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("6"), true)
	call := session + "/call?" + url.Values{"rid": {"replay-1"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}}.Encode()
	_, first := fetchDoc(t, call)
	first = awaitJob(t, first)
	if first["status"] != "done" {
		t.Fatalf("first attempt=%v", first)
	}
	previous := DefaultExecSeconds
	DefaultExecSeconds = 77
	t.Cleanup(func() { DefaultExecSeconds = previous })
	_, replayed := fetchDoc(t, call)
	if replayed["job_id"] != first["job_id"] || replayed["duplicate_request"] != true {
		t.Fatalf("a changed default turned a replay into a new identity: %v", replayed)
	}
	// An explicitly supplied timeout is part of the operation and still clashes.
	explicit := session + "/call?" + url.Values{"rid": {"replay-2"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}, "timeout_seconds": {"120"}}.Encode()
	if status, _ := fetchDoc(t, explicit); status != 200 {
		t.Fatalf("explicit timeout rejected: %d", status)
	}
	changed := session + "/call?" + url.Values{"rid": {"replay-2"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"printf webfetch-ok"}, "timeout_seconds": {"240"}}.Encode()
	if status, _ := fetchDoc(t, changed); status != 409 {
		t.Fatalf("a changed explicit timeout did not conflict: %d", status)
	}
}

// deadline_at is a promise about when a result can still arrive. A grant that
// ends in a minute cannot honour a fifteen-minute timeout.
func TestWebFetchDeadlineIsClampedToTheGrant(t *testing.T) {
	f := liveWebFetch(t)
	session, request := f.approve(t, testTicket("5"), true)
	if request.ExpiresAt == nil {
		t.Fatal("approved grant carries no expiry")
	}
	long := url.Values{"rid": {"clamped"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"sleep 2"}, "timeout_seconds": {"900"}}
	_, job := fetchDoc(t, session+"/call?"+long.Encode())
	raw, _ := job["deadline_at"].(string)
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("deadline_at=%q %v", raw, err)
	}
	// The fixture approves one minute, so 900 seconds must not survive.
	if gap := deadline.Sub(*request.ExpiresAt); gap > time.Second || gap < -time.Second {
		t.Fatalf("deadline_at %s is not the grant expiry %s", deadline, request.ExpiresAt)
	}
	awaitJob(t, job)
}

// unknown is recorded well before any deadline when the adapter stops. The text
// the model reads must not tell it the deadline elapsed.
func TestWebFetchInterruptedJobDoesNotBlameTheDeadline(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("4"), true)
	long := url.Values{"rid": {"interrupted"}, "tool": {"exec"}, "target": {f.device.Target()}, "command": {"sleep 2"}, "timeout_seconds": {"900"}}
	_, job := fetchDoc(t, session+"/call?"+long.Encode())
	if job["status"] != "running" && job["status"] != "queued" {
		t.Fatalf("job did not start: %v", job)
	}
	f.h.Close() // cancels the operation mid-flight, minutes before its deadline
	_, stopped := fetchDoc(t, job["result_url"].(string))
	if stopped["status"] != "unknown" {
		t.Fatalf("an interrupted job did not read unknown: %v", stopped)
	}
	text := fmt.Sprint(stopped["result"]) + fmt.Sprint(stopped["instruction"])
	for _, forbidden := range []string{"deadline passed", "timed out", "expired"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("the interruption was reported as %q: %v", forbidden, stopped)
		}
	}
	if !strings.Contains(text, "check the device") {
		t.Fatalf("the unknown result does not send the human to the device: %v", stopped)
	}
}
