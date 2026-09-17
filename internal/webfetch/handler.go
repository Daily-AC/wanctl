// Package webfetch adapts finite GET fetches to ordinary wanctl controller
// operations. All authorization comes from wanctl delegation and device policy.
package webfetch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wanctl/internal/client"
	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

const (
	MaxURLBytes    = 8192
	MaxOutputBytes = 32 << 10
	MaxWriteBytes  = 2048
	MaxRequestTime = 60 * time.Second
)

var ticketPattern = regexp.MustCompile(`^[0-9]{10}-[a-f0-9]{48}$`)
var clientNoncePattern = regexp.MustCompile(`^[a-f0-9]{48}$`)
var ridPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Config struct {
	Store        delegation.Store
	Jobs         delegation.JobStore
	Seed         []byte
	RelayURL     string
	PublicOrigin string
	PortalOrigin string
}

type Handler struct {
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	queue    chan work
	wg       sync.WaitGroup
	mu       sync.Mutex
	requests map[string]rateWindow
}

type rateWindow struct {
	at    time.Time
	count int
}
type work struct {
	job       delegation.Job
	request   delegation.Request
	token     string
	operation Operation
}

type Operation struct {
	Tool    string `json:"tool"`
	Target  string `json:"target"`
	Command string `json:"command,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Timeout int    `json:"timeout_seconds"`
}

func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil || cfg.Jobs == nil || len(cfg.Seed) < 32 {
		return nil, fmt.Errorf("webfetch requires delegation/job stores and a seed of at least 32 bytes")
	}
	for _, origin := range []string{cfg.PublicOrigin, cfg.PortalOrigin, cfg.RelayURL} {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) {
			return nil, fmt.Errorf("webfetch origins must use HTTPS (or loopback HTTP)")
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("webfetch origins cannot contain a path")
		}
	}
	cfg.Seed = append([]byte(nil), cfg.Seed...)
	cfg.PublicOrigin, cfg.PortalOrigin, cfg.RelayURL = strings.TrimRight(cfg.PublicOrigin, "/"), strings.TrimRight(cfg.PortalOrigin, "/"), strings.TrimRight(cfg.RelayURL, "/")
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{cfg: cfg, ctx: ctx, cancel: cancel, queue: make(chan work, 64), requests: map[string]rateWindow{}}
	for i := 0; i < 4; i++ {
		h.wg.Add(1)
		go h.worker()
	}
	if cleaner, ok := cfg.Store.(interface {
		CleanupDelegations(context.Context, time.Duration) error
	}); ok {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			ticker := time.NewTicker(15 * time.Minute)
			defer ticker.Stop()
			for {
				ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
				if err := cleaner.CleanupDelegations(ctx, 24*time.Hour); err != nil && h.ctx.Err() == nil {
					log.Print("webfetch: expired-record cleanup unavailable")
				}
				cancel()
				select {
				case <-h.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	return h, nil
}

func (h *Handler) Close() { h.cancel(); h.wg.Wait() }

func (h *Handler) rateAllowed(key string, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if len(h.requests) >= 4096 {
		for k, v := range h.requests {
			if now.Sub(v.at) > time.Minute {
				delete(h.requests, k)
			}
		}
	}
	v, exists := h.requests[key]
	if !exists && len(h.requests) >= 4096 {
		return false
	}
	if now.Sub(v.at) > time.Minute {
		v = rateWindow{at: now}
	}
	v.count++
	h.requests[key] = v
	return v.count <= limit
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (h *Handler) derive(domain, input string) []byte {
	m := hmac.New(sha256.New, h.cfg.Seed)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(input))
	return m.Sum(nil)
}

func (h *Handler) credentials(ticket string) (string, string, *transport.Identity, error) {
	id := "d_" + digest([]byte(ticket))[:32]
	token := "wfd_" + hex.EncodeToString(h.derive("wanctl-webfetch-access-v1", ticket))
	identity, err := transport.IdentityFromSeed(h.derive("wanctl-webfetch-identity-v1", id)[:ed25519.SeedSize], "wanctl-webfetch:"+id)
	return id, token, identity, err
}

func (h *Handler) sessionURL(ticket string) string {
	return h.cfg.PublicOrigin + "/webfetch/s/" + ticket
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private, max-age=0")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.respond(w, r, 405, map[string]any{"error": "GET required; HEAD never creates requests or runs tools"})
		return
	}
	if len(r.URL.RequestURI()) > MaxURLBytes {
		h.respond(w, r, 414, map[string]any{"error": "URL exceeds 8192 bytes"})
		return
	}
	if r.Header.Get("Origin") != "" {
		h.respond(w, r, 403, map[string]any{"error": "cross-origin browser requests are not supported"})
		return
	}
	if r.URL.Path == "/webfetch" || r.URL.Path == "/webfetch/" || r.URL.Path == "/webfetch/v1" {
		h.respond(w, r, 200, map[string]any{
			"title": "wanctl WebFetch", "status": "start", "entry_url": h.cfg.PublicOrigin + "/webfetch/v1",
			"owner_start_url":     h.cfg.PortalOrigin + "/webfetch/connect",
			"start_url_template":  h.cfg.PublicOrigin + "/webfetch/new/{client_nonce}",
			"client_nonce_format": "48 lowercase hexadecimal characters from 24 cryptographically random bytes; never reuse a sample, guess randomness, or copy another conversation's value",
			"instruction":         "This is a GET-only API, not a web form. If you have a secure random generator, create client_nonce, substitute it into start_url_template and read that complete URL with the same URL-reading tool. Otherwise give owner_start_url to the owner: that page generates a fresh connection prompt to paste here. Never fetch the literal template or invent approval/status URLs. Verify the returned client_nonce equals yours; a mismatch indicates another request's cached response. Give the real approval_url and continuation_prompt to the owner and wait. Only the owner may approve in wanctl. A URL-reading tool can read subsequent GET URLs too; no browser cookies, POST, Python, MCP or special headers are required.",
			"authorization":       "wanctl device scope, expiry, revocation, identity trust and device-local policy apply",
		})
		return
	}
	// A public, commonly fetched discovery URL must never carry a live ticket:
	// extractors may reuse responses despite Cache-Control: no-store. The caller
	// creates a unique fetch URL first; the server still generates the actual
	// secret ticket independently. Repeating a nonce at the origin cannot recover
	// an existing ticket or approved grant.
	clientNonce := ""
	if strings.HasPrefix(r.URL.Path, "/webfetch/new/") {
		clientNonce = strings.TrimPrefix(r.URL.Path, "/webfetch/new/")
		if !clientNoncePattern.MatchString(clientNonce) {
			h.respond(w, r, 400, map[string]any{"error": "client_nonce must be 48 fresh random lowercase hexadecimal characters; do not use the literal URL template", "entry_url": h.cfg.PublicOrigin + "/webfetch/v1"})
			return
		}
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			h.fail(w, r, err)
			return
		}
		ticket := strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(random[:])
		clonedURL := *r.URL
		clonedURL.Path, clonedURL.RawPath = "/webfetch/s/"+ticket, ""
		r = r.Clone(r.Context())
		r.URL = &clonedURL
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "webfetch" || parts[1] != "s" || !ticketPattern.MatchString(parts[2]) {
		h.respond(w, r, 404, map[string]any{"error": "not found"})
		return
	}
	ticket := parts[2]
	issuedSeconds, _ := strconv.ParseInt(strings.SplitN(ticket, "-", 2)[0], 10, 64)
	issued := time.Unix(issuedSeconds, 0)
	if issued.After(time.Now().Add(30*time.Second)) || !time.Now().Before(issued.Add(70*time.Minute)) {
		h.fail(w, r, delegation.ErrExpired)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		h.respond(w, r, 400, map[string]any{"error": "invalid URL encoding"})
		return
	}
	for _, values := range query {
		if len(values) != 1 {
			h.respond(w, r, 400, map[string]any{"error": "duplicate query parameter"})
			return
		}
	}
	id, token, identity, err := h.credentials(ticket)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.rateAllowed("grant:"+id, 120) {
		h.respond(w, r, 429, map[string]any{"error": "request rate exceeded"})
		return
	}
	request, err := h.cfg.Store.GetDelegationByTicket(r.Context(), id, digest([]byte(ticket)))
	if errors.Is(err, delegation.ErrNotFound) && len(parts) == 3 {
		if !time.Now().Before(issued.Add(10 * time.Minute)) {
			h.fail(w, r, delegation.ErrExpired)
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !h.rateAllowed("new:"+ip, 10) {
			h.respond(w, r, 429, map[string]any{"error": "too many access requests"})
			return
		}
		label := query.Get("label")
		if label == "" {
			label = "WebFetch AI"
		}
		if len(label) > 120 || strings.ContainsAny(label, "\r\n\x00") || !utf8.ValidString(label) {
			h.respond(w, r, 400, map[string]any{"error": "invalid client label"})
			return
		}
		request, err = h.cfg.Store.CreateDelegation(r.Context(), delegation.NewRequest{ID: id, TicketHash: digest([]byte(ticket)), TokenHash: digest([]byte(token)), Label: label, ControllerFingerprint: identity.Fingerprint, RequestExpiresAt: issued.Add(10 * time.Minute)})
		if err == nil {
			log.Printf("webfetch: request created grant=%s", request.ID)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(parts) == 3 && request.Status == "pending" {
		statusURL := h.statusURL(ticket)
		pending := map[string]any{
			"title": "Device access requested", "status": "pending", "request_id": id,
			"request_expires_at": request.RequestExpiresAt, "controller_fingerprint": identity.Fingerprint,
			"approval_url": h.cfg.PortalOrigin + "/webfetch/approve?request=" + url.QueryEscape(id),
			"status_url":   statusURL, "continuation_prompt": h.continuationPrompt(statusURL),
			"instruction": "Give the owner BOTH approval_url and continuation_prompt, including its complete URL. The owner verifies identities, selects devices and approves a duration in wanctl. Stop while approval is pending. After approval, GET the SAME status_url to discover devices and tools; do not open start_url or create another request. This GET does not grant access.",
		}
		if clientNonce != "" {
			pending["client_nonce"] = clientNonce
		}
		h.respond(w, r, 200, pending)
		return
	}
	access, ok := h.cfg.Store.ResolveAccess(token)
	if !ok || !access.Delegated || access.GrantID != id || request.Status != "approved" || access.ControllerFingerprint != identity.Fingerprint {
		log.Printf("webfetch: request rejected grant=%s reason=access_denied", id)
		h.respond(w, r, 403, map[string]any{"error": "delegation is not approved, has expired, or was revoked", "error_code": "access_denied", "grant_status": request.Status})
		return
	}
	switch {
	case len(parts) == 3:
		h.respond(w, r, 200, h.manifest(ticket, access))
	case len(parts) == 4 && parts[3] == "call":
		op, err := parseOperation(query)
		if err != nil {
			log.Printf("webfetch: call rejected grant=%s reason=invalid_parameters", id)
			h.respond(w, r, 400, map[string]any{"error": err.Error(), "error_code": "invalid_parameters", "status_url": h.statusURL(ticket), "instruction": "No job was created. Read status_url and correct the reported parameter. Copy a devices[].target exactly; do not guess device names or enumerate target formats."})
			return
		}
		if !access.Allows(op.Target) {
			log.Printf("webfetch: call rejected grant=%s reason=target_not_allowed", id)
			h.respond(w, r, 403, map[string]any{"error": "target is not in this delegation's allowed devices", "error_code": "target_not_allowed", "status_url": h.statusURL(ticket), "instruction": "No job was created. Only exact devices[].target values in status_url are allowed. Do not guess targets or try other devices."})
			return
		}
		payload, _ := json.Marshal(op)
		job, fresh, err := h.cfg.Jobs.BeginJob(r.Context(), id, query.Get("rid"), digest(payload), payload)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if fresh {
			log.Printf("webfetch: job created grant=%s job=%s", id, job.ID)
			select {
			case h.queue <- work{job: job, request: request, token: token, operation: op}:
			default:
				result, _ := json.Marshal(map[string]any{"ok": false, "error": "adapter queue is full; operation did not start"})
				if err := h.cfg.Jobs.FinishJob(r.Context(), id, job.ID, "failed", result); err != nil {
					h.fail(w, r, err)
					return
				}
				job.State, job.Result = "failed", result
			}
		}
		h.jobResponse(w, r, ticket, job, !fresh)
	case len(parts) == 5 && parts[3] == "jobs":
		job, err := h.cfg.Jobs.GetJob(r.Context(), id, parts[4])
		if err != nil {
			h.fail(w, r, err)
			return
		}
		var op Operation
		if json.Unmarshal(job.Payload, &op) != nil || !access.Allows(op.Target) {
			h.fail(w, r, delegation.ErrForbidden)
			return
		}
		h.jobResponse(w, r, ticket, job, false)
	default:
		h.respond(w, r, 404, map[string]any{"error": "not found"})
	}
}

func parseOperation(q url.Values) (Operation, error) {
	op := Operation{Tool: q.Get("tool"), Target: q.Get("target"), Timeout: 30}
	if !ridPattern.MatchString(q.Get("rid")) {
		return op, fmt.Errorf("rid must contain 1..64 ASCII letters, digits, hyphens or underscores")
	}
	if len(op.Target) > 200 || strings.ContainsAny(op.Target, "\r\n\x00") || strings.Count(op.Target, "/") != 1 || strings.HasPrefix(op.Target, "/") || strings.HasSuffix(op.Target, "/") {
		return op, fmt.Errorf("target must equal a devices[].target value in namespace/device_id format; a bare namespace, bare ID or namespace:ID is invalid")
	}
	allowed := map[string]bool{"rid": true, "tool": true, "target": true, "format": true, "timeout_seconds": true}
	if value := q.Get("timeout_seconds"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 60 {
			return op, fmt.Errorf("timeout_seconds must be 1..60")
		}
		op.Timeout = n
	}
	switch op.Tool {
	case "exec":
		allowed["command"], allowed["cwd"] = true, true
		op.Command, op.Cwd = q.Get("command"), q.Get("cwd")
		if op.Command == "" || len(op.Command) > 2048 || len(op.Cwd) > 1024 || strings.ContainsAny(op.Command+op.Cwd, "\x00") {
			return op, fmt.Errorf("command/cwd exceeds limits or is invalid")
		}
	case "write_text", "read_text":
		allowed["path"] = true
		op.Path = q.Get("path")
		if op.Path == "" || len(op.Path) > 1024 || strings.ContainsAny(op.Path, "\x00") {
			return op, fmt.Errorf("path required and must be at most 1024 bytes")
		}
		if op.Tool == "write_text" {
			allowed["content"] = true
			op.Content = q.Get("content")
			if !q.Has("content") || len(op.Content) > MaxWriteBytes || !utf8.ValidString(op.Content) {
				return op, fmt.Errorf("content is required, must be UTF-8 and at most 2048 bytes; use content= explicitly to write an empty file")
			}
		}
	default:
		return op, fmt.Errorf("unknown tool")
	}
	for key := range q {
		if !allowed[key] {
			return op, fmt.Errorf("unknown parameter %q", key)
		}
	}
	return op, nil
}

func (h *Handler) jobResponse(w http.ResponseWriter, r *http.Request, ticket string, job delegation.Job, duplicate bool) {
	resultURL := h.sessionURL(ticket) + "/jobs/" + job.ID
	state := job.State
	if (state == "running" || state == "queued") && time.Since(job.CreatedAt) > 90*time.Second {
		state = "unknown"
	}
	statusURL := h.statusURL(ticket)
	data := map[string]any{"title": "wanctl job", "job_id": job.ID, "request_id": job.RequestID, "status": state, "duplicate_request": duplicate, "result_url": resultURL, "result": job.Result, "status_url": statusURL, "continuation_prompt": h.continuationPrompt(statusURL)}
	if state == "running" || state == "queued" {
		data["next_url"] = resultURL + "?check=" + strconv.FormatInt(time.Now().UnixNano(), 10)
		data["instruction"] = "Read next_url for the result. Do not submit this operation under a new rid."
	}
	if state == "unknown" {
		data["instruction"] = "The operation outcome is unknown after an interruption. Do not automatically repeat it. Ask the owner to inspect device logs."
	}
	h.respond(w, r, 200, data)
}

func (h *Handler) worker() {
	defer h.wg.Done()
	for {
		select {
		case <-h.ctx.Done():
			return
		case task := <-h.queue:
			h.execute(task)
		}
	}
}

func (h *Handler) execute(task work) {
	result := map[string]any{"ok": false, "tool": task.operation.Tool, "target": task.operation.Target}
	state := "failed"
	defer func() {
		if recover() != nil {
			result = map[string]any{"ok": false, "error": "internal adapter failure; outcome unknown"}
			state = "unknown"
		}
		encoded, _ := json.Marshal(result)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.cfg.Jobs.FinishJob(ctx, task.job.GrantID, task.job.ID, state, encoded); err != nil {
			log.Printf("webfetch: could not persist outcome for grant=%s job=%s; do not retry automatically", task.job.GrantID, task.job.ID)
		}
	}()
	access, ok := h.cfg.Store.ResolveAccess(task.token)
	if !ok || !access.Delegated || access.GrantID != task.job.GrantID || !access.Allows(task.operation.Target) {
		result["error"] = "delegation is no longer valid; operation did not start"
		return
	}
	deadline := task.job.CreatedAt.Add(time.Duration(task.operation.Timeout) * time.Second)
	if access.ExpiresAt.Before(deadline) {
		deadline = access.ExpiresAt
	}
	if !time.Now().Before(deadline) {
		result["error"] = "job expired before dispatch; operation did not start"
		return
	}
	ctx, cancel := context.WithDeadline(h.ctx, deadline)
	defer cancel()
	identity, err := transport.IdentityFromSeed(h.derive("wanctl-webfetch-identity-v1", task.job.GrantID)[:ed25519.SeedSize], "wanctl-webfetch:"+task.job.GrantID)
	if err != nil {
		result["error"] = "controller identity unavailable"
		return
	}
	if identity.Fingerprint != access.ControllerFingerprint {
		result["error"] = "controller identity changed; request a new delegation"
		return
	}
	known := transport.NewMemStore()
	for _, device := range access.Devices {
		if err := known.Pin(device.Target(), device.Fingerprint, false); err != nil {
			result["error"] = "invalid approved device identity"
			return
		}
	}
	c := client.NewWith(identity, known, h.cfg.RelayURL, task.token, "http")
	c.SetLabel("WebFetch · " + task.request.Label + " · " + task.job.GrantID)
	switch task.operation.Tool {
	case "exec":
		stdout, stderr := &boundedOutput{limit: MaxOutputBytes / 2, cancel: cancel}, &boundedOutput{limit: MaxOutputBytes / 2, cancel: cancel}
		var code int
		code, err = c.ExecTo(ctx, client.ExecRequest{Target: task.operation.Target, Command: task.operation.Command, Cwd: task.operation.Cwd, OneShot: true}, stdout, stderr)
		result["stdout"], result["stderr"], result["exit_code"] = stdout.String(), stderr.String(), code
		result["truncated"] = stdout.truncated || stderr.truncated
	case "write_text":
		data := []byte(task.operation.Content)
		err = c.PushBytes(ctx, task.operation.Target, task.operation.Path, data, 0o600)
		result["byte_count"], result["sha256"] = len(data), digest(data)
	case "read_text":
		var data []byte
		data, err = c.PullBytes(ctx, task.operation.Target, task.operation.Path, MaxOutputBytes)
		if err == nil && !utf8.Valid(data) {
			err = fmt.Errorf("file is not UTF-8 text")
		}
		if err == nil {
			result["content"], result["byte_count"], result["sha256"] = string(data), len(data), digest(data)
		}
	}
	if err != nil {
		result["error"] = boundedText(err.Error(), 4096)
		var rejected *client.RejectError
		var untrusted *client.TrustRequiredError
		if task.operation.Tool != "read_text" && !errors.As(err, &rejected) && !errors.As(err, &untrusted) {
			state = "unknown"
			result["instruction"] = "The operation may have started. Do not retry under a new rid; ask the owner to inspect the device."
		}
		if errors.As(err, &rejected) {
			result["error"] = boundedText(rejected.Reason, 4096)
			if len(rejected.PairingURL) <= 2048 && h.ownerLink(rejected.PairingURL) {
				result["pairing_url"] = rejected.PairingURL
				result["error_code"] = "pairing_required"
				result["execution_started"] = false
				result["instruction"] = "The operation did not start. The owner must approve pairing in wanctl; do not approve it by fetching the link. After the owner confirms pairing, submit a NEW attempt with a NEW rid. Reusing this completed job's rid only returns the same failure."
			}
		}
		return
	}
	result["ok"] = true
	state = "done"
}

func boundedText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return text + " [truncated]"
}

func (h *Handler) ownerLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	origin, err := url.Parse(h.cfg.PortalOrigin)
	return err == nil && u.Scheme == origin.Scheme && u.Host == origin.Host
}

type boundedOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
	cancel    context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.truncated = true
		if remaining > 0 {
			b.Buffer.Write(p[:remaining])
		}
		b.cancel()
		return n, nil
	}
	return b.Buffer.Write(p)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := 500, "delegation service unavailable"
	switch {
	case errors.Is(err, delegation.ErrNotFound):
		status, message = 404, "not found"
	case errors.Is(err, delegation.ErrForbidden), errors.Is(err, delegation.ErrExpired):
		status, message = 403, "delegation denied or expired"
	case errors.Is(err, delegation.ErrConflict):
		status, message = 409, "request ID already exists with different arguments"
	case errors.Is(err, delegation.ErrInvalid):
		status, message = 400, "invalid request"
	case errors.Is(err, delegation.ErrLimit):
		status, message = 429, "request or job limit exceeded"
	}
	h.respond(w, r, status, map[string]any{"error": message})
}

var responseTemplate = template.Must(template.New("response").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>wanctl WebFetch</title>
<style>body{font:16px/1.65 system-ui;max-width:800px;margin:64px auto;padding:0 24px;color:#222}a,code{overflow-wrap:anywhere}a{color:#1268d3}pre{background:#f5f5f7;padding:20px;border-radius:12px;white-space:pre-wrap;overflow-wrap:anywhere}h1{font-size:28px}h2{font-size:22px}</style>
<h1>wanctl WebFetch</h1>
{{if .Document.call_endpoint}}
<h2>Authorized: call tools with GET</h2>
<p>Use the same URL-reading tool that opened this page. Fill a template below with URL-encoded values and GET the complete URL. No separate exec connector, POST, cookies or special headers are needed.</p>
<p><strong>call_endpoint</strong><br><code>{{.Document.call_endpoint}}</code></p>
<p><strong>Allowed target values</strong>{{range .Document.devices}}<br><code>{{.Target}}</code>{{end}}</p>
{{range .Document.tools}}<p><strong>{{.name}} — GET call_url_template</strong><br><code>{{.call_url_template}}</code><br>{{.description}}</p>{{end}}
<p>Replace every {placeholder}; never fetch an unfilled template. Use a new rid for each new operation and the same rid and arguments when retrying it. GET result_url / next_url until done, failed or unknown (at most 8 polls). Return only the actual result. Keep the full status_url and exec template in your reply so later turns can continue.</p>
{{end}}
{{range .Links}}<p><strong>{{.Name}}</strong><br><a href="{{.URL}}">{{.URL}}</a></p>{{end}}
<h2>Protocol response</h2><pre>{{.JSON}}</pre></html>`))

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	data["protocol"] = "wanctl.webfetch.v1"
	data["help_url"] = h.cfg.PortalOrigin + "/webfetch/help"
	data["http_status"] = status
	if status >= 400 {
		data["status"] = "error"
		data["entry_url"] = h.cfg.PublicOrigin + "/webfetch/v1"
	}
	encoded, _ := json.MarshalIndent(data, "", "  ")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(encoded)
		return
	}
	var links []struct{ Name, URL string }
	for _, key := range []string{"help_url", "entry_url", "owner_start_url", "approval_url", "status_url", "next_url", "result_url"} {
		if value, ok := data[key].(string); ok {
			links = append(links, struct{ Name, URL string }{key, value})
		}
	}
	var body bytes.Buffer
	if responseTemplate.Execute(&body, struct {
		Links    []struct{ Name, URL string }
		Document map[string]any
		JSON     string
	}{links, data, string(encoded)}) != nil {
		http.Error(w, "render failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// URL extractors often discard every non-2xx response body. A GET error
	// document must remain readable, while its envelope carries the actual
	// outcome. JSON clients retain normal HTTP status codes. This changes only
	// presentation: authorization and validation returned before any dispatch.
	pageStatus := status
	if r.Method == http.MethodGet && r.Header.Get("Origin") == "" && status >= 400 && status < 500 {
		pageStatus = http.StatusOK
	}
	w.WriteHeader(pageStatus)
	_, _ = io.Copy(w, &body)
}
