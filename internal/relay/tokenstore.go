package relay

import (
	"strings"
	"time"

	"wanctl/internal/delegation"
)

// TokenStore resolves an access/registration token to its namespace.
type TokenStore interface {
	Resolve(token string) (namespace string, ok bool)
}

// restrictedToken reports a credential kind that carries limits a namespace-only
// lookup cannot express: a WebFetch delegation or a scoped command token. Every
// legacy path that maps a token to a bare namespace refuses these by prefix.
func restrictedToken(token string) bool {
	return strings.HasPrefix(token, "wfd_") || strings.HasPrefix(token, delegation.ScopedTokenPrefix)
}

// ResolveAccess preserves device scope when a store supports delegation. The
// legacy Resolve method must never resolve delegated credentials: callers of
// that method are agent and account-management endpoints.
func ResolveAccess(store TokenStore, token string) (delegation.Access, bool) {
	if scoped, ok := store.(interface {
		ResolveAccess(string) (delegation.Access, bool)
	}); ok {
		a, valid := scoped.ResolveAccess(token)
		if !valid || a.Namespace == "" {
			return delegation.Access{}, false
		}
		if restrictedToken(token) && !a.Delegated {
			return delegation.Access{}, false
		}
		// A scoped command token and its prefix are the same fact: one never
		// appears without the other, whichever store vouched for it.
		if (a.ExecOnly != "") != strings.HasPrefix(token, delegation.ScopedTokenPrefix) {
			return delegation.Access{}, false
		}
		if a.CredentialID == "" {
			a.CredentialID = HashToken(token)
		}
		// A scoped token is held by whichever controller the owner gave it to,
		// so it has no controller binding; every other delegation has one.
		if a.Delegated && (a.GrantID == "" || (a.ControllerFingerprint == "" && a.ExecOnly == "") || len(a.Devices) == 0 || a.ExpiresAt.IsZero() || !time.Now().Before(a.ExpiresAt)) {
			return delegation.Access{}, false
		}
		if a.ExecOnly != "" && (!a.Delegated || len(a.Devices) != 1) {
			return delegation.Access{}, false
		}
		return a, true
	}
	// A legacy store cannot express a restricted credential's limits.
	if restrictedToken(token) {
		return delegation.Access{}, false
	}
	ns, ok := store.Resolve(token)
	return delegation.Access{Namespace: ns, CredentialID: HashToken(token)}, ok
}

type envTokenStore map[string]string

// EnvTokenStore builds a static store from "token:ns,token:ns" (used for the
// foundation milestone; later replaced by a Postgres-backed implementation).
func EnvTokenStore(spec string) TokenStore {
	m := envTokenStore{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if i := strings.LastIndex(pair, ":"); i > 0 {
			m[pair[:i]] = pair[i+1:]
		}
	}
	return m
}

func (m envTokenStore) Resolve(token string) (string, bool) {
	if restrictedToken(token) {
		return "", false
	}
	ns, ok := m[token]
	return ns, ok
}
