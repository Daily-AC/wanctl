package relay

import (
	"encoding/json"
	"errors"
	"net/http"

	"wanctl/internal/delegation"
)

func (r *Relay) registerDelegationAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/delegations/request", r.adminDelegationRequest)
	mux.HandleFunc("/admin/delegations/approve", r.adminDelegationApprove)
	mux.HandleFunc("/admin/delegations/reject", r.adminDelegationReject)
	mux.HandleFunc("/admin/tokens/inspect", r.adminTokenInspect)
}

func (r *Relay) delegationAdminStore(w http.ResponseWriter, req *http.Request) (delegation.Store, bool) {
	if !r.requireAdminStore(w, req) {
		return nil, false
	}
	store, ok := r.admin.(delegation.Store)
	if !ok {
		http.Error(w, "delegation store is not configured", http.StatusServiceUnavailable)
	}
	return store, ok
}

func writeDelegationError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, delegation.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, delegation.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, delegation.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, delegation.ErrExpired):
		status = http.StatusGone
	case errors.Is(err, delegation.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, delegation.ErrLimit):
		status = http.StatusTooManyRequests
	}
	// DB failures may contain SQL details; expose only stable contract errors.
	if status == http.StatusInternalServerError {
		http.Error(w, "delegation store unavailable", status)
		return
	}
	http.Error(w, err.Error(), status)
}

func (r *Relay) adminDelegationRequest(w http.ResponseWriter, req *http.Request) {
	store, ok := r.delegationAdminStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodGet) {
		return
	}
	ns := req.URL.Query().Get("namespace")
	if guardNamespace(ns, r.portalNS) != nil {
		writeDelegationError(w, delegation.ErrForbidden)
		return
	}
	out, err := store.GetDelegation(req.Context(), req.URL.Query().Get("id"))
	if err != nil {
		writeDelegationError(w, err)
		return
	}
	if out.Namespace != "" && out.Namespace != ns {
		writeDelegationError(w, delegation.ErrForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

func (r *Relay) adminDelegationApprove(w http.ResponseWriter, req *http.Request) {
	store, ok := r.delegationAdminStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var in delegation.Approval
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in) != nil {
		writeDelegationError(w, delegation.ErrInvalid)
		return
	}
	if guardNamespace(in.Namespace, r.portalNS) != nil {
		writeDelegationError(w, delegation.ErrForbidden)
		return
	}
	out, err := store.ApproveDelegation(req.Context(), in)
	if err != nil {
		writeDelegationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

func (r *Relay) adminDelegationReject(w http.ResponseWriter, req *http.Request) {
	store, ok := r.delegationAdminStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var in struct {
		RequestID string `json:"request_id"`
		Namespace string `json:"namespace"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in) != nil {
		writeDelegationError(w, delegation.ErrInvalid)
		return
	}
	if guardNamespace(in.Namespace, r.portalNS) != nil {
		writeDelegationError(w, delegation.ErrForbidden)
		return
	}
	if err := store.RejectDelegation(req.Context(), in.RequestID, in.Namespace); err != nil {
		writeDelegationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Satellite relays must retain admission metadata, never downgrade delegated
// credentials through the older namespace-only resolve endpoint.
func (r *Relay) adminTokenInspect(w http.ResponseWriter, req *http.Request) {
	if !r.secretOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in) != nil || in.Token == "" {
		writeDelegationError(w, delegation.ErrInvalid)
		return
	}
	out, ok := ResolveAccess(r.ts, in.Token)
	if !ok {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}
