package portal

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

type delegationDevice struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Shared      bool   `json:"shared"`
	Alias       string `json:"alias"`
	DisplayName string `json:"display_name"`
	Fingerprint string `json:"fingerprint"`
	Online      bool   `json:"online"`
}

func (d delegationDevice) Label() string {
	if d.Alias != "" {
		return d.Alias
	}
	if d.DisplayName != "" {
		return d.DisplayName
	}
	return d.Name
}

func validDelegationRequestID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Server) delegationRequest(w http.ResponseWriter, ns, id string) (delegation.Request, bool) {
	var out delegation.Request
	if !validDelegationRequestID(id) {
		http.Error(w, "invalid request id", http.StatusBadRequest)
		return out, false
	}
	resp, err := s.adminReq(http.MethodGet, "/admin/delegations/request", url.Values{"namespace": {ns}, "id": {id}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return out, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		copyResp(w, resp)
		return out, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		http.Error(w, "invalid relay response", http.StatusBadGateway)
		return out, false
	}
	// Pending requests have not selected an owner. Once decided, only that
	// owner can read the resulting grant through the human portal.
	if out.Namespace != "" && out.Namespace != ns {
		http.Error(w, "forbidden", http.StatusForbidden)
		return delegation.Request{}, false
	}
	return out, true
}

func (s *Server) delegationDevices(w http.ResponseWriter, ns string) ([]delegationDevice, bool) {
	resp, err := s.adminReq(http.MethodGet, "/admin/devices", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		copyResp(w, resp)
		return nil, false
	}
	var out struct {
		Devices []delegationDevice `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		http.Error(w, "invalid relay response", http.StatusBadGateway)
		return nil, false
	}
	owned := []delegationDevice{}
	for _, d := range out.Devices {
		if !d.Shared && (d.Owner == "" || d.Owner == ns) && transport.ValidFingerprint(d.Fingerprint) {
			owned = append(owned, d)
		}
	}
	return owned, true
}

func (s *Server) handleDelegationPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("request")
	if !validDelegationRequestID(id) {
		http.Error(w, "invalid request id", http.StatusBadRequest)
		return
	}
	next := "/webfetch/approve?" + url.Values{"request": {id}}.Encode()
	ns, ok := s.pageAuth(w, r, next)
	if !ok {
		return
	}
	req, ok := s.delegationRequest(w, ns, id)
	if !ok {
		return
	}
	pending := req.Status == "pending" && time.Now().Before(req.RequestExpiresAt)
	var devices []delegationDevice
	if pending {
		devices, ok = s.delegationDevices(w, ns)
		if !ok {
			return
		}
	}
	status := req.Status
	if req.Status == "pending" && !pending {
		status = "expired"
	}
	if req.Status == "approved" && req.ExpiresAt != nil && !time.Now().Before(*req.ExpiresAt) {
		status = "expired"
	}
	s.render(w, "delegation.html", map[string]any{"NS": ns, "Request": req, "Devices": devices, "Pending": pending, "Status": status})
}

func (s *Server) handleDelegationRequest(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	req, ok := s.delegationRequest(w, ns, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(req)
}

func (s *Server) handleDelegationApprove(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	var body struct {
		RequestID             string            `json:"request_id"`
		Devices               []string          `json:"devices"`
		Minutes               int               `json:"minutes"`
		Confirmed             bool              `json:"confirmed"`
		ControllerFingerprint string            `json:"controller_fingerprint"`
		DeviceFingerprints    map[string]string `json:"device_fingerprints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Confirmed || len(body.Devices) == 0 || len(body.Devices) > 16 || body.Minutes < 1 || body.Minutes > 60 {
		http.Error(w, "select 1 to 16 devices, confirm identities and choose 1 to 60 minutes", http.StatusBadRequest)
		return
	}
	req, ok := s.delegationRequest(w, ns, body.RequestID)
	if !ok {
		return
	}
	if req.Status != "pending" || !time.Now().Before(req.RequestExpiresAt) {
		http.Error(w, "request is no longer pending", http.StatusConflict)
		return
	}
	if req.ControllerFingerprint != body.ControllerFingerprint {
		http.Error(w, "controller identity changed; reload before confirming", http.StatusConflict)
		return
	}
	owned, ok := s.delegationDevices(w, ns)
	if !ok {
		return
	}
	fingerprints := map[string]string{}
	for _, d := range owned {
		fingerprints[d.Name] = d.Fingerprint
	}
	seen := map[string]bool{}
	for _, id := range body.Devices {
		if fingerprints[id] == "" || seen[id] {
			http.Error(w, "device not owned by you or selected twice", http.StatusForbidden)
			return
		}
		if body.DeviceFingerprints[id] != fingerprints[id] {
			http.Error(w, "device identity changed; reload before confirming", http.StatusConflict)
			return
		}
		seen[id] = true
	}
	resp, err := s.adminReq(http.MethodPost, "/admin/delegations/approve", nil, map[string]any{
		"namespace": ns, "request_id": body.RequestID, "devices": body.Devices, "minutes": body.Minutes,
		"controller_fingerprint": body.ControllerFingerprint, "device_fingerprints": body.DeviceFingerprints,
	})
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

func (s *Server) handleDelegationReject(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validDelegationRequestID(body.RequestID) {
		http.Error(w, "invalid request id", http.StatusBadRequest)
		return
	}
	resp, err := s.adminReq(http.MethodPost, "/admin/delegations/reject", nil, map[string]string{"namespace": ns, "request_id": body.RequestID})
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}
