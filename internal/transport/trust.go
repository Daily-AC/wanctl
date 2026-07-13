package transport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Peer is one remembered fingerprint, on either side of the relationship.
type Peer struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	Label       string    `json:"label,omitempty"` // controller self-description (who/why)
	Added       time.Time `json:"added"`
	LastSeen    time.Time `json:"last_seen"`
}

// Store is a JSON-backed set of trusted peers keyed by fingerprint. The server
// uses it as its client allow-list (known_clients.json); the client uses it to
// pin server identities (known_servers.json).
type Store struct {
	path    string
	mu      sync.Mutex
	m       map[string]Peer
	modTime time.Time
	size    int64
}

// NewMemStore returns a trust store that lives in memory only — never persists
// to disk. Used by the remote MCP server, where each user's known-servers set
// is per-session and we don't want sessions on the same process clobbering each
// other's known_servers.json.
func NewMemStore() *Store {
	return &Store{m: map[string]Peer{}}
}

// OpenStore loads (or initializes) the named store file inside the config dir.
func OpenStore(name string) (*Store, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, name), m: map[string]Peer{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.modTime = time.Time{}
			s.size = 0
			s.m = map[string]Peer{}
			return nil
		}
		return err
	}
	var peers []Peer
	if err := json.Unmarshal(data, &peers); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(s.path), err)
	}
	m := make(map[string]Peer, len(peers))
	for _, p := range peers {
		m[p.Fingerprint] = p
	}
	s.m = m
	if st, err := os.Stat(s.path); err == nil {
		s.modTime = st.ModTime()
		s.size = st.Size()
	}
	return nil
}

// Get returns the remembered peer for a fingerprint, if any.
func (s *Store) Get(fp string) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[fp]
	if !ok && s.reloadChangedLocked() {
		p, ok = s.m[fp]
	}
	return p, ok
}

// Has reports whether a fingerprint is trusted.
func (s *Store) Has(fp string) bool {
	_, ok := s.Get(fp)
	return ok
}

func (s *Store) reloadChangedLocked() bool {
	if s.path == "" {
		return false
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return false
	}
	if st.ModTime().Equal(s.modTime) && st.Size() == s.size {
		return false
	}
	return s.load() == nil
}

// GetByName returns the remembered peer with the given name, if any. Used by the
// client to pin a server's identity across IP changes.
func (s *Store) GetByName(name string) (Peer, bool) {
	if name == "" {
		return Peer{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.m {
		if p.Name == name {
			return p, true
		}
	}
	return Peer{}, false
}

// Add records a new trusted peer and persists the store.
func (s *Store) Add(fp, name string) error { return s.AddLabeled(fp, name, "") }

// AddLabeled records a new trusted peer with a self-described label.
func (s *Store) AddLabeled(fp, name, label string) error {
	s.mu.Lock()
	now := time.Now()
	s.m[fp] = Peer{Fingerprint: fp, Name: name, Label: label, Added: now, LastSeen: now}
	s.mu.Unlock()
	return s.save()
}

// Touch updates LastSeen for an existing peer (best-effort; ignores unknowns).
func (s *Store) Touch(fp string) {
	s.mu.Lock()
	if p, ok := s.m[fp]; ok {
		p.LastSeen = time.Now()
		s.m[fp] = p
	}
	s.mu.Unlock()
	_ = s.save()
}

// List returns all trusted peers.
func (s *Store) List() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.m))
	for _, p := range s.m {
		out = append(out, p)
	}
	return out
}

// Remove forgets a fingerprint.
func (s *Store) Remove(fp string) error {
	s.mu.Lock()
	delete(s.m, fp)
	s.mu.Unlock()
	return s.save()
}

func (s *Store) save() error {
	if s.path == "" {
		return nil // memory-only store (see NewMemStore)
	}
	s.mu.Lock()
	peers := make([]Peer, 0, len(s.m))
	for _, p := range s.m {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	data, err := json.MarshalIndent(peers, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.mu.Lock()
		s.modTime = st.ModTime()
		s.size = st.Size()
		s.mu.Unlock()
	}
	return nil
}
