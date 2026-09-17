package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
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
	for _, rule := range []string{
		"client_nonce_per_conversation", "no_url_reuse", "get_only", "no_store",
		"rid_unique_per_operation", "pairing_required_means_not_started",
		"unknown_means_check_the_device", "human_approves",
	} {
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

func TestLongJobIsNotReportedUnknownBeforeItsOwnDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		timeout int
		want    string
	}{
		{"two minutes into a fifteen minute job", 120 * time.Second, 900, "running"},
		{"past a thirty second job", 30*time.Second + staleGrace + time.Second, 30, "unknown"},
		{"past a fifteen minute job", 900*time.Second + staleGrace + time.Second, 900, "unknown"},
		{"a finished job is never re-judged", time.Hour, 30, "done"},
	} {
		state := "running"
		if tc.want == "done" {
			state = "done"
		}
		if got := jobState(state, tc.age, tc.timeout); got != tc.want {
			t.Fatalf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}
