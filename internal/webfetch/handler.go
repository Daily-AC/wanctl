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
)

// Line bounds for read_text. maxReadLines is the device's own line budget, so
// asking for more is a mistake worth naming here rather than silently clamping
// on the far side. maxLineNumber is only an overflow guard: a caller may
// legitimately start at line four million of a log.
const (
	maxReadLines  = 1 << 20
	maxLineNumber = 1<<31 - 1
)

// Per-tool time budgets. An exec is whatever the device's own policy already
// allows, and that includes builds and renders: the first real caller's job was
// a ten-minute Blender run that a sixty-second ceiling turned into an unknown
// outcome. A file transfer is bounded by MaxWriteBytes / MaxOutputBytes, so a
// slow one is a stuck one and must not hold a slot for half an hour. The
// defaults are variables so a test can prove that changing one does not change
// the replay identity of a call that never sent timeout_seconds.
var (
	MaxExecSeconds     = 1800
	DefaultExecSeconds = 300
	MaxFileSeconds     = 60
	DefaultFileSeconds = 30
)

// Long jobs hold a slot for as long as they run, so these caps are what keeps
// one grant from owning the adapter. They are variables so a test can compress
// the scale.
var (
	maxOperationsPerGrant = 4
	maxOperations         = 64
)

// staleGrace is how long after a job's own deadline the adapter still expects to
// write an outcome. Past that the result is genuinely unknown. It is a variable
// so tests can compress the time scale instead of sleeping through a deadline.
var staleGrace = 90 * time.Second

var ticketPattern = regexp.MustCompile(`^[0-9]{10}-[a-f0-9]{48}$`)
var clientNoncePattern = regexp.MustCompile(`^[a-f0-9]{48}$`)
var ridPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var sha256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

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
	wg       sync.WaitGroup
	mu       sync.Mutex
	requests map[string]rateWindow
	running  map[string]int // grant -> operations in flight
	total    int
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
	// read_text paging. Nil for the same reason as Timeout: a default that the
	// caller never sent must not become part of what the URL hashes to.
	Offset *int `json:"offset,omitempty"`
	Limit  *int `json:"limit,omitempty"`
	// edit_text. New is required but may be empty, which deletes Old, so an
	// empty replacement has one canonical form rather than being an omission.
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// Nil when the caller did not send all. An explicit all=false is a
	// different URL from no all at all and has to stay one, or a caller that
	// spells the default out cannot tell a replay from a fresh operation.
	All         *bool  `json:"all,omitempty"`
	ExpectedSHA string `json:"expected_sha256,omitempty"`
	// Nil when the caller did not send timeout_seconds. The default is applied
	// at dispatch and never written into the canonical payload: a replay of an
	// identical URL must keep hashing the same way across a deploy that changes
	// a default, or a lost-response retry turns into a 409.
	Timeout *int `json:"timeout_seconds,omitempty"`
}

func (o Operation) timeout() int {
	if o.Timeout != nil {
		return *o.Timeout
	}
	_, fallback := timeoutBounds(o.Tool)
	return fallback
}

