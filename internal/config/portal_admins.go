package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

const portalAdminsVersion = 1

var ErrLastPortalAdmin = errors.New("refusing to remove the last portal admin fingerprint")

type portalAdminsFile struct {
	Version      int      `json:"version"`
	Fingerprints []string `json:"fingerprints"`
}

// PortalAdmins is the persisted set of controller fingerprints allowed to act
// as portal administrators. It is separate from general paired controllers so
// future console-admin authorization can require membership in this set.
type PortalAdmins struct {
	mu   sync.Mutex
	path string
	set  map[string]struct{}
}

func ParsePortalFingerprints(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		fp := strings.TrimSpace(part)
		if fp == "" {
			return nil, errors.New("empty portal fingerprint")
		}
		if !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("invalid portal fingerprint %q: want SHA256:<base64>", fp)
		}
		digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fp, "SHA256:"))
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("invalid portal fingerprint %q: want SHA256 of 32 bytes", fp)
		}
		if _, ok := seen[fp]; ok {
			continue
		}
		seen[fp] = struct{}{}
		out = append(out, fp)
	}
	return out, nil
}

func PortalFingerprintsEnv() string {
	if value := os.Getenv("WANCTL_PORTAL_FPS"); value != "" {
		return value
	}
	return os.Getenv("WANCTL_PORTAL_FP")
}

func OpenPortalAdmins() (*PortalAdmins, error) {
	path, err := fileIn("portal_admins.json")
	if err != nil {
		return nil, err
	}
	p := &PortalAdmins{path: path, set: map[string]struct{}{}}
	if err := p.loadLocked(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PortalAdmins) loadLocked() error {
	data, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		p.set = map[string]struct{}{}
		return nil
	}
	if err != nil {
		return err
	}
	var stored portalAdminsFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse portal_admins.json: %w", err)
	}
	if stored.Version != portalAdminsVersion {
		return fmt.Errorf("unsupported portal admin config version %d", stored.Version)
	}
	parsed, err := ParsePortalFingerprints(strings.Join(stored.Fingerprints, ","))
	if err != nil {
		return err
	}
	p.set = make(map[string]struct{}, len(parsed))
	for _, fp := range parsed {
		p.set[fp] = struct{}{}
	}
	return nil
}

func (p *PortalAdmins) List() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.loadLocked()
	out := make([]string, 0, len(p.set))
	for fp := range p.set {
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

func (p *PortalAdmins) Contains(fp string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.loadLocked()
	_, ok := p.set[fp]
	return ok
}

func (p *PortalAdmins) Add(fingerprints ...string) error {
	parsed, err := ParsePortalFingerprints(strings.Join(fingerprints, ","))
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.loadLocked(); err != nil {
		return err
	}
	next := cloneSet(p.set)
	for _, fp := range parsed {
		next[fp] = struct{}{}
	}
	return p.saveLocked(next)
}

func (p *PortalAdmins) Remove(fp string) error {
	parsed, err := ParsePortalFingerprints(fp)
	if err != nil {
		return err
	}
	if len(parsed) != 1 {
		return errors.New("exactly one portal fingerprint is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.loadLocked(); err != nil {
		return err
	}
	if _, ok := p.set[fp]; !ok {
		return nil
	}
	if len(p.set) == 1 {
		return ErrLastPortalAdmin
	}
	next := cloneSet(p.set)
	delete(next, fp)
	return p.saveLocked(next)
}

func cloneSet(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}

func (p *PortalAdmins) saveLocked(next map[string]struct{}) error {
	fingerprints := make([]string, 0, len(next))
	for fp := range next {
		fingerprints = append(fingerprints, fp)
	}
	sort.Strings(fingerprints)
	data, err := json.MarshalIndent(portalAdminsFile{Version: portalAdminsVersion, Fingerprints: fingerprints}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	p.set = next
	return nil
}
