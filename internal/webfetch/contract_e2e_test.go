package webfetch

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Read the same static document a URL extractor sees, not a separate API mock.
func fetchPage(t *testing.T, raw string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Anchor on the section, not on the first <pre>: the page also prints the
	// harness instructions in one, above the protocol response.
	_, section, ok := strings.Cut(string(body), "<h2>Protocol response</h2>")
	_, after, opened := strings.Cut(section, "<pre>")
	encoded, _, closed := strings.Cut(after, "</pre>")
	if !ok || !opened || !closed {
		t.Fatalf("missing readable error/document: HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(encoded)), &data); err != nil {
		t.Fatal(err)
	}
	if data["protocol"] != "wanctl.webfetch.v1" || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatal("missing protocol identity or cache protection")
	}
	if data["help_url"] != "http://127.0.0.1:9999/webfetch/help" {
		t.Fatal("client cannot discover public calling instructions")
	}
	return resp.StatusCode, data
}

func TestDiscoveryCannotCacheAnApprovedSessionForAnotherClient(t *testing.T) {
	f := liveWebFetch(t)
	_, entry := fetchPage(t, f.web.URL+"/webfetch/v1")
	encoded, _ := json.Marshal(entry)
	if entry["start_url"] != nil || strings.Contains(string(encoded), "/webfetch/s/") {
		t.Fatal("public discovery exposed a cacheable session ticket")
	}
	template := entry["start_url_template"].(string)
	firstURL := strings.ReplaceAll(template, "{client_nonce}", strings.Repeat("a", 48))
	_, first := fetchPage(t, firstURL)
	if first["client_nonce"] != strings.Repeat("a", 48) || first["status"] != "pending" {
		t.Fatalf("first request = %v", first)
	}
	statusURL := first["status_url"].(string)
	if !strings.Contains(first["continuation_prompt"].(string), statusURL) {
		t.Fatal("owner cannot recover the full continuation URL")
	}
	u, _ := url.Parse(statusURL)
	ticket := strings.TrimPrefix(u.Path, "/webfetch/s/")
	f.approve(t, ticket, false)

	// The common entry remains static and contains no grant, even after approval.
	_, after := fetchPage(t, f.web.URL+"/webfetch")
	encodedAfter, _ := json.Marshal(after)
	if string(encodedAfter) != string(encoded) {
		t.Fatal("shared discovery changed with an individual authorization")
	}
	secondURL := strings.ReplaceAll(template, "{client_nonce}", strings.Repeat("b", 48))
	_, second := fetchPage(t, secondURL)
	_, repeated := fetchPage(t, firstURL)
	for _, other := range []map[string]any{second, repeated} {
		if other["status"] != "pending" || other["request_id"] == first["request_id"] || other["status_url"] == first["status_url"] {
			t.Fatal("another fetch recovered the first client's approved session")
		}
	}
	if second["client_nonce"] != strings.Repeat("b", 48) {
		t.Fatal("caller cannot check the cache key echo")
	}

	var count int
	f.db.QueryRow("SELECT count(*) FROM delegation_requests").Scan(&count)
	if count != 3 {
		t.Fatalf("unexpected creation count: %d", count)
	}
	status, invalid := fetchPage(t, strings.ReplaceAll(template, "{client_nonce}", "GENERATE_A_NEW_NONCE"))
	if status != 200 || invalid["http_status"] != float64(400) || invalid["status"] != "error" {
		t.Fatalf("invalid nonce not readable: %d %v", status, invalid)
	}
	f.db.QueryRow("SELECT count(*) FROM delegation_requests").Scan(&count)
	if count != 3 {
		t.Fatal("an unfilled template created an authorization request")
	}
}

