package relay

import "strings"

// TokenStore resolves an access/registration token to its namespace.
type TokenStore interface {
	Resolve(token string) (namespace string, ok bool)
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
	ns, ok := m[token]
	return ns, ok
}
