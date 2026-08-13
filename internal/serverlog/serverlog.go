// Package serverlog keeps a bounded, in-process copy of service logs.
package serverlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxLines  = 2000
	DefaultMaxBytes  = 2 << 20
	MaxResponseBytes = 16 << 20
	DefaultLimit     = 200
	MaxLimit         = 2000
	DefaultSince     = 15 * time.Minute
)

type entry struct {
	at   time.Time
	line string
}

// Buffer is an io.Writer that retains complete log lines within line and byte
// budgets. It is safe for concurrent writers and readers.
type Buffer struct {
	mu             sync.Mutex
	entries        []entry
	bytes          int
	maxLines       int
	maxBytes       int
	pending        []byte
	droppedThrough time.Time
	now            func() time.Time
}

func New(maxLines, maxBytes int) *Buffer {
	return &Buffer{maxLines: maxLines, maxBytes: maxBytes, now: time.Now}
}

func NewDefault() *Buffer { return New(DefaultMaxLines, DefaultMaxBytes) }

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, p...)
	if len(b.pending) > b.maxBytes {
		b.pending = append([]byte(nil), b.pending[len(b.pending)-b.maxBytes:]...)
	}
	for {
		i := bytes.IndexByte(b.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(b.pending[:i]), "\r")
		b.pending = b.pending[i+1:]
		b.appendLocked(b.now(), line)
	}
	return len(p), nil
}

// AppendAt adds one complete line. It is primarily useful when adapting log
// sources that already carry their own timestamp.
func (b *Buffer) AppendAt(at time.Time, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(at, strings.TrimRight(line, "\r\n"))
}

func (b *Buffer) appendLocked(at time.Time, line string) {
	if b.maxLines <= 0 || b.maxBytes <= 0 {
		return
	}
	if len(line) > b.maxBytes {
		line = line[len(line)-b.maxBytes:]
	}
	b.entries = append(b.entries, entry{at: at, line: line})
	b.bytes += len(line)
	for len(b.entries) > b.maxLines || b.bytes > b.maxBytes {
		old := b.entries[0]
		b.entries = b.entries[1:]
		b.bytes -= len(old.line)
		b.droppedThrough = old.at
	}
}

type Query struct {
	Service string
	Since   time.Duration
	Limit   int
	Grep    string
}

type Response struct {
	Service   string   `json:"service"`
	Logs      []string `json:"logs"`
	Truncated bool     `json:"truncated"`
}

func ParseQuery(values url.Values) (Query, error) {
	q := Query{Service: values.Get("service"), Since: DefaultSince, Limit: DefaultLimit, Grep: values.Get("grep")}
	if raw := values.Get("since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Query{}, fmt.Errorf("since must be a non-negative duration")
		}
		q.Since = d
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return Query{}, fmt.Errorf("limit must be a positive integer")
		}
		q.Limit = n
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	return q, nil
}

func (b *Buffer) Read(q Query) Response {
	if q.Limit < 1 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	cutoff := b.now().Add(-q.Since)
	b.mu.Lock()
	entries := append([]entry(nil), b.entries...)
	droppedThrough := b.droppedThrough
	b.mu.Unlock()

	lines := make([]string, 0, min(q.Limit, len(entries)))
	for _, e := range entries {
		if e.at.Before(cutoff) {
			continue
		}
		line := Redact(e.line)
		if q.Grep != "" && !strings.Contains(line, q.Grep) {
			continue
		}
		lines = append(lines, line)
	}
	truncated := !droppedThrough.IsZero() && !droppedThrough.Before(cutoff)
	if len(lines) > q.Limit {
		lines = lines[len(lines)-q.Limit:]
		truncated = true
	}
	return Response{Service: q.Service, Logs: lines, Truncated: truncated}
}

var (
	bearerRE = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]+`)
	keyRE    = regexp.MustCompile(`(?i)((?:token|secret|password|api[_-]?key)["']?[ \t]*[:=][ \t]*["']?)[^"' ,;\t]+`)
	base64RE = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{32,}={0,2}\b`)
)

// Redact removes credential-shaped fragments before any caller-supplied grep
// is evaluated, preventing grep from acting as a credential-presence oracle.
func Redact(line string) string {
	line = bearerRE.ReplaceAllString(line, "Bearer [REDACTED]")
	line = keyRE.ReplaceAllString(line, "${1}[REDACTED]")
	return base64RE.ReplaceAllString(line, "[REDACTED]")
}

func WriteJSON(w http.ResponseWriter, response Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// Fetch calls one service's secret-gated admin log endpoint.
func Fetch(ctx context.Context, client *http.Client, baseURL, secret string, q Query) (Response, error) {
	if baseURL == "" || secret == "" {
		return Response{}, fmt.Errorf("server logs require an admin URL and WANCTL_ADMIN_SECRET")
	}
	v := url.Values{"service": {q.Service}, "since": {q.Since.String()}, "limit": {strconv.Itoa(q.Limit)}}
	if q.Grep != "" {
		v.Set("grep", q.Grep)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/admin/logs?"+v.Encode(), nil)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("X-Admin-Secret", secret)
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Response{}, fmt.Errorf("server logs: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxResponseBytes)).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("decode server logs: %w", err)
	}
	return out, nil
}

func Format(w io.Writer, response Response) error {
	bw := bufio.NewWriter(w)
	for _, line := range response.Logs {
		if _, err := fmt.Fprintln(bw, line); err != nil {
			return err
		}
	}
	if response.Truncated {
		if _, err := fmt.Fprintln(bw, "[wanctl: results truncated]"); err != nil {
			return err
		}
	}
	return bw.Flush()
}