// effectiveDeadline is the moment after which this job cannot still be running:
// its own timeout, or the end of the grant, whichever comes first. One value
// drives execution, the advertised deadline_at and the stale-state inference,
// so a client is never told to wait past the point where a result can arrive.
func effectiveDeadline(created time.Time, timeout int, grantExpiry time.Time) time.Time {
	deadline := created.Add(time.Duration(timeout) * time.Second)
	if !grantExpiry.IsZero() && grantExpiry.Before(deadline) {
		return grantExpiry
	}
	return deadline
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
	h := &Handler{cfg: cfg, ctx: ctx, cancel: cancel, requests: map[string]rateWindow{}, running: map[string]int{}}
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
		h.respond(w, r, 200, discovery(h.cfg.PublicOrigin, h.cfg.PortalOrigin))
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
			"continuation_prompt_zh": h.continuationPromptZH(statusURL),
			"human_checkpoint":       humanCheckpoints()[0],
			"instruction":            "Show the human approval_url with human_checkpoint.summary (use summary_zh if you are speaking Chinese), then stop and wait. Only the human can approve, in the wanctl portal. When they say they are done, GET the SAME status_url; do not create another request. This request expires at request_expires_at.",
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
		rid, hash := query.Get("rid"), digest(payload)
		// Look the rid up before deciding anything about capacity. A client
		// recovering a lost response replays the identical URL, and telling it
		// "nothing ran, use a new rid" because the adapter happens to be full
		// would run the same operation a second time.
		switch existing, err := h.cfg.Jobs.FindJob(r.Context(), id, rid); {
		case err == nil && existing.PayloadHash != hash:
			h.conflict(w, r, ticket, id)
			return
		case err == nil:
			h.jobResponse(w, r, ticket, existing, true, op, access.ExpiresAt)
			return
		case !errors.Is(err, delegation.ErrNotFound):
			h.fail(w, r, err)
			return
		}
		// Only a genuinely new operation can be refused for capacity, and that
		// refusal writes nothing: a job row spent on something that never ran
		// would eat the caller's 64-job ledger allowance.
		if !h.reserve(id) {
			log.Printf("webfetch: call rejected grant=%s reason=adapter_busy", id)
			h.respond(w, r, 429, map[string]any{
				"error": "too many operations are already running; this one did not start", "error_code": "adapter_busy",
				"execution_started": false, "status_url": h.statusURL(ticket),
				"instruction": "No job was created and nothing ran under this rid. Read your running jobs' next_url until one finishes, then send this same URL again.",
			})
			return
		}
		// Held until the dispatch goroutine takes ownership, so that an error or
		// a panic anywhere between here and there still returns the slot.
		held := true
		defer func() {
			if held {
				h.release(id)
			}
		}()
		job, fresh, err := h.cfg.Jobs.BeginJob(r.Context(), id, rid, hash, payload)
		if errors.Is(err, delegation.ErrConflict) {
			h.conflict(w, r, ticket, id)
			return
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if fresh {
			log.Printf("webfetch: job created grant=%s job=%s", id, job.ID)
			held = false
			h.dispatch(work{job: job, request: request, token: token, operation: op})
		}
		h.jobResponse(w, r, ticket, job, !fresh, op, access.ExpiresAt)
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
		h.jobResponse(w, r, ticket, job, false, op, access.ExpiresAt)
	default:
		h.respond(w, r, 404, map[string]any{"error": "not found"})
	}
}

// timeoutBounds is the per-tool ceiling and default, in seconds. exec may be a
// build or a render; a file transfer of at most 32 KiB may not.
func timeoutBounds(tool string) (ceiling, fallback int) {
	if tool == "exec" {
		return MaxExecSeconds, DefaultExecSeconds
	}
	return MaxFileSeconds, DefaultFileSeconds
}

// lineParam reads an optional 1-based line parameter. An absent one stays
// absent for the same reason an absent timeout does: materializing today's
// default into the canonical payload would make an identical URL hash
// differently after a deploy that changes that default.
func lineParam(q url.Values, key string, ceiling int) (*int, error) {
	if !q.Has(key) {
		return nil, nil
	}
	n, err := strconv.Atoi(q.Get(key))
	if err != nil || n < 1 || n > ceiling {
		return nil, fmt.Errorf("%s must be a whole number of 1..%d", key, ceiling)
	}
	return &n, nil
}

