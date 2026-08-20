package portal

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Friends: the portal surface over the relay's friendship store. Sharing is
// friend-gated, so this is also where user discovery got closed down — the
// portal no longer exposes the full namespace list, only accepted friends
// (see handleNamespaces) plus an exact-match lookup for sending requests.

func (s *Server) handleFriendsList(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	s.proxyGet(w, ns, "/admin/friends")
}

// friendAction forwards one friendship mutation. The SPA posts the peer as
// {"namespace": ...}; the relay admin mirror wants {namespace: subject, peer}.
func (s *Server) friendAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns, ok := s.requireNS(w, r)
		if !ok {
			return
		}
		var in struct {
			Namespace string `json:"namespace"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		peer := strings.TrimSpace(in.Namespace)
		if peer == "" {
			http.Error(w, "empty namespace", http.StatusBadRequest)
			return
		}
		resp, err := s.adminReq("POST", "/admin/friends/"+action, nil,
			map[string]string{"namespace": ns, "peer": peer})
		if err != nil {
			http.Error(w, "relay unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		copyResp(w, resp)
	}
}

func (s *Server) handleUserLookup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireNS(w, r); !ok {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if target == "" {
		http.Error(w, "empty namespace", http.StatusBadRequest)
		return
	}
	resp, err := s.adminReq("GET", "/admin/users/lookup", url.Values{"namespace": {target}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}
