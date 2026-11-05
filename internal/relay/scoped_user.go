package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The owner-side surface of scoped command tokens
// (docs/adr/0021-scoped-command-token.md). Authenticated with a full token via
// requireUserStore, which resolves through the namespace-only path and so
// refuses scoped and delegated credentials: a scoped token cannot mint another.

func (r *Relay) scopedStore(w http.ResponseWriter) (ScopedTokenStore, bool) {
	store, ok := r.admin.(ScopedTokenStore)
	if !ok {
		http.Error(w, "scoped tokens need the Postgres store", http.StatusServiceUnavailable)
	}
	return store, ok
}

func (r *Relay) userScopedTokens(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok {
		return
	}
	store, ok := r.scopedStore(w)
	if !ok {
		return
	}
	switch req.Method {
	case http.MethodGet:
		tokens, err := store.ListScopedTokens(namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"tokens": tokens})
	case http.MethodPost:
		var body struct {
			Device  string `json:"device"`
			Command string `json:"command"`
			Label   string `json:"label"`
			Days    int    `json:"days"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		body.Device, body.Label = strings.TrimSpace(body.Device), strings.TrimSpace(body.Label)
		if body.Device == "" || body.Command == "" {
			http.Error(w, "device and command required", http.StatusBadRequest)
			return
		}
		raw, info, err := store.IssueScopedToken(namespace, body.Device, body.Command, body.Label, body.Days)
		switch {
		case errors.Is(err, ErrScopedInvalid):
			http.Error(w, "invalid command, label or days (command: one printable line up to 4096 bytes; days: 1-365)", http.StatusBadRequest)
		case errors.Is(err, ErrScopedLimit):
			http.Error(w, "too many live scoped tokens; revoke one first", http.StatusConflict)
		case errors.Is(err, ErrDeviceNotFound):
			http.Error(w, "no such device of yours that can be scoped (it must be enrolled with a device id)", http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, map[string]any{"token": raw, "info": info})
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Relay) userScopedTokenRevoke(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	store, ok := r.scopedStore(w)
	if !ok {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ID <= 0 {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	revoked, err := store.RevokeScopedToken(namespace, body.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !revoked {
		writeErrorToken(w, http.StatusNotFound, "not-found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