func parseOperation(q url.Values) (Operation, error) {
	op := Operation{Tool: q.Get("tool"), Target: q.Get("target")}
	if !ridPattern.MatchString(q.Get("rid")) {
		return op, fmt.Errorf("rid must contain 1..64 ASCII letters, digits, hyphens or underscores")
	}
	if len(op.Target) > 200 || strings.ContainsAny(op.Target, "\r\n\x00") || strings.Count(op.Target, "/") != 1 || strings.HasPrefix(op.Target, "/") || strings.HasSuffix(op.Target, "/") {
		return op, fmt.Errorf("target must equal a devices[].target value in namespace/device_id format; a bare namespace, bare ID or namespace:ID is invalid")
	}
	allowed := map[string]bool{"rid": true, "tool": true, "target": true, "format": true, "timeout_seconds": true}
	switch op.Tool {
	case "exec":
		allowed["command"], allowed["cwd"] = true, true
		op.Command, op.Cwd = q.Get("command"), q.Get("cwd")
		if op.Command == "" || len(op.Command) > 2048 || len(op.Cwd) > 1024 || strings.ContainsAny(op.Command+op.Cwd, "\x00") {
			return op, fmt.Errorf("command/cwd exceeds limits or is invalid")
		}
	case "write_text", "read_text", "edit_text":
		allowed["path"] = true
		op.Path = q.Get("path")
		if op.Path == "" || len(op.Path) > 1024 || strings.ContainsAny(op.Path, "\x00") {
			return op, fmt.Errorf("path required and must be at most 1024 bytes")
		}
		switch op.Tool {
		case "write_text":
			allowed["content"] = true
			op.Content = q.Get("content")
			if !q.Has("content") || len(op.Content) > MaxWriteBytes || !utf8.ValidString(op.Content) {
				return op, fmt.Errorf("content is required, must be UTF-8 and at most 2048 bytes; use content= explicitly to write an empty file")
			}
		case "read_text":
			allowed["offset"], allowed["limit"] = true, true
			var err error
			// A file may be long enough that the line worth reading is millions
			// of lines in, so only the line budget is bounded tightly.
			if op.Offset, err = lineParam(q, "offset", maxLineNumber); err != nil {
				return op, err
			}
			if op.Limit, err = lineParam(q, "limit", maxReadLines); err != nil {
				return op, err
			}
		case "edit_text":
			allowed["old"], allowed["new"], allowed["all"], allowed["expected_sha256"] = true, true, true, true
			op.Old, op.New = q.Get("old"), q.Get("new")
			if op.Old == "" || !utf8.ValidString(op.Old) || strings.Contains(op.Old, "\x00") {
				return op, fmt.Errorf("old is required and must be UTF-8 text copied from the file; its length is bounded by the %d-byte URL", MaxURLBytes)
			}
			if !q.Has("new") || !utf8.ValidString(op.New) || strings.Contains(op.New, "\x00") {
				return op, fmt.Errorf("new is required and must be UTF-8; use new= explicitly to delete old")
			}
			if q.Has("all") {
				every := q.Get("all") == "true"
				if !every && q.Get("all") != "false" {
					return op, fmt.Errorf(`all must be "true" or "false"`)
				}
				op.All = &every
			}
			if q.Has("expected_sha256") {
				op.ExpectedSHA = q.Get("expected_sha256")
				if !sha256Pattern.MatchString(op.ExpectedSHA) {
					return op, fmt.Errorf("expected_sha256 must be the 64 hexadecimal characters a read_text result reported for this file")
				}
			}
		}
	default:
		return op, fmt.Errorf("unknown tool")
	}
	// An omitted timeout stays omitted: op is the canonical replay identity, and
	// baking today's default into it would make an identical URL hash
	// differently after a deploy that changes that default.
	if value := q.Get("timeout_seconds"); value != "" {
		ceiling, _ := timeoutBounds(op.Tool)
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > ceiling {
			return op, fmt.Errorf("timeout_seconds must be 1..%d for tool %s", ceiling, op.Tool)
		}
		op.Timeout = &n
	}
	for key := range q {
		if !allowed[key] {
			return op, fmt.Errorf("unknown parameter %q", key)
		}
	}
	return op, nil
}

// unknownInstruction says what "unknown" means without naming a cause. The
// adapter records that state for a lost transport, an overflowing output, a
// recovered panic and an elapsed deadline alike; naming only the deadline told
// the reader something false in three of the four cases.
const unknownInstruction = "The adapter lost track of this job before a result was recorded. It may have run on the device. Do not repeat it automatically: ask the human to check the device before you create a new request."

// jobState downgrades a still-running job to "unknown" only once its effective
// deadline has passed and the adapter has had staleGrace on top of that to write
// an outcome. A fixed 90-second window reported healthy long jobs as unknown.
func jobState(state string, now, deadline time.Time) string {
	if state != "running" && state != "queued" {
		return state
	}
	if now.After(deadline.Add(staleGrace)) {
		return "unknown"
	}
	return state
}

