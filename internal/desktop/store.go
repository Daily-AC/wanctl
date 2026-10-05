package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"wanctl/internal/protocol"
)

const SnapshotTTL = 2 * time.Minute
const snapshotsPerController = 16
const maxControllers = 64

type stored struct {
	snapshot protocol.DesktopSnapshot
	used     bool
}

// Store is owned by the resident agent. Even non-coordinate batches consume
// one snapshot, so eviction, duplicate delivery and restart can never make an
// old batch executable again. Failed batches consume their snapshot as well.
type Store struct {
	mu    sync.Mutex
	peers map[string][]stored
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (s *Store) Put(peer string, snap protocol.DesktopSnapshot, now time.Time) (protocol.DesktopSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers == nil {
		s.peers = map[string][]stored{}
	}
	for p, entries := range s.peers {
		var keep []stored
		for _, e := range entries {
			if now.Sub(time.UnixMilli(e.snapshot.CapturedAt)) < SnapshotTTL {
				keep = append(keep, e)
			}
		}
		if len(keep) == 0 {
			delete(s.peers, p)
		} else {
			s.peers[p] = keep
		}
	}
	if _, ok := s.peers[peer]; !ok && len(s.peers) >= maxControllers {
		return snap, errors.New("desktop snapshot capacity reached")
	}
	snap.ID = NewID()
	if snap.CapturedAt == 0 {
		snap.CapturedAt = now.UnixMilli()
	}
	entries := append(s.peers[peer], stored{snapshot: snap})
	if len(entries) > snapshotsPerController {
		entries = entries[len(entries)-snapshotsPerController:]
	}
	s.peers[peer] = entries
	return snap, nil
}
func (s *Store) Get(peer, id string, consume bool, now time.Time) (protocol.DesktopSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.peers[peer] {
		if e.snapshot.ID == id {
			age := now.Sub(time.UnixMilli(e.snapshot.CapturedAt))
			if age < 0 || age >= SnapshotTTL {
				return protocol.DesktopSnapshot{}, errors.New("stale screenshot_id; take a fresh screenshot")
			}
			if consume && e.used {
				return protocol.DesktopSnapshot{}, errors.New("state unknown: screenshot already consumed; duplicate act was not replayed")
			}
			if consume {
				s.peers[peer][i].used = true
			}
			return e.snapshot, nil
		}
	}
	return protocol.DesktopSnapshot{}, errors.New("unknown screenshot_id; take a fresh screenshot")
}
func Contains(r protocol.Rect, p protocol.Point) bool {
	return p.X >= r.X && p.Y >= r.Y && p.X-r.X < r.Width && p.Y-r.Y < r.Height
}
func MapPoint(s protocol.DesktopSnapshot, layout string, x, y int) (protocol.Point, error) {
	if s.Layout != layout {
		return protocol.Point{}, errors.New("display configuration or session changed; take a fresh screenshot")
	}
	if s.Width <= 0 || s.Height <= 0 || s.Source.Width <= 0 || s.Source.Height <= 0 || x < 0 || y < 0 || x >= s.Width || y >= s.Height {
		return protocol.Point{}, errors.New("coordinate outside screenshot")
	}
	// Round to nearest physical pixel, halves away from zero BEFORE adding the
	// possibly negative virtual-desktop origin. Clamp the far edge after rounding.
	p := protocol.Point{X: s.Source.X + min(s.Source.Width-1, int(math.Round(float64(x)*float64(s.Source.Width)/float64(s.Width)))), Y: s.Source.Y + min(s.Source.Height-1, int(math.Round(float64(y)*float64(s.Source.Height)/float64(s.Height))))}
	for _, m := range s.Monitors {
		if Contains(m.Rect, p) {
			return p, nil
		}
	}
	return protocol.Point{}, errors.New("coordinate lies in a gap between monitors")
}
func CropSource(s protocol.DesktopSnapshot, r protocol.Rect) (protocol.Rect, error) {
	if r.Width <= 0 || r.Height <= 0 || r.X < 0 || r.Y < 0 || r.X >= s.Width || r.Y >= s.Height || r.Width > s.Width-r.X || r.Height > s.Height-r.Y {
		return protocol.Rect{}, errors.New("region outside full screenshot")
	}
	// Endpoints use the same mapping rule, without monitor-gap rejection (a
	// crop can span two monitors and the empty space between them).
	x := int(math.Round(float64(r.X) * float64(s.Source.Width) / float64(s.Width)))
	y := int(math.Round(float64(r.Y) * float64(s.Source.Height) / float64(s.Height)))
	right := int(math.Round(float64(r.X+r.Width) * float64(s.Source.Width) / float64(s.Width)))
	bottom := int(math.Round(float64(r.Y+r.Height) * float64(s.Source.Height) / float64(s.Height)))
	return protocol.Rect{X: s.Source.X + x, Y: s.Source.Y + y, Width: right - x, Height: bottom - y}, nil
}
func FocusGuard(expected, current protocol.DesktopWindow) error {
	if expected.ID == "" || current.ID != expected.ID || current.PID != expected.PID {
		return errors.New("foreground window changed; stopped before sending input")
	}
	if !current.ElevationKnown {
		return errors.New("foreground window integrity is unknown; input refused")
	}
	if current.Elevated {
		return errors.New("foreground window is elevated; ask the person at the computer")
	}
	return nil
}
func TargetAt(s protocol.DesktopSnapshot, p protocol.Point) (protocol.DesktopWindow, error) {
	for _, w := range s.Windows {
		if Contains(w.Rect, p) {
			return w, nil
		}
	}
	return protocol.DesktopWindow{}, fmt.Errorf("no captured target window at physical coordinate (%d,%d)", p.X, p.Y)
}
