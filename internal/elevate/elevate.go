// Package elevate runs a command with more privilege than the wanctl agent's
// own process has.
//
// This exists because of one number. The Android APK agent runs as an ordinary
// app — uid 10601, SELinux domain untrusted_app (ADR 0003, and it is deliberate)
// — while `adb shell` runs as uid 2000 in domain shell. Nearly the whole adb
// surface, `pm` and `am` and `input` and `screencap` and `dumpsys` and
// `settings`, lives on the far side of that line. No amount of work inside the
// app sandbox reaches it; v0.1.12's battery verb had to route around it through
// Java, which answers one question and does not generalize.
//
// Two mechanisms cross the line, and this package treats them as peers rather
// than as a preference list with a "real" one at the bottom, because each is
// unavailable on some devices and one of them (local adb) has an open proposal
// at Google to remove it. See ADR 0004.
//
//   - su   — a rooted device. Survives reboots.
//   - adb  — an adb client connecting to the device's own adbd over loopback,
//     after the user turned on wireless debugging.
//
// A third, Shizuku's binder, was planned and then cut on 2026-08-14: it reaches
// the same uid 2000 the adb channel already reaches, it is started by wireless
// debugging (so it needs everything the adb channel needs, plus itself), and it
// asks the device's owner to install and re-start a second app. See ADR 0004.
//
// Availability is probed, not assumed, and a caller that names a channel gets
// an error rather than a downgrade when that channel is missing: a command that
// ran unprivileged after the caller asked for root is a wrong answer wearing a
// right answer's clothes.
package elevate

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind identifies an elevation mechanism.
type Kind string

const (
	KindSu  Kind = "su"
	KindADB Kind = "adb"
)

// ProbeOrder is the order channels are tried when the caller does not name one.
// su first because it is the only one that survives a reboot unattended.
var ProbeOrder = []Kind{KindSu, KindADB}

// ParseKind validates a channel name from the wire or a command line.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(strings.TrimSpace(strings.ToLower(s))); k {
	case KindSu, KindADB:
		return k, nil
	case "shizuku":
		// Named in earlier plans and docs, so say what happened rather than
		// "unknown channel": someone typing this read something real.
		return "", fmt.Errorf("wanctl has no Shizuku channel (dropped 2026-08-14 — " +
			"it needs wireless debugging too, and then a second app on top; use --via adb)")
	case "":
		return "", fmt.Errorf("empty elevation channel")
	default:
		return "", fmt.Errorf("unknown elevation channel %q (want su or adb)", s)
	}
}

