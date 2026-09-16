package relay

import (
	"net/http"
	"sync"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/delegation"
	"wanctl/internal/sessionauth"
)

const delegationRecheckInterval = time.Second

func (r *Relay) authAccess(w http.ResponseWriter, req *http.Request) (delegation.Access, string, bool) {
	token, legacy, ok := admission.Token(req)
	if !ok {
		return delegation.Access{}, "", false
	}
	if legacy {
		admission.MarkLegacy(w)
	}
	a, ok := ResolveAccess(r.ts, token)
	if a.Delegated && a.Namespace == r.portalNS {
		return delegation.Access{}, "", false
	}
	return a, token, ok
}

func (r *Relay) dialAccessAllowed(a delegation.Access, target string) (string, sessionauth.Open, string, bool) {
	key, auth, reason, ok := r.dialAllowedReason(a.Namespace, target)
	if !ok {
		// Account-level resolution diagnostics can enumerate shared devices
		// outside this grant. A delegated caller receives only a generic denial.
		if a.Delegated {
			return "", sessionauth.Open{}, "", false
		}
		return key, auth, reason, false
	}
	if a.Delegated {
		if !a.Allows(key) {
			return "", sessionauth.Open{}, "", false
		}
		auth.Capabilities = sessionauth.UseCapabilities
		auth.GrantID = a.GrantID
		auth.CredentialID = a.CredentialID
		auth.ControllerFingerprint = a.ControllerFingerprint
		auth.ExpiresAt = a.ExpiresAt
	}
	return key, auth, "", true
}

func (r *Relay) accessPeers(a delegation.Access) map[string]any {
	devices, aliases := r.livePeers(a.Namespace)
	shared := r.sharedPeers(a.Namespace)
	if a.Delegated {
		filtered := make([]string, 0, len(devices))
		for _, d := range devices {
			if a.Allows(a.Namespace + "/" + d) {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
		for d := range aliases {
			if !a.Allows(a.Namespace + "/" + d) {
				delete(aliases, d)
			}
		}
		filteredShared := make([]SharedPeer, 0, len(shared))
		for _, d := range shared {
			if a.Allows(d.Target) {
				filteredShared = append(filteredShared, d)
			}
		}
		shared = filteredShared
	}
	return peersBody(a.Namespace, devices, aliases, shared)
}

// accessLease ties all carrier legs to the exact controller credential. Raw
// credentials live only in relay memory and are never forwarded to the agent.
type accessLease struct {
	r                  *Relay
	sid, target, token string
	access             delegation.Access
	done               chan struct{}
	once               sync.Once
	mu                 sync.Mutex
	closers            []func()
}

func (r *Relay) beginAccessLease(sid, target string, access delegation.Access, token string) *accessLease {
	l := &accessLease{r: r, sid: sid, target: target, token: token, access: access, done: make(chan struct{})}
	if !access.Delegated {
		return l
	}
	r.leaseMu.Lock()
	if r.leases == nil {
		r.leases = make(map[string]*accessLease)
	}
	r.leases[sid] = l
	r.leaseMu.Unlock()
	// Expiry must not wait for a slow database or upstream revalidation.
	deadline := time.AfterFunc(time.Until(access.ExpiresAt), l.close)
	go func() {
		defer deadline.Stop()
		ticker := time.NewTicker(delegationRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-l.done:
				return
			case <-ticker.C:
				if !l.valid() {
					l.close()
					return
				}
			}
		}
	}()
	return l
}

func (l *accessLease) valid() bool {
	select {
	case <-l.done:
		return false
	default:
	}
	if !l.credentialValid() {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

// A normal EOF may still leave final output in the HTTP queue. Revalidate its
// credential without discarding those bytes merely because the peer closed.
func (l *accessLease) credentialValid() bool {
	if !l.access.Delegated {
		return true
	}
	a, ok := ResolveAccess(l.r.ts, l.token)
	return ok && a.Delegated && a.GrantID == l.access.GrantID &&
		a.CredentialID == l.access.CredentialID && a.ControllerFingerprint == l.access.ControllerFingerprint && a.Allows(l.target)
}

func (l *accessLease) addCloser(f func()) {
	l.mu.Lock()
	select {
	case <-l.done:
		l.mu.Unlock()
		f()
	default:
		l.closers = append(l.closers, f)
		l.mu.Unlock()
	}
}

func (l *accessLease) close() {
	l.once.Do(func() {
		l.mu.Lock()
		close(l.done)
		closers := l.closers
		l.closers = nil
		l.mu.Unlock()
		if l.access.Delegated {
			l.r.leaseMu.Lock()
			if l.r.leases[l.sid] == l {
				delete(l.r.leases, l.sid)
			}
			l.r.leaseMu.Unlock()
		}
		for _, f := range closers {
			f()
		}
	})
}

// An agent revalidates after a potentially long policy approval. Merely closing
// a socket is insufficient: approval UIs can resolve after the caller left.
func (r *Relay) handleAgentDelegationCheck(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	ns, device, ok := r.authAgentInstance(w, req)
	if !ok {
		return
	}
	r.leaseMu.Lock()
	l := r.leases[req.URL.Query().Get("session")]
	r.leaseMu.Unlock()
	if l == nil || l.target != ns+"/"+device || l.access.GrantID != req.URL.Query().Get("grant") ||
		l.access.ControllerFingerprint != req.URL.Query().Get("controller_fp") || !l.valid() {
		http.Error(w, "delegation inactive", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
