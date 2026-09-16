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
		if strings.HasPrefix(token, "wfd_") && !a.Delegated {
			return delegation.Access{}, false
		}
		if a.CredentialID == "" {
			a.CredentialID = HashToken(token)
		}
		if a.Delegated && (a.GrantID == "" || a.ControllerFingerprint == "" || len(a.Devices) == 0 || a.ExpiresAt.IsZero() || !time.Now().Before(a.ExpiresAt)) {
			return delegation.Access{}, false
		}
		return a, true
	}
	// A legacy store cannot express a WebFetch credential's restrictions.
	if strings.HasPrefix(token, "wfd_") {
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
	if strings.HasPrefix(token, "wfd_") {
		return "", false
	}
	ns, ok := m[token]
	return ns, ok
}