func (h *Handler) jobResponse(w http.ResponseWriter, r *http.Request, ticket string, job delegation.Job, duplicate bool, op Operation, grantExpiry time.Time) {
	resultURL := h.sessionURL(ticket) + "/jobs/" + job.ID
	now := time.Now()
	age := now.Sub(job.CreatedAt)
	deadline := effectiveDeadline(job.CreatedAt, op.timeout(), grantExpiry)
	state := jobState(job.State, now, deadline)
	statusURL := h.statusURL(ticket)
	data := map[string]any{"title": "wanctl job", "job_id": job.ID, "request_id": job.RequestID, "status": state, "duplicate_request": duplicate, "result_url": resultURL, "result": job.Result, "status_url": statusURL, "continuation_prompt": h.continuationPrompt(statusURL)}
	if state == "running" || state == "queued" {
		wait := 5
		if age > time.Minute {
			wait = 30
		}
		data["next_url"] = resultURL + "?check=" + strconv.FormatInt(now.UnixNano(), 10)
		data["poll_after_seconds"] = wait
		data["deadline_at"] = deadline
		data["instruction"] = "Still running. Wait poll_after_seconds, then read next_url. Keep polling until the status is done, failed or unknown; a long job is normal up to deadline_at, which is also bounded by when this authorization ends. Retry a lost read by fetching the same URL again; never resubmit the operation under a new rid."
	}
	if state == "unknown" {
		data["instruction"] = unknownInstruction
	}
	h.respond(w, r, 200, data)
}

// dispatch runs one operation on its own goroutine, on a slot the caller has
// already reserved. A long exec therefore cannot block another grant's short
// call behind it, which a fixed worker pool did as soon as operations were
// allowed to outlive a minute. The release is deferred here rather than inside
// execute so that every exit — a result, a transport failure, a store write
// that fails, a recovered panic — gives the slot back.
func (h *Handler) dispatch(task work) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.release(task.job.GrantID)
		h.execute(task)
	}()
}

func (h *Handler) reserve(grant string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= maxOperations || h.running[grant] >= maxOperationsPerGrant {
		return false
	}
	h.running[grant]++
	h.total++
	return true
}

func (h *Handler) release(grant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total--
	if h.running[grant]--; h.running[grant] <= 0 {
		delete(h.running, grant)
	}
}

