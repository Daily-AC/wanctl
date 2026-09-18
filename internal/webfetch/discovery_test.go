package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"wanctl/internal/client"
	"wanctl/internal/delegation"
	"wanctl/internal/protocol"
)

// The discovery document is public and credential-free, so it can be read
// without a database. These checks are on stable keys, never on wording.
type emptyStore struct{}

func (emptyStore) CreateDelegation(context.Context, delegation.NewRequest) (delegation.Request, error) {
	return delegation.Request{}, delegation.ErrNotFound
}
func (emptyStore) GetDelegation(context.Context, string) (delegation.Request, error) {
	return delegation.Request{}, delegation.ErrNotFound
}
func (emptyStore) GetDelegationByTicket(context.Context, string, string) (delegation.Request, error) {
	return delegation.Request{}, delegation.ErrNotFound
}
func (emptyStore) ApproveDelegation(context.Context, delegation.Approval) (delegation.Request, error) {
	return delegation.Request{}, delegation.ErrNotFound
}
func (emptyStore) RejectDelegation(context.Context, string, string) error {
	return delegation.ErrNotFound
}
func (emptyStore) ResolveAccess(string) (delegation.Access, bool) { return delegation.Access{}, false }
func (emptyStore) FindJob(context.Context, string, string) (delegation.Job, error) {
	return delegation.Job{}, delegation.ErrNotFound
}
func (emptyStore) BeginJob(context.Context, string, string, string, json.RawMessage) (delegation.Job, bool, error) {
	return delegation.Job{}, false, delegation.ErrNotFound
}
func (emptyStore) FinishJob(context.Context, string, string, string, json.RawMessage) error {
	return delegation.ErrNotFound
}
func (emptyStore) GetJob(context.Context, string, string) (delegation.Job, error) {
	return delegation.Job{}, delegation.ErrNotFound
}

func staticHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(Config{
		Store: emptyStore{}, Jobs: emptyStore{}, Seed: bytes.Repeat([]byte{7}, 32),
		RelayURL: "https://relay.example", PublicOrigin: "https://relay.example", PortalOrigin: "https://portal.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func getDiscovery(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/v1"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("discovery = %d", w.Code)
	}
	return w
}

