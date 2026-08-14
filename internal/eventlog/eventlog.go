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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/transport"
)

// Event is one logged action.
type Event struct {
	Ts       time.Time `json:"ts"`
	Type     string    `json:"type"`              // connect | exec | file | logs
	PeerFP   string    `json:"peer_fp,omitempty"` // peer fingerprint
	PeerName string    `json:"peer_name,omitempty"`
	Detail   string    `json:"detail,omitempty"` // command line or file path
	Cwd      string    `json:"cwd,omitempty"`
	Decision string    `json:"decision,omitempty"` // bypass | pre-approved | approved | remembered:* | denied
	Exit     *int      `json:"exit,omitempty"`     // exec exit code
	Bytes    int64     `json:"bytes,omitempty"`
	// Via names the elevation channel that ran an elevated command (su,
	// adb). Present only on elevated execs, which is what makes
	// "what has run as root on this phone" a greppable question.
	Via string `json:"via,omitempty"`
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
	mu          sync.Mutex
	path        string
	maxBytes    int64
	backupCount int
}

const (
	defaultMaxBytes    = 1 << 20 // 1 MiB per JSONL segment
	defaultBackupCount = 3
	maxDetailBytes     = 64 << 10
	maxContextBytes    = 8 << 10
	truncatedMarker    = "...[TRUNCATED]"
)

var (
	secretFlagRE = regexp.MustCompile(`(?i)(--(?:token|password|passwd|secret|api[-_]?key|access[-_]?token|auth(?:orization)?)(?:[ \t]*=[ \t]*|[ \t]+))(?:(?:bearer|basic)[ \t]+[^\s'\"]+|"[^"\r\n]*"|'[^'\r\n]*'|[^\s]+)`)
	authHeaderRE = regexp.MustCompile(`(?i)(\bauthorization[ \t]*[:=][ \t]*)(?:(?:bearer|basic)[ \t]+)?[^\s'\";]+`)
	urlSecretRE  = regexp.MustCompile(`(?i)([?&](?:token|password|passwd|secret|api[-_]?key|access[-_]?token|authorization)=)[^&#\s'\"]+`)
	envSecretRE  = regexp.MustCompile(`(\b(?:WANCTL_TOKEN|TOKEN|PASSWORD|PASSWD|SECRET|API_KEY|ACCESS_TOKEN|AUTHORIZATION)[ \t]*=[ \t]*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s]+)`)
)

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
	return &Logger{
		path: filepath.Join(logDir, name), maxBytes: defaultMaxBytes,
		backupCount: defaultBackupCount,
	}, nil
}

// RedactText removes credential-shaped values while preserving enough context
// for audit logs and approval prompts.
func RedactText(s string) string {
	s = urlSecretRE.ReplaceAllString(s, `${1}[REDACTED]`)
	s = authHeaderRE.ReplaceAllString(s, `${1}[REDACTED]`)
	s = secretFlagRE.ReplaceAllString(s, `${1}[REDACTED]`)
	return envSecretRE.ReplaceAllString(s, `${1}[REDACTED]`)
}

func redactEvent(e Event) Event {
	e.PeerName = truncateText(RedactText(e.PeerName), maxContextBytes)
	e.Detail = truncateText(RedactText(e.Detail), maxDetailBytes)
	e.Cwd = truncateText(RedactText(e.Cwd), maxContextBytes)
	return e
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max - len(truncatedMarker)
	if keep < 0 {
		keep = 0
	}
	return strings.ToValidUTF8(s[:keep], "") + truncatedMarker
}

// Append writes one event (stamps Ts if zero). Best-effort: errors are returned
// but callers typically ignore them so logging never blocks an operation.
func (l *Logger) Append(e Event) error {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	e = redactEvent(e)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line := append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}

func (l *Logger) rotateIfNeeded(incoming int64) error {
	if l.maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() == 0 || info.Size()+incoming <= l.maxBytes {
		return nil
	}
	if l.backupCount <= 0 {
		return os.Remove(l.path)
	}
	if err := os.Remove(l.path + "." + strconv.Itoa(l.backupCount)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := l.backupCount - 1; i >= 1; i-- {
		src := l.path + "." + strconv.Itoa(i)
		dst := l.path + "." + strconv.Itoa(i+1)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(l.path, l.path+".1")
}

// Read returns events matching the filter (oldest first; Limit keeps the last N).
func (l *Logger) Read(f Filter) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Event
	paths := make([]string, 0, l.backupCount+1)
	for i := l.backupCount; i >= 1; i-- {
		paths = append(paths, l.path+"."+strconv.Itoa(i))
	}
	paths = append(paths, l.path)
	for _, path := range paths {
		data, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, err
		}
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
		scanErr := sc.Err()
		closeErr := data.Close()
		if scanErr != nil {
			return out, scanErr
		}
		if closeErr != nil {
			return out, closeErr
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

// Path returns the underlying file path.
func (l *Logger) Path() string { return l.path }