func TestDiscoveredTargetExecutesAndHTMLDenialsRemainEnforced(t *testing.T) {
	f := liveWebFetch(t)
	session, grant := f.approve(t, testTicket("e"), true)
	_, manifest := fetchPage(t, session)
	device := manifest["devices"].([]any)[0].(map[string]any)
	target := device["target"].(string)
	if target != "owner/"+f.device.ID {
		t.Fatalf("discovered target = %q", target)
	}
	endpoint := manifest["call_endpoint"].(string)
	// A reader can learn how to execute from the opening HTML alone, without
	// paging through the JSON schemas or guessing endpoints from prior turns.
	response, err := http.Get(session)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	opening, _, _ := strings.Cut(string(body), "<h2>Protocol response</h2>")
	match := regexp.MustCompile(`<strong>exec — GET call_url_template</strong><br><code>([^<]+)</code>`).FindStringSubmatch(opening)
	if len(match) != 2 || !strings.Contains(opening, target) || !strings.Contains(opening, endpoint) {
		t.Fatal("opening HTML omits the executable contract")
	}
	execTemplate := html.UnescapeString(match[1])
	if !strings.Contains(manifest["continuation_prompt"].(string), "call_url_template") {
		t.Fatal("continuation prompt drops the calling contract")
	}
	for _, entry := range manifest["tools"].([]any) {
		tool := entry.(map[string]any)
		schema := tool["input_schema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		choices := props["target"].(map[string]any)["enum"].([]any)
		if len(choices) != 1 || choices[0] != target {
			t.Fatal("tool schema and discovered device scope disagree")
		}
		// A crawler following an unfilled example must not execute anything.
		_, unfilled := fetchPage(t, tool["call_url_template"].(string))
		if unfilled["http_status"] != float64(400) {
			t.Fatal("an unfilled call template was accepted")
		}
	}
	q := url.Values{"rid": {"discovery-exec"}, "tool": {"exec"}, "target": {device["id"].(string)}, "command": {"printf webfetch-ok"}}
	status, denied := fetchPage(t, endpoint+"?"+q.Encode())
	if status != 200 || denied["http_status"] != float64(400) || denied["status"] != "error" || denied["error_code"] != "invalid_parameters" {
		t.Fatalf("bare ID was not an actionable readable error: %d %v", status, denied)
	}
	if !strings.Contains(denied["error"].(string), "namespace/device_id") {
		t.Fatal("error does not explain the target format")
	}
	if status, _ := fetchDoc(t, endpoint+"?"+q.Encode()); status != 400 {
		t.Fatalf("JSON HTTP contract changed: %d", status)
	}
	q.Set("target", "another-owner/"+f.device.ID)
	_, outside := fetchPage(t, endpoint+"?"+q.Encode())
	if outside["http_status"] != float64(403) || outside["error_code"] != "target_not_allowed" {
		t.Fatalf("scope check changed: %v", outside)
	}
	var count int
	f.db.QueryRow("SELECT count(*) FROM delegation_jobs").Scan(&count)
	if count != 0 {
		t.Fatal("readable errors created executable jobs")
	}

	// Fill the visible template with the requested operation; no JSON schema or
	// hard-coded endpoint path is needed to complete the first successful call.
	q.Set("target", target)
	callURL := strings.NewReplacer("{rid}", url.QueryEscape(q.Get("rid")), "{target}", url.QueryEscape(target), "{command}", url.QueryEscape(q.Get("command"))).Replace(execTemplate)
	_, job := fetchPage(t, callURL)
	job = awaitJob(t, job)
	if job["status"] != "done" || job["result"].(map[string]any)["stdout"] != "webfetch-ok" {
		t.Fatalf("discovered call failed: %v", job)
	}
	_, duplicate := fetchPage(t, endpoint+"?"+q.Encode())
	if duplicate["job_id"] != job["job_id"] || duplicate["duplicate_request"] != true {
		t.Fatal("retry did not preserve the original job")
	}
	q.Set("command", "echo different")
	_, conflict := fetchPage(t, endpoint+"?"+q.Encode())
	if conflict["http_status"] != float64(409) {
		t.Fatal("HTML presentation changed idempotency enforcement")
	}
	if err := f.pg.RevokeToken("owner", grant.TokenID); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{session, job["result_url"].(string), endpoint + "?" + q.Encode()} {
		status, revoked := fetchPage(t, raw)
		if status != 200 || revoked["http_status"] != float64(403) || revoked["status"] != "error" || revoked["result"] != nil || revoked["devices"] != nil {
			t.Fatalf("revoked HTML leaked data: %d %v", status, revoked)
		}
		if status, _ := fetchDoc(t, raw); status != 403 {
			t.Fatalf("revoked JSON = %d", status)
		}
	}
	f.db.QueryRow("SELECT count(*) FROM delegation_jobs").Scan(&count)
	if count != 1 {
		t.Fatalf("denial/retry dispatched another job: %d", count)
	}
}