func TestDiscoveryNamesBothHumanCheckpointsAndKeepsSecurityRules(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(getDiscovery(t, staticHandler(t), "?format=json").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	checkpoints, ok := doc["human_checkpoints"].([]any)
	if !ok || len(checkpoints) != 2 {
		t.Fatalf("human_checkpoints = %v", doc["human_checkpoints"])
	}
	want := []struct{ id, name, url string }{
		{"device_access", "Step 1 of 2", "approval_url"},
		{"pairing", "Step 2 of 2", "pairing_url"},
	}
	for i, expect := range want {
		got := checkpoints[i].(map[string]any)
		if got["id"] != expect.id || !strings.Contains(got["name"].(string), expect.name) || got["url_field"] != expect.url {
			t.Fatalf("checkpoint %d = %v", i+1, got)
		}
		if got["step"] != float64(i+1) || got["of"] != float64(2) {
			t.Fatalf("checkpoint %d is not numbered 1..2: %v", i+1, got)
		}
		// Both languages ship, because the human may read either one.
		for _, key := range []string{"summary", "summary_zh", "name_zh"} {
			if text, _ := got[key].(string); strings.TrimSpace(text) == "" {
				t.Fatalf("checkpoint %d missing %s", i+1, key)
			}
		}
	}
	security, ok := doc["security"].(map[string]any)
	if !ok {
		t.Fatalf("security = %v", doc["security"])
	}
	for _, rule := range securityRuleKeys {
		if text, _ := security[rule].(string); strings.TrimSpace(text) == "" {
			t.Fatalf("security rule %q was dropped", rule)
		}
	}
	steps, ok := doc["procedure"].([]any)
	if !ok || len(steps) < 6 {
		t.Fatalf("procedure = %v", doc["procedure"])
	}
	// A public entry that hands out a session URL can be replayed from a cache.
	if doc["start_url"] != nil || strings.Contains(string(getDiscovery(t, staticHandler(t), "?format=json").Body.Bytes()), "/webfetch/s/") {
		t.Fatal("discovery exposed a session URL")
	}
}

var tagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

// countWords reports Latin words and CJK characters separately: "word" has no
// meaning in Chinese, so each han character is counted as one.
func countWords(rendered string) (latin, han int) {
	text := html.UnescapeString(tagPattern.ReplaceAllString(rendered, " "))
	for _, field := range strings.Fields(text) {
		hasHan := false
		for _, r := range field {
			if unicode.Is(unicode.Han, r) {
				han++
				hasHan = true
			}
		}
		if !hasHan {
			latin++
		}
	}
	return latin, han
}

func TestDiscoveryPageStaysShortEnoughToRead(t *testing.T) {
	latin, han := countWords(getDiscovery(t, staticHandler(t), "").Body.String())
	t.Logf("rendered /webfetch/v1: %d latin words, %d han characters", latin, han)
	if latin >= 2500 || han >= 2500 {
		t.Fatalf("discovery page grew past the budget: %d latin words, %d han characters", latin, han)
	}
}

// Every rule the protocol promises, by key. A client that reads one document
// must not have to find the rest somewhere else.
var securityRuleKeys = []string{
	"client_nonce_per_conversation", "no_url_reuse", "get_only", "no_store",
	"rid_unique_per_operation", "retry_a_lost_response_on_the_same_url",
	"new_rid_only_when_nothing_ran", "pairing_required_means_not_started",
	"unknown_means_check_the_device", "human_approves",
}

// A client that only ever reads the approved status document still has to learn
// that a lost response is recovered on the same rid, and that a new rid is for
// the cases where nothing ran. JSON callers never see the HTML wrapper that
// used to be the only place saying so.
func TestApprovedManifestCarriesTheRetryRules(t *testing.T) {
	h := staticHandler(t)
	doc := h.manifest("1700000000-"+strings.Repeat("a", 48), delegation.Access{
		Namespace: "alice", GrantID: "d_test", Delegated: true,
		ExpiresAt: time.Now().Add(30 * time.Minute),
		Devices:   []delegation.Device{{Namespace: "alice", ID: "dev-1", Fingerprint: "SHA256:x"}},
	})
	security, ok := doc["security"].(map[string]any)
	if !ok {
		t.Fatalf("the approved manifest carries no security block: %v", doc["security"])
	}
	for _, rule := range securityRuleKeys {
		if text, _ := security[rule].(string); strings.TrimSpace(text) == "" {
			t.Fatalf("the approved manifest dropped security rule %q", rule)
		}
	}
	same := security["retry_a_lost_response_on_the_same_url"].(string)
	if !strings.Contains(same, "SAME rid") || !strings.Contains(same, "IDENTICAL URL") {
		t.Fatalf("the same-rid retry rule is not stated plainly: %q", same)
	}
	fresh := security["new_rid_only_when_nothing_ran"].(string)
	if !strings.Contains(fresh, "pairing_required") || !strings.Contains(fresh, "adapter_busy") {
		t.Fatalf("the new-rid rule does not name the cases where nothing ran: %q", fresh)
	}
	// The instruction a model actually follows has to say it too.
	if !strings.Contains(doc["instruction"].(string), "identical URL") {
		t.Fatalf("the calling instruction omits the lost-response retry: %v", doc["instruction"])
	}
}

// The canonical payload is the replay identity. Moving a default must not move
// it, or an identical URL becomes a 409 across a deploy.
func TestOmittedTimeoutIsNotPartOfTheReplayIdentity(t *testing.T) {
	base := url.Values{"rid": {"r1"}, "tool": {"exec"}, "target": {"alice/dev-1"}, "command": {"make"}}
	omitted, err := parseOperation(base)
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Timeout != nil {
		t.Fatalf("an omitted timeout was materialized: %v", *omitted.Timeout)
	}
	if omitted.timeout() != DefaultExecSeconds {
		t.Fatalf("dispatch default = %d", omitted.timeout())
	}
	payload, _ := json.Marshal(omitted)
	if strings.Contains(string(payload), "timeout_seconds") {
		t.Fatalf("the default leaked into the canonical payload: %s", payload)
	}
	explicit := base
	explicit.Set("timeout_seconds", "900")
	supplied, err := parseOperation(explicit)
	if err != nil {
		t.Fatal(err)
	}
	given, _ := json.Marshal(supplied)
	if !strings.Contains(string(given), `"timeout_seconds":900`) {
		t.Fatalf("an explicit timeout left the identity: %s", given)
	}
	// Changing a supplied value is a different operation and must still clash.
	explicit.Set("timeout_seconds", "600")
	changed, _ := parseOperation(explicit)
	other, _ := json.Marshal(changed)
	if string(other) == string(given) {
		t.Fatal("two different explicit timeouts hash alike")
	}
}

// A long job holds a slot, so the caps have to hold and every terminal path has
// to give the slot back.
func TestOperationSlotsAreBoundedAndAlwaysReleased(t *testing.T) {
	h := staticHandler(t)
	for i := 0; i < maxOperationsPerGrant; i++ {
		if !h.reserve("d_alice") {
			t.Fatalf("refused slot %d inside the per-grant limit", i)
		}
	}
	if h.reserve("d_alice") {
		t.Fatal("one grant went past its own limit")
	}
	if !h.reserve("d_bob") {
		t.Fatal("a busy grant blocked an unrelated one")
	}
	h.release("d_alice")
	if !h.reserve("d_alice") {
		t.Fatal("releasing a slot did not free the grant's budget")
	}
	for i := 0; i < maxOperationsPerGrant; i++ {
		h.release("d_alice")
	}
	h.release("d_bob")
	h.mu.Lock()
	total, grants := h.total, len(h.running)
	h.mu.Unlock()
	if total != 0 || grants != 0 {
		t.Fatalf("released slots leaked: total=%d grants=%d", total, grants)
	}
}

// The adapter records unknown for four different causes. The sentence a model
// reads must not claim one of them.
func TestUnknownDoesNotBlameTheDeadline(t *testing.T) {
	for _, forbidden := range []string{"deadline", "timed out", "expired"} {
		if strings.Contains(strings.ToLower(unknownInstruction), forbidden) {
			t.Fatalf("the unknown instruction names a cause it cannot know: %q", forbidden)
		}
	}
	for _, required := range []string{"may have run", "check the device", "Do not repeat it automatically"} {
		if !strings.Contains(unknownInstruction, required) {
			t.Fatalf("the unknown instruction dropped %q", required)
		}
	}
}

func TestLongJobIsNotReportedUnknownBeforeItsOwnDeadline(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	farOff := created.Add(48 * time.Hour)
	for _, tc := range []struct {
		name    string
		age     time.Duration
		timeout int
		expiry  time.Time
		want    string
	}{
		{"two minutes into a fifteen minute job", 120 * time.Second, 900, farOff, "running"},
		{"past a thirty second job", 30*time.Second + staleGrace + time.Second, 30, farOff, "unknown"},
		{"past a fifteen minute job", 900*time.Second + staleGrace + time.Second, 900, farOff, "unknown"},
		// A long timeout inside a short grant cannot outlive the grant.
		{"a fifteen minute job in a one minute grant", 60*time.Second + staleGrace + time.Second, 900, created.Add(time.Minute), "unknown"},
		{"a finished job is never re-judged", time.Hour, 30, farOff, "done"},
	} {
		state := "running"
		if tc.want == "done" {
			state = "done"
		}
		deadline := effectiveDeadline(created, tc.timeout, tc.expiry)
		if got := jobState(state, created.Add(tc.age), deadline); got != tc.want {
			t.Fatalf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// approvedStore is the smallest store that gets a request as far as the call
// path, so the slot accounting around the ledger write can be exercised without
// a database. Its BeginJob panics: that is the failure a plain error return
// would not catch.
type approvedStore struct {
	emptyStore
	grant       string
	fingerprint string
	begins      int
}

func (s *approvedStore) GetDelegationByTicket(context.Context, string, string) (delegation.Request, error) {
	return delegation.Request{ID: s.grant, Status: "approved", ControllerFingerprint: s.fingerprint}, nil
}

func (s *approvedStore) ResolveAccess(string) (delegation.Access, bool) {
	return delegation.Access{
		Namespace: "alice", GrantID: s.grant, Delegated: true, ExpiresAt: time.Now().Add(time.Hour),
		ControllerFingerprint: s.fingerprint,
		Devices:               []delegation.Device{{Namespace: "alice", ID: "dev-1", Fingerprint: "SHA256:x"}},
	}, true
}

func (s *approvedStore) BeginJob(context.Context, string, string, string, json.RawMessage) (delegation.Job, bool, error) {
	s.begins++
	panic("ledger exploded")
}

func TestASlotSurvivesALedgerThatPanics(t *testing.T) {
	store := &approvedStore{}
	h, err := New(Config{
		Store: store, Jobs: store, Seed: bytes.Repeat([]byte{7}, 32),
		RelayURL: "https://relay.example", PublicOrigin: "https://relay.example", PortalOrigin: "https://portal.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	ticket := strconv.FormatInt(time.Now().Unix(), 10) + "-" + strings.Repeat("a", 48)
	grant, _, identity, err := h.credentials(ticket)
	if err != nil {
		t.Fatal(err)
	}
	store.grant, store.fingerprint = grant, identity.Fingerprint

	call := "/webfetch/s/" + ticket + "/call?" + url.Values{
		"rid": {"boom"}, "tool": {"exec"}, "target": {"alice/dev-1"}, "command": {"true"},
	}.Encode()
	func() {
		// net/http recovers a handler panic per connection; a direct call does
		// not, so the test stands in for the server.
		defer func() { recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, call, nil))
	}()
	if store.begins != 1 {
		t.Fatalf("the request never reached the ledger: %d writes", store.begins)
	}
	h.mu.Lock()
	total, grants := h.total, len(h.running)
	h.mu.Unlock()
	if total != 0 || grants != 0 {
		t.Fatalf("a panicking ledger leaked a slot: total=%d grants=%d", total, grants)
	}
	if !h.reserve(grant) {
		t.Fatal("the grant cannot start work again")
	}
	h.release(grant)
}

// The file tools are the ones a model has to be taught, so their contract is
// what the manifest has to carry: a schema it can fill and a description that
// says when to reach for them and what each refusal means.
func TestManifestPublishesBothFileToolContracts(t *testing.T) {
	h := staticHandler(t)
	doc := h.manifest("1700000000-"+strings.Repeat("a", 48), delegation.Access{
		Namespace: "alice", GrantID: "d_test", Delegated: true,
		ExpiresAt: time.Now().Add(30 * time.Minute),
		Devices:   []delegation.Device{{Namespace: "alice", ID: "dev-1", Fingerprint: "SHA256:x"}},
	})
	tools := map[string]map[string]any{}
	for _, tool := range doc["tools"].([]map[string]any) {
		tools[tool["name"].(string)] = tool
	}
	for _, name := range []string{"exec", "read_text", "edit_text", "write_text"} {
		if tools[name] == nil {
			t.Fatalf("the manifest does not publish %s", name)
		}
	}

	schema := func(name string) (map[string]any, []string) {
		s := tools[name]["input_schema"].(map[string]any)
		var required []string
		for _, value := range s["required"].([]string) {
			required = append(required, value)
		}
		return s["properties"].(map[string]any), required
	}

	// read_text: paging is optional, so it must not be required, and both
	// parameters have to advertise the default the caller gets without them.
	props, required := schema("read_text")
	offset, limit := props["offset"].(map[string]any), props["limit"].(map[string]any)
	if offset["default"] != 1 || limit["default"] != protocol.DefaultReadLines {
		t.Fatalf("read_text does not publish its defaults: %v %v", offset, limit)
	}
	if offset["minimum"] != 1 || limit["maximum"] != maxReadLines {
		t.Fatalf("read_text does not publish its bounds: %v %v", offset, limit)
	}
	if strings.Join(required, ",") != "rid,target,path" {
		t.Fatalf("read_text required = %v", required)
	}

	// edit_text: old and new are required, the other two are not, and the
	// template has to carry the two the caller cannot guess.
	props, required = schema("edit_text")
	for _, key := range []string{"old", "new", "all", "expected_sha256"} {
		if props[key] == nil {
			t.Fatalf("edit_text schema omits %s", key)
		}
	}
	if strings.Join(required, ",") != "rid,target,path,old,new" {
		t.Fatalf("edit_text required = %v", required)
	}
	if props["expected_sha256"].(map[string]any)["pattern"] != sha256Pattern.String() {
		t.Fatalf("edit_text does not say what a sha256 looks like: %v", props["expected_sha256"])
	}
	template := tools["edit_text"]["call_url_template"].(string)
	for _, placeholder := range []string{"tool=edit_text", "{path}", "{old}", "{new}"} {
		if !strings.Contains(template, placeholder) {
			t.Fatalf("edit_text template lacks %s: %s", placeholder, template)
		}
	}
	if strings.Contains(tools["read_text"]["call_url_template"].(string), "{offset}") {
		t.Fatal("read_text puts an optional parameter in its template")
	}

	// The descriptions are operating instructions, not feature lists.
	for tool, phrases := range map[string][]string{
		"read_text":  {"offset=last_line+1", "long_line", "expected_sha256", "not exec"},
		"edit_text":  {"expected_sha256", "NEW rid", "occurs N times", "all=true", strconv.Itoa(MaxURLBytes)},
		"write_text": {"edit_text"},
		"exec":       {"read_text and edit_text"},
	} {
		description := tools[tool]["description"].(string)
		for _, phrase := range phrases {
			if !strings.Contains(description, phrase) {
				t.Fatalf("%s description does not tell a model about %q", tool, phrase)
			}
		}
	}

	limits := doc["limits"].(map[string]any)
	if limits["read_lines_default"] != protocol.DefaultReadLines || limits["output_bytes"] != MaxOutputBytes {
		t.Fatalf("limits = %v", limits)
	}
	// A refused file operation is the third case where nothing ran, and a model
	// that does not learn that has no way to send a corrected edit.
	fresh := doc["security"].(map[string]any)["new_rid_only_when_nothing_ran"].(string)
	for _, code := range []string{"pairing_required", "adapter_busy", "file_refused"} {
		if !strings.Contains(fresh, code) {
			t.Fatalf("the new-rid rule does not name %s: %q", code, fresh)
		}
	}

	// Two more tools must not cost the reader the documents themselves.
	latin, han := countWords(getDiscovery(t, h, "").Body.String())
	t.Logf("rendered /webfetch/v1: %d latin words, %d han characters", latin, han)
	if latin >= 2500 || han >= 2500 {
		t.Fatalf("discovery page grew past the budget: %d latin words, %d han characters", latin, han)
	}
	latin, han = countWords(approvedPage(t).Body.String())
	t.Logf("rendered approved manifest: %d latin words, %d han characters", latin, han)
	if latin >= 3000 || han >= 2500 {
		t.Fatalf("the approved manifest grew past the budget: %d latin words, %d han characters", latin, han)
	}
}

// approvedPage renders the document a client reads after approval, through the
// same handler and template a web chat fetches.
func approvedPage(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	store := &approvedStore{}
	h, err := New(Config{
		Store: store, Jobs: store, Seed: bytes.Repeat([]byte{7}, 32),
		RelayURL: "https://relay.example", PublicOrigin: "https://relay.example", PortalOrigin: "https://portal.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	ticket := strconv.FormatInt(time.Now().Unix(), 10) + "-" + strings.Repeat("c", 48)
	grant, _, identity, err := h.credentials(ticket)
	if err != nil {
		t.Fatal(err)
	}
	store.grant, store.fingerprint = grant, identity.Fingerprint
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/s/"+ticket, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("approved page = %d", w.Code)
	}
	return w
}

// The canonical payload is the replay identity. A parameter the caller never
// sent must stay out of it, or an identical URL becomes a 409 the day a default
// moves — the same rule that already governs timeout_seconds.
func TestOmittedFileParametersAreNotPartOfTheReplayIdentity(t *testing.T) {
	canonical := func(t *testing.T, q url.Values) string {
		t.Helper()
		op, err := parseOperation(q)
		if err != nil {
			t.Fatalf("%v: %v", q, err)
		}
		payload, _ := json.Marshal(op)
		return string(payload)
	}
	read := url.Values{"rid": {"r1"}, "tool": {"read_text"}, "target": {"alice/dev-1"}, "path": {"/tmp/a.txt"}}
	bare := canonical(t, read)
	for _, key := range []string{"offset", "limit"} {
		if strings.Contains(bare, key) {
			t.Fatalf("an omitted %s entered the identity: %s", key, bare)
		}
	}
	op, _ := parseOperation(read)
	if op.Offset != nil || op.Limit != nil {
		t.Fatalf("an omitted line range was materialized: %v %v", op.Offset, op.Limit)
	}

	paged := url.Values{}
	for key, value := range read {
		paged[key] = value
	}
	paged.Set("offset", "41")
	paged.Set("limit", "10")
	windowed := canonical(t, paged)
	if !strings.Contains(windowed, `"offset":41`) || !strings.Contains(windowed, `"limit":10`) {
		t.Fatalf("an explicit window left the identity: %s", windowed)
	}
	paged.Set("offset", "51")
	if canonical(t, paged) == windowed {
		t.Fatal("two different windows hash alike")
	}

	edit := url.Values{"rid": {"e1"}, "tool": {"edit_text"}, "target": {"alice/dev-1"}, "path": {"/tmp/a.txt"}, "old": {"x"}, "new": {"y"}}
	plain := canonical(t, edit)
	for _, key := range []string{"all", "expected_sha256"} {
		if strings.Contains(plain, key) {
			t.Fatalf("an omitted %s entered the identity: %s", key, plain)
		}
	}
	// An explicit all=false is a different URL, so it has to be a different
	// identity: a caller that spells the default out must still be able to tell
	// a replay of its own call from a fresh operation.
	edit.Set("all", "false")
	if spelled := canonical(t, edit); spelled == plain || !strings.Contains(spelled, `"all":false`) {
		t.Fatalf("an explicit all=false vanished into the default: %s", spelled)
	}
	edit.Set("all", "true")
	if every := canonical(t, edit); every == plain || !strings.Contains(every, `"all":true`) {
		t.Fatalf("all=true did not enter the identity: %s", every)
	}
	edit.Del("all")
	edit.Set("expected_sha256", strings.Repeat("a", 64))
	if guarded := canonical(t, edit); guarded == plain || !strings.Contains(guarded, `"expected_sha256"`) {
		t.Fatalf("expected_sha256 did not enter the identity: %s", guarded)
	}
	// An empty replacement is a real operation, not an omission, so it must be
	// sent explicitly and must keep one canonical form.
	empty := url.Values{"rid": {"e2"}, "tool": {"edit_text"}, "target": {"alice/dev-1"}, "path": {"/tmp/a.txt"}, "old": {"x"}, "new": {""}}
	deleting := canonical(t, empty)
	if strings.Contains(deleting, `"new"`) {
		t.Fatalf("an empty replacement was written into the identity: %s", deleting)
	}
	empty.Del("new")
	if _, err := parseOperation(empty); err == nil {
		t.Fatal("an omitted replacement was accepted as an empty one")
	}
}

// internal/client raises one error for two different things: a device that has
// never heard of file_edit, and a session that ended after the request went out.
// The second can mean the edit was committed and only its answer was lost, so an
// edit must not be told that nothing ran.
func TestAnEditWhoseAnswerNeverArrivedStaysUnknown(t *testing.T) {
	h := staticHandler(t)
	lost := &client.UnsupportedError{Target: "alice/dev-1", Kind: protocol.KindFileEdit, Version: "0.6.1"}
	result := map[string]any{}
	if state := h.recordFailure("edit_text", lost, result); state != "unknown" {
		t.Fatalf("a lost edit answer = %q, want unknown", state)
	}
	if _, present := result["execution_started"]; present {
		t.Fatalf("a lost edit claimed something about execution: %v", result)
	}
	if result["error_code"] != nil {
		t.Fatalf("a lost edit was given a cause it cannot know: %v", result["error_code"])
	}
	if result["instruction"] != unknownInstruction {
		t.Fatalf("a lost edit does not send the human to the device: %v", result["instruction"])
	}
	text := strings.ToLower(fmt.Sprint(result["error"]) + " " + fmt.Sprint(result["instruction"]))
	for _, forbidden := range []string{"write_text", "wanctl update", "nothing ran", "does not support"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("a lost edit told the caller %q: %v", forbidden, result)
		}
	}

	// A read wrote nothing whichever cause it was, so it keeps the actionable
	// answer: the device's agent is too old and the human can fix that.
	result = map[string]any{}
	if state := h.recordFailure("read_text", &client.UnsupportedError{Kind: protocol.KindFileRead}, result); state != "failed" {
		t.Fatalf("an unsupported read = %q, want failed", state)
	}
	if result["error_code"] != "device_agent_too_old" || result["execution_started"] != false {
		t.Fatalf("an unsupported read lost its way out: %v", result)
	}
}

// The outer response cap cuts a window the device already filled with whole
// lines. The cut has to land on a line boundary and name the line after it, or
// a caller paging through a file loses one or reads one twice.
func TestFitReadCutsManyLinesAtTheResponseCapWithoutLosingOne(t *testing.T) {
	var file strings.Builder
	for i := 1; file.Len() <= MaxOutputBytes*2; i++ {
		fmt.Fprintf(&file, "%d %s\n", i, strings.Repeat("x", 100))
	}
	text := file.String()
	total := strings.Count(text, "\n")
	out := map[string]any{}
	fitRead(&client.ReadResult{
		Content: text, TotalLines: total, FirstLine: 1, LastLine: total,
		SizeBytes: int64(len(text)), SHA256: digest([]byte(text)),
	}, out)

	content := out["content"].(string)
	switch {
	case len(content) > MaxOutputBytes:
		t.Fatalf("content = %d bytes, cap is %d", len(content), MaxOutputBytes)
	case !strings.HasSuffix(content, "\n"):
		t.Fatal("the cut did not land on a line boundary")
	case out["truncated"] != true || out["long_line"] != nil:
		t.Fatalf("a window of whole lines was misreported: %v", out)
	case out["first_line"] != 1 || out["total_lines"] != total:
		t.Fatalf("the window misdescribes the file: %v", out)
	}
	kept := strings.Count(content, "\n")
	if out["last_line"] != kept || out["next_offset"] != kept+1 {
		t.Fatalf("%d lines returned, last_line %v, next_offset %v", kept, out["last_line"], out["next_offset"])
	}
	// The continuation must start on the very next line: each line here names
	// its own number, so the first line of the remainder settles it.
	number, _, _ := strings.Cut(text[len(content):], " ")
	if number != strconv.Itoa(out["next_offset"].(int)) {
		t.Fatalf("the rest of the file starts at line %s, next_offset says %v", number, out["next_offset"])
	}
}

// A host that can load an Agent Skill should not have to be told the protocol
// again every conversation, so discovery names the file that carries it.
func TestDiscoveryPointsAtTheSkillFile(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(getDiscovery(t, staticHandler(t), "?format=json").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["skill_url"] != "https://portal.example/webfetch/skill" {
		t.Errorf("skill_url = %v", doc["skill_url"])
	}
	// It is the portal that serves the owner's surfaces, not the relay.
	if doc["help_url"] != "https://portal.example/webfetch/help" {
		t.Errorf("help_url = %v", doc["help_url"])
	}
	if body := getDiscovery(t, staticHandler(t), "").Body.String(); !strings.Contains(body, "https://portal.example/webfetch/skill") {
		t.Error("the HTML discovery page never links the skill file")
	}
}
