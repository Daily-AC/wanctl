package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ApprovalPhone is a namespace's designated approval phone (ADR 0015). Device
// is the device's route name, the same key the portal dials it by.
type ApprovalPhone struct {
	Namespace string    `json:"namespace,omitempty"`
	Device    string    `json:"device"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ApprovalPhoneStore persists the designation. It is optional in the same way
// DeviceAliasStore is, so test doubles of AdminStore need not grow.
type ApprovalPhoneStore interface {
	ApprovalPhone(namespace string) (ApprovalPhone, error)
	ListApprovalPhones() ([]ApprovalPhone, error)
	SetApprovalPhone(namespace, device string) (ApprovalPhone, error)
}

// ApprovalPhone returns the namespace's approval phone, or a zero value when
// none is set.
func (p *PGStore) ApprovalPhone(namespace string) (ApprovalPhone, error) {
	out := ApprovalPhone{Namespace: namespace}
	err := p.db.QueryRow(
		`SELECT device, updated_at FROM approval_phone WHERE namespace = $1`, namespace,
	).Scan(&out.Device, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalPhone{}, nil
	}
	return out, err
}

// ListApprovalPhones returns every designation, for the portal's reconcile
// loop: one query instead of one per namespace every 30 seconds.
func (p *PGStore) ListApprovalPhones() ([]ApprovalPhone, error) {
	rows, err := p.db.Query(`SELECT namespace, device, updated_at FROM approval_phone ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalPhone{}
	for rows.Next() {
		var ph ApprovalPhone
		if err := rows.Scan(&ph.Namespace, &ph.Device, &ph.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// SetApprovalPhone designates device, or clears the designation when device is
// empty. Only a device the namespace itself owns can be designated: a phone
// someone shared with you approves for its owner, not for you.
func (p *PGStore) SetApprovalPhone(namespace, device string) (ApprovalPhone, error) {
	if device == "" {
		_, err := p.db.Exec(`DELETE FROM approval_phone WHERE namespace = $1`, namespace)
		return ApprovalPhone{}, err
	}
	out := ApprovalPhone{Namespace: namespace}
	err := p.db.QueryRow(
		`INSERT INTO approval_phone (namespace, device)
		 SELECT owner_namespace, device_id FROM devices
		  WHERE owner_namespace = $1 AND device_id = $2
		 ON CONFLICT (namespace) DO UPDATE
		   SET device = EXCLUDED.device, updated_at = now()
		 RETURNING device, updated_at`, namespace, device,
	).Scan(&out.Device, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalPhone{}, ErrDeviceNotFound
	}
	return out, err
}

// adminApprovalPhone lists every designation (GET), reads one namespace's
// (GET ?namespace=) or sets it (POST {namespace, device}; an empty device
// clears it).
func (r *Relay) adminApprovalPhone(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	store, ok := r.admin.(ApprovalPhoneStore)
	if !ok {
		http.Error(w, "approval phones are not supported by the admin store", http.StatusServiceUnavailable)
		return
	}
	switch req.Method {
	case http.MethodGet:
		ns := req.URL.Query().Get("namespace")
		if ns == "" {
			out, err := store.ListApprovalPhones()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"phones": out})
			return
		}
		out, err := store.ApprovalPhone(ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	case http.MethodPost:
		var body struct {
			Namespace string `json:"namespace"`
			Device    string `json:"device"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Namespace == "" {
			http.Error(w, "namespace required", http.StatusBadRequest)
			return
		}
		out, err := store.SetApprovalPhone(body.Namespace, body.Device)
		switch {
		case errors.Is(err, ErrDeviceNotFound):
			writeErrorToken(w, http.StatusNotFound, ErrDeviceNotFound.Error())
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			writeJSON(w, out)
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
