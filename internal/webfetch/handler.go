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
	if r.URL.Path == "/webfetch" || r.URL.Path == "/webfetch/" {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			h.fail(w, r, err)
			return
		}
		ticket := strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(random[:])
		h.respond(w, r, 200, map[string]any{"title": "wanctl WebFetch", "status": "start", "instruction": "Open start_url to request temporary device access. Give approval_url to the device owner. Only the owner may approve in the wanctl portal. Do not fetch an approval link as a way to approve it.", "start_url": h.sessionURL(ticket), "authorization": "wanctl device scope, expiry, revocation, identity trust and device-local policy apply"})
		return
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
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(parts) == 3 && request.Status == "pending" {
		h.respond(w, r, 200, map[string]any{"title": "Device access requested", "status": "pending", "request_id": id, "request_expires_at": request.RequestExpiresAt, "controller_fingerprint": identity.Fingerprint, "approval_url": h.cfg.PortalOrigin + "/webfetch/approve?request=" + url.QueryEscape(id), "status_url": h.sessionURL(ticket) + "?check=" + strconv.FormatInt(time.Now().UnixNano(), 10), "instruction": "Ask the owner to open approval_url, verify identities, choose devices and approve a duration. Stop and report this link while approval is pending. This GET does not grant access."})
		return
	}
	access, ok := h.cfg.Store.ResolveAccess(token)
	if !ok || !access.Delegated || access.GrantID != id || request.Status != "approved" || access.ControllerFingerprint != identity.Fingerprint {
		h.respond(w, r, 403, map[string]any{"error": "delegation is not approved, has expired, or was revoked", "status": request.Status})
		return
	}
	switch {
	case len(parts) == 3:
		h.respond(w, r, 200, map[string]any{"title": "wanctl tools", "status": "approved", "request_id": id, "owner": access.Namespace, "expires_at": access.ExpiresAt, "devices": access.Devices, "call_endpoint": h.sessionURL(ticket) + "/call", "instruction": "Construct GET call_endpoint?rid=UNIQUE_ID&tool=TOOL&target=CANONICAL_TARGET plus the tool parameters. URL-encode all values. Reuse exactly the same rid and arguments for retries. Poll result_url/next_url until done; at most 8 polls. Pairing or device-policy approval must be performed by the owner in wanctl, never by the AI.", "tools": []map[string]any{{"name": "exec", "parameters": []string{"command", "cwd (optional)", "timeout_seconds (1..60, default30)"}, "description": "One-shot command through wanctl; device policy applies; bounded stdout/stderr."}, {"name": "write_text", "parameters": []string{"path", "content"}, "description": "Write up to 2048 UTF-8 bytes through wanctl file-put; device write rules apply."}, {"name": "read_text", "parameters": []string{"path"}, "description": "Read a UTF-8 file up to 32768 bytes through wanctl file-get; device read rules apply."}}, "limits": map[string]any{"jobs_per_grant": 64, "url_bytes": MaxURLBytes, "output_bytes": MaxOutputBytes}, "notice": "Commands and results are visible to this adapter and the web chat provider. Do not send secrets. A lost/ambiguous job is never automatically rerun."})
	case len(parts) == 4 && parts[3] == "call":
		op, err := parseOperation(query)
		if err != nil {
			h.respond(w, r, 400, map[string]any{"error": err.Error()})
			return
		}
		if !access.Allows(op.Target) {
			h.fail(w, r, delegation.ErrForbidden)
			return
		}
		payload, _ := json.Marshal(op)
		job, fresh, err := h.cfg.Jobs.BeginJob(r.Context(), id, query.Get("rid"), digest(payload), payload)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if fresh {
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
	if op.Target == "" || len(op.Target) > 200 || strings.ContainsAny(op.Target, "\r\n\x00") {
		return op, fmt.Errorf("canonical target required")
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
			if len(op.Content) > MaxWriteBytes || !utf8.ValidString(op.Content) {
				return op, fmt.Errorf("content must be UTF-8 and at most 2048 bytes")
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
	data := map[string]any{"title": "wanctl job", "job_id": job.ID, "request_id": job.RequestID, "status": state, "duplicate_request": duplicate, "result_url": resultURL, "result": job.Result}
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
				result["instruction"] = "The device owner must approve pairing in wanctl. Do not approve it by fetching the link."
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

var responseTemplate = template.Must(template.New("response").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>wanctl WebFetch</title><style>body{font:16px/1.65 system-ui;max-width:800px;margin:64px auto;padding:0 24px;color:#222}a{color:#1268d3;overflow-wrap:anywhere}pre{background:#f5f5f7;padding:20px;border-radius:12px;white-space:pre-wrap;overflow-wrap:anywhere}h1{font-size:28px}</style><h1>wanctl WebFetch</h1>{{range .Links}}<p><strong>{{.Name}}</strong><br><a href="{{.URL}}">{{.URL}}</a></p>{{end}}<pre>{{.JSON}}</pre></html>`))

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	encoded, _ := json.MarshalIndent(data, "", "  ")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(encoded)
		return
	}
	var links []struct{ Name, URL string }
	for _, key := range []string{"start_url", "approval_url", "status_url", "next_url", "result_url"} {
		if value, ok := data[key].(string); ok {
			links = append(links, struct{ Name, URL string }{key, value})
		}
	}
	var body bytes.Buffer
	if responseTemplate.Execute(&body, struct {
		Links []struct{ Name, URL string }
		JSON  string
	}{links, string(encoded)}) != nil {
		http.Error(w, "render failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.Copy(w, &body)
}