func (h *Handler) execute(task work) {
	result := map[string]any{"ok": false, "tool": task.operation.Tool, "target": task.operation.Target}
	state := "failed"
	defer func() {
		if recover() != nil {
			result = map[string]any{"ok": false, "error": "internal adapter failure; outcome unknown", "instruction": unknownInstruction}
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
	deadline := effectiveDeadline(task.job.CreatedAt, task.operation.timeout(), access.ExpiresAt)
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
		var read *client.ReadResult
		read, err = c.ReadFile(ctx, client.ReadRequest{
			Target: task.operation.Target, Path: task.operation.Path,
			Offset: value(task.operation.Offset), Limit: value(task.operation.Limit),
		})
		if err == nil {
			fitRead(read, result)
		}
	case "edit_text":
		var edit *client.EditResult
		edit, err = c.EditFile(ctx, client.EditRequest{
			Target: task.operation.Target, Path: task.operation.Path,
			Old: task.operation.Old, New: task.operation.New,
			All: flag(task.operation.All), ExpectedSHA: task.operation.ExpectedSHA,
		})
		if err == nil {
			result["replaced"], result["sha256"], result["size_bytes"] = edit.Replaced, edit.SHA256, edit.SizeBytes
		}
	}
	if err != nil {
		state = h.recordFailure(task.operation.Tool, err, result)
		return
	}
	result["ok"] = true
	state = "done"
}

// recordFailure turns one operation's error into the result a model reads and
// the state the ledger keeps. It is a method of its own so that each
// classification can be tested without a device on the other end, which is the
// only way to exercise the ones a healthy device never produces.
func (h *Handler) recordFailure(tool string, err error, result map[string]any) string {
	result["error"] = boundedText(err.Error(), 4096)
	var rejected *client.RejectError
	var untrusted *client.TrustRequiredError
	var refused *client.FileOpError
	var unsupported *client.UnsupportedError

	// An UnsupportedError is not only a device saying it has never heard of this
	// verb. internal/client raises the same error when the session simply ends
	// after the request went out, and an edit committed a moment before the
	// connection dropped is then indistinguishable from one that never started.
	// A read pays nothing for that confusion — neither case wrote anything — but
	// for an edit it is the difference between "send it again" and "a file may
	// already have changed", so an edit keeps the cause-neutral unknown until
	// the client can tell the two apart.
	tooOld := errors.As(err, &unsupported) && tool != "edit_text"
	// The device answered and said what it did. Anything else is a lost outcome,
	// and for a tool that can change the device that is unknown.
	decided := errors.As(err, &rejected) || errors.As(err, &untrusted) ||
		errors.As(err, &refused) || tooOld

	state := "failed"
	if !decided && tool != "read_text" {
		state = "unknown"
		result["instruction"] = unknownInstruction
	}
	if errors.As(err, &unsupported) && !tooOld {
		// The client's own sentence names a cause it cannot know. Replace it
		// rather than let a model read "run wanctl update" as "nothing ran".
		result["error"] = "the device session ended without an answer to this edit. An agent too old to know edit_text ends it that way, and so does an edit that was applied just before its reply was lost; the adapter cannot tell those apart."
	}
	if errors.As(err, &refused) {
		// A refused read or edit did not replace the target file, and the
		// caller's way out is a corrected operation — which is a different
		// operation, so it needs a new rid. Saying only "failed" would strand
		// it: the rules forbid reusing a rid with new arguments and forbid a new
		// rid after a failure whose effect is unknown. This one is not unknown.
		result["error_code"], result["execution_started"] = "file_refused", false
		if r := refused.Result; r != nil {
			if r.SHA256 != "" {
				result["sha256"] = r.SHA256
			}
			if r.SizeBytes != 0 {
				result["size_bytes"] = r.SizeBytes
			}
			if r.Occurrences != 0 {
				result["occurrences"] = r.Occurrences
			}
		}
		result["instruction"] = refusalInstruction(tool)
	}
	if tooOld {
		result["error_code"], result["execution_started"] = "device_agent_too_old", false
		result["instruction"] = "Nothing ran. This device's wanctl agent predates read_text. Ask the human to run `wanctl update` on the device; until they have, read the file with exec instead."
	}
	if errors.As(err, &rejected) {
		result["error"] = boundedText(rejected.Reason, 4096)
		if len(rejected.PairingURL) <= 2048 && h.ownerLink(rejected.PairingURL) {
			result["pairing_url"] = rejected.PairingURL
			result["error_code"] = "pairing_required"
			result["execution_started"] = false
			result["human_checkpoint"] = humanCheckpoints()[1]
			result["instruction"] = "Step 2 of 2: pairing. Nothing ran. Show the human pairing_url with human_checkpoint.summary (summary_zh in Chinese), and wait until they say they approved it — never fetch that link yourself. Then submit the same operation again under a NEW rid. This job stays failed, and reusing its rid only returns this same failure."
		}
	}
	return state
}

func value(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func flag(b *bool) bool { return b != nil && *b }

// refusalInstruction tells the caller what a device-side refusal leaves behind
// and how to get out of it. Both tools left the file alone; only an edit has a
// correction to make.
func refusalInstruction(tool string) string {
	if tool == "edit_text" {
		return "The device refused this edit; the target file was not replaced and still holds exactly the text it had. Read the error: 'not found' means your old text does not appear, so read the file again rather than guessing at whitespace; 'occurs N times' means add surrounding lines to old, or pass all=true if you really mean every occurrence; 'changed since it was read' carries the file's current sha256, so read it again and redo the edit against that text. Then send the corrected edit under a NEW rid. Do not fall back to exec or write_text."
	}
	return "The device refused this read; no file was touched. A file that is not UTF-8 text cannot be read with this tool at all — do not retry it here. Correct the path or the line range and read again under a NEW rid."
}

// fitRead cuts a device read down to what one WebFetch response may carry. The
// device already returns whole lines up to its own larger cap, so the only work
// left is the outer 32 KiB bound: cut it on a line boundary and say where to
// continue, or, for a single line that cannot fit at all, say that paging on
// would never make progress.
func fitRead(res *client.ReadResult, out map[string]any) {
	content, last, truncated, longLine := res.Content, res.LastLine, res.Truncated, res.LongLine
	if len(content) > MaxOutputBytes {
		if cut := strings.LastIndexByte(content[:MaxOutputBytes], '\n'); cut >= 0 {
			content = content[:cut+1]
			last = res.FirstLine + strings.Count(content, "\n") - 1
			truncated, longLine = true, 0
		} else {
			content = string(trimPartialRune([]byte(content[:MaxOutputBytes])))
			last, longLine, truncated = res.FirstLine, res.FirstLine, true
		}
	}
	out["content"], out["total_lines"] = content, res.TotalLines
	out["first_line"], out["last_line"] = res.FirstLine, last
	out["size_bytes"], out["sha256"], out["truncated"] = res.SizeBytes, res.SHA256, truncated
	switch {
	case longLine != 0:
		out["long_line"] = longLine
		out["instruction"] = "Line " + strconv.Itoa(longLine) + " does not fit in one response on its own, so only its beginning is above. Do NOT ask for this line again — the same prefix would come back every time. Read it with exec (sed/cut) instead."
	case truncated:
		out["next_offset"] = last + 1
		out["instruction"] = "Cut at the response cap after a whole number of lines. Continue with offset=" + strconv.Itoa(last+1) + " under a NEW rid; nothing is lost and nothing repeats."
	}
}

// trimPartialRune drops up to three trailing bytes of a buffer cut at an
// arbitrary offset, so a multi-byte character split by the cut does not leave
// invalid UTF-8 in the response.
func trimPartialRune(b []byte) []byte {
	for range 3 {
		if len(b) == 0 || utf8.Valid(b) {
			break
		}
		b = b[:len(b)-1]
	}
	return b
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

// conflict answers a rid that already names a different operation. It carries
// no execution_started and no invitation to pick a new rid: the earlier
// operation under this rid may well have run, and a model that reads "nothing
// ran" here will happily run the work twice.
func (h *Handler) conflict(w http.ResponseWriter, r *http.Request, ticket, grant string) {
	log.Printf("webfetch: call rejected grant=%s reason=rid_conflict", grant)
	h.respond(w, r, 409, map[string]any{
		"error":      "this rid already names a different operation; its arguments do not match the ones recorded",
		"error_code": "rid_conflict", "status_url": h.statusURL(ticket),
		"instruction": "Stop here. Nothing ran just now, but the earlier operation recorded under this rid may have. Read status_url and find out what it did before you decide anything else. Do not resend this URL, and do not move the same work to another rid until you know.",
	})
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
{{with .Document.summary}}<p>{{.}}</p>{{end}}
{{with .Document.procedure}}<h2>Procedure</h2><ol>{{range .}}<li>{{.}}</li>{{end}}</ol>{{end}}
{{with .Document.human_checkpoints}}<h2>The two things the human does</h2><ul>{{range .}}<li><strong>{{.name}}</strong> — {{.summary}} (link: <code>{{.url_field}}</code>)</li>{{end}}</ul>{{end}}
{{with .Document.human_checkpoint}}<h2>{{.name}}</h2><p>{{.summary}}</p><p lang="zh-CN">{{.summary_zh}}</p>{{end}}
{{if .Document.call_endpoint}}
<h2>Approved: call tools with GET</h2>
<p><strong>call_endpoint</strong><br><code>{{.Document.call_endpoint}}</code></p>
<p><strong>Allowed target values</strong>{{range .Document.devices}}<br><code>{{.Target}}</code>{{end}}</p>
{{range .Document.tools}}<p><strong>{{.name}} — GET call_url_template</strong><br><code>{{.call_url_template}}</code><br>{{.description}}</p>{{end}}
<p>Fill every {placeholder} with a URL-encoded value; never fetch an unfilled template. A new rid per new operation, the same rid and arguments to retry one. Follow next_url until the status is done, failed or unknown, waiting poll_after_seconds between reads. Report the actual result, and keep status_url and the exec template in your reply.</p>
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
	for _, key := range []string{"help_url", "skill_url", "entry_url", "owner_start_url", "approval_url", "status_url", "next_url", "result_url"} {
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