// Status is what a probe learned about one channel.
type Status struct {
	Kind      Kind `json:"kind"`
	Available bool `json:"available"`
	// Detail identifies what the channel actually got when available — the
	// output of `id`, so the audit trail records the uid that ran rather than
	// the uid we expected to run.
	Detail string `json:"detail,omitempty"`
	// Reason says why not, in terms of what the device's owner would have to
	// do about it. It is shown to the controller verbatim.
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// Channel is one way to run a command with elevated privilege.
type Channel interface {
	Kind() Kind
	// Probe reports whether this channel can run a command right now. It is
	// expected to be slow (su may raise a consent dialog the user has to tap)
	// and is cached by the Manager.
	Probe(ctx context.Context) Status
	// Run executes command, streaming merged stdout+stderr to out, and returns
	// its exit code. cwd may be empty.
	Run(ctx context.Context, command, cwd string, out io.Writer) (int, error)
	Close() error
}

// Manager probes channels, caches the result, and picks one.
type Manager struct {
	mu       sync.Mutex
	enabled  bool
	why      string // when disabled, what the owner would do to enable it
	channels []Channel
	cache    map[Kind]Status
	ttl      time.Duration
	now      func() time.Time
}

// DefaultProbeTTL is how long a probe result is trusted. Short enough that
// turning on wireless debugging takes effect without restarting the agent,
// long enough that a burst of commands does not re-probe per command (and, for
// su, does not raise a consent dialog per command).
const DefaultProbeTTL = 60 * time.Second

// NewManager builds a manager over the given channels. When enabled is false
// nothing is probed and every request fails with why — the APK's elevation
// switch is off by default and this is how that reaches the controller as an
// explanation instead of as a mysterious refusal.
func NewManager(enabled bool, why string, channels ...Channel) *Manager {
	return &Manager{
		enabled:  enabled,
		why:      why,
		channels: channels,
		cache:    map[Kind]Status{},
		ttl:      DefaultProbeTTL,
		now:      time.Now,
	}
}

// Enabled reports whether elevation is switched on for this device.
func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// SetEnabled flips the switch at runtime (the app writes it; the agent reads it
// on the next command rather than on restart).
func (m *Manager) SetEnabled(on bool, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled, m.why = on, why
	if !on {
		m.cache = map[Kind]Status{}
	}
}

// disabledErr is the single explanation for a device with elevation off.
func (m *Manager) disabledErr() error {
	if m.why != "" {
		return fmt.Errorf("elevation is disabled on this device: %s", m.why)
	}
	return fmt.Errorf("elevation is disabled on this device")
}

// probe returns a channel's status, using the cache when it is fresh.
func (m *Manager) probe(ctx context.Context, c Channel) Status {
	m.mu.Lock()
	st, ok := m.cache[c.Kind()]
	fresh := ok && m.now().Sub(st.CheckedAt) < m.ttl
	m.mu.Unlock()
	if fresh {
		return st
	}
	st = c.Probe(ctx)
	st.Kind = c.Kind()
	st.CheckedAt = m.now()
	m.mu.Lock()
	m.cache[c.Kind()] = st
	m.mu.Unlock()
	return st
}

// Select picks a channel. An empty via probes ProbeOrder and takes the first
// available one; a named via must be available or this fails, never silently
// falling back to a weaker channel or to no elevation at all.
func (m *Manager) Select(ctx context.Context, via Kind) (Channel, Status, error) {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled {
		return nil, Status{}, m.disabledErr()
	}
	if via != "" {
		for _, c := range m.channels {
			if c.Kind() != via {
				continue
			}
			st := m.probe(ctx, c)
			if !st.Available {
				return nil, st, fmt.Errorf("elevation channel %q is not available: %s", via, st.Reason)
			}
			return c, st, nil
		}
		return nil, Status{}, fmt.Errorf("elevation channel %q is not built into this agent", via)
	}

	var reasons []string
	for _, want := range ProbeOrder {
		for _, c := range m.channels {
			if c.Kind() != want {
				continue
			}
			st := m.probe(ctx, c)
			if st.Available {
				return c, st, nil
			}
			reasons = append(reasons, fmt.Sprintf("%s: %s", st.Kind, st.Reason))
		}
	}
	if len(reasons) == 0 {
		return nil, Status{}, fmt.Errorf("no elevation channels are built into this agent")
	}
	return nil, Status{}, fmt.Errorf("no elevation channel is available (%s)", strings.Join(reasons, "; "))
}

// Run selects a channel and executes command on it, returning the channel that
// ran so the caller can record it in the audit log.
func (m *Manager) Run(ctx context.Context, via Kind, command, cwd string, out io.Writer) (Kind, int, error) {
	c, _, err := m.Select(ctx, via)
	if err != nil {
		return "", -1, err
	}
	code, err := c.Run(ctx, command, cwd, out)
	return c.Kind(), code, err
}

// Statuses probes every channel and reports what it found, for `wanctl status`
// and the app's own display. Unlike Select it never errors: "nothing is
// available and here is why for each" is the useful answer.
func (m *Manager) Statuses(ctx context.Context) []Status {
	m.mu.Lock()
	enabled, why := m.enabled, m.why
	m.mu.Unlock()
	out := make([]Status, 0, len(m.channels))
	for _, c := range m.channels {
		if !enabled {
			out = append(out, Status{Kind: c.Kind(), Available: false, Reason: "elevation is switched off: " + why})
			continue
		}
		out = append(out, m.probe(ctx, c))
	}
	sort.Slice(out, func(i, j int) bool { return rank(out[i].Kind) < rank(out[j].Kind) })
	return out
}

func rank(k Kind) int {
	for i, o := range ProbeOrder {
		if o == k {
			return i
		}
	}
	return len(ProbeOrder)
}

// Close releases whatever the channels hold (adb keeps a live connection).
func (m *Manager) Close() error {
	var first error
	for _, c := range m.channels {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
