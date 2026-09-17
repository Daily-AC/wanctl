package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"wanctl/internal/delegation"
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
