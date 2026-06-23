// Package eventlog is the device-side structured activity log. The agent appends
// one JSON object per line to <config>/logs/events.jsonl so an operator (or a
// driving agent via `wanctl logs`) can trace what ran, who asked, and how it was
// decided. The relay never sees this — content lives only on the device.
package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wanctl/internal/transport"
)

// Event is one logged action.
type Event struct {
	Ts       time.Time `json:"ts"`
	Type     string    `json:"type"`               // connect | exec | file
	PeerFP   string    `json:"peer_fp,omitempty"`  // peer fingerprint
	PeerName string    `json:"peer_name,omitempty"`
	Detail   string    `json:"detail,omitempty"`   // command line or file path
	Cwd      string    `json:"cwd,omitempty"`
	Decision string    `json:"decision,omitempty"` // bypass | pre-approved | approved | remembered:* | denied
	Exit     *int      `json:"exit,omitempty"`     // exec exit code
	Bytes    int64     `json:"bytes,omitempty"`
}

// Filter narrows a Read.
type Filter struct {
	Since time.Time
	Type  string
	Grep  string
	Limit int // last N matching events (0 = all)
}

// Logger appends events to a JSONL file.
type Logger struct {
	mu   sync.Mutex
	path string
}

// Open creates (or opens) the named log inside <config>/logs/.
func Open(name string) (*Logger, error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return nil, err
	}
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}
	return &Logger{path: filepath.Join(logDir, name)}, nil
}

// Append writes one event (stamps Ts if zero). Best-effort: errors are returned
// but callers typically ignore them so logging never blocks an operation.
func (l *Logger) Append(e Event) error {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns events matching the filter (oldest first; Limit keeps the last N).
func (l *Logger) Read(f Filter) ([]Event, error) {
	l.mu.Lock()
	data, err := os.Open(l.path)
	l.mu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer data.Close()

	var out []Event
	sc := bufio.NewScanner(data)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if !f.Since.IsZero() && e.Ts.Before(f.Since) {
			continue
		}
		if f.Type != "" && e.Type != f.Type {
			continue
		}
		if f.Grep != "" && !strings.Contains(e.Detail, f.Grep) {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

// Path returns the underlying file path.
func (l *Logger) Path() string { return l.path }
