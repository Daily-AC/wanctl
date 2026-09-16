package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"wanctl/internal/delegation"
)

// UpstreamTokenStore resolves tokens by asking another relay's admin API
// (POST /admin/tokens/resolve, gated by X-Admin-Secret). It lets a satellite
// relay (e.g. the intranet fast-path relay) accept the exact same tokens the
// portal issues, without sharing the Postgres DSN: the satellite has outbound
// internet access, the main relay owns the DB.
//
// Results are cached in memory: hits for hitTTL, misses for missTTL, so a
// steady exec stream costs one upstream round-trip per token per few minutes
// and a token revocation propagates within hitTTL.
type UpstreamTokenStore struct {
	url    string // upstream relay base URL, e.g. https://relay.example
	secret string
	hc     *http.Client

	mu    sync.Mutex
	cache map[string]upstreamEntry
}

type upstreamEntry struct {
	ns     string
	ok     bool
	expiry time.Time
}

const (
	upstreamHitTTL  = 5 * time.Minute
	upstreamMissTTL = 30 * time.Second
)

// NewUpstreamTokenStore builds a store resolving against upstreamURL with the
// shared admin secret.
func NewUpstreamTokenStore(upstreamURL, secret string) *UpstreamTokenStore {
	return &UpstreamTokenStore{
		url:    upstreamURL,
		secret: secret,
		hc:     &http.Client{Timeout: 5 * time.Second},
		cache:  map[string]upstreamEntry{},
	}
}

// Resolve implements TokenStore.
func (u *UpstreamTokenStore) Resolve(token string) (string, bool) {
	if strings.HasPrefix(token, "wfd_") {
		return "", false
	}
	u.mu.Lock()
	if e, hit := u.cache[token]; hit && time.Now().Before(e.expiry) {
		u.mu.Unlock()
		return e.ns, e.ok
	}
	u.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequest("POST", u.url+"/admin/tokens/resolve", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("X-Admin-Secret", u.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		// Upstream unreachable: fail closed but do NOT cache, so a blip doesn't
		// lock a valid token out for missTTL.
		return "", false
	}
	defer resp.Body.Close()

	ns, ok := "", false
	if resp.StatusCode == http.StatusOK {
		var out struct{ Namespace string }
		if json.NewDecoder(resp.Body).Decode(&out) == nil && out.Namespace != "" {
			ns, ok = out.Namespace, true
		}
	} else if resp.StatusCode != http.StatusNotFound {
		// 403 (bad secret) or 5xx: fail closed without caching.
		return "", false
	}

	ttl := upstreamMissTTL
	if ok {
		ttl = upstreamHitTTL
	}
	u.mu.Lock()
	u.cache[token] = upstreamEntry{ns: ns, ok: ok, expiry: time.Now().Add(ttl)}
	u.mu.Unlock()
	return ns, ok
}

// ChainTokenStore tries each store in order and returns the first hit.
type ChainTokenStore []TokenStore

// Resolve implements TokenStore.
func (c ChainTokenStore) Resolve(token string) (string, bool) {
	for _, ts := range c {
		if ns, ok := ts.Resolve(token); ok {
			return ns, ok
		}
	}
	return "", false
}

func (c ChainTokenStore) ResolveAccess(token string) (delegation.Access, bool) {
	for _, ts := range c {
		if access, ok := ResolveAccess(ts, token); ok {
			return access, true
		}
	}
	return delegation.Access{}, false
}

// Delegated grants are deliberately not cached: revocation is rechecked by
// active-session leases, and a minutes-long cache would outlive the grant.
func (u *UpstreamTokenStore) ResolveAccess(token string) (delegation.Access, bool) {
	u.mu.Lock()
	if e, hit := u.cache[token]; hit && e.ok && time.Now().Before(e.expiry) {
		u.mu.Unlock()
		return delegation.Access{Namespace: e.ns, CredentialID: HashToken(token)}, true
	}
	u.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequest(http.MethodPost, u.url+"/admin/tokens/inspect", bytes.NewReader(body))
	if err != nil {
		return delegation.Access{}, false
	}
	req.Header.Set("X-Admin-Secret", u.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return delegation.Access{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if strings.HasPrefix(token, "wfd_") {
			return delegation.Access{}, false
		}
		// A pre-delegation upstream has no inspect route. Its legacy route is
		// permitted only for complete credentials; modern upstreams enforce it.
		ns, ok := u.Resolve(token)
		return delegation.Access{Namespace: ns, CredentialID: HashToken(token)}, ok
	}
	if resp.StatusCode != http.StatusOK {
		return delegation.Access{}, false
	}
	var a delegation.Access
	if json.NewDecoder(resp.Body).Decode(&a) != nil || a.Namespace == "" {
		return delegation.Access{}, false
	}
	if a.CredentialID == "" {
		a.CredentialID = HashToken(token)
	}
	if !a.Delegated {
		u.mu.Lock()
		u.cache[token] = upstreamEntry{ns: a.Namespace, ok: true, expiry: time.Now().Add(upstreamHitTTL)}
		u.mu.Unlock()
	}
	return a, true
}
