package portal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

// Public, credential-free instructions must be readable by the AI's HTTP client,
// without the owner's portal session or a live relay request.
func (s *Server) handleWebFetchHelp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	s.render(w, "webfetch-help.html", map[string]any{"Relay": s.relayPublic})
}

func (s *Server) handleWebFetchConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := s.pageAuth(w, r, "/webfetch/connect")
	if !ok {
		return
	}
	// Consume the same public discovery document as other clients, through the
	// configured internal relay address. This also checks that WebFetch is on.
	resp, err := s.hc.Get(s.relayURL + "/webfetch/v1?format=json")
	if err != nil {
		s.renderStatus(w, http.StatusServiceUnavailable, "webfetch-connect.html", map[string]any{"NS": ns})
		return
	}
	defer resp.Body.Close()
	var entry struct {
		Protocol string `json:"protocol"`
		Template string `json:"start_url_template"`
	}
	const suffix = "/webfetch/new/{client_nonce}"
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&entry) != nil || entry.Protocol != "wanctl.webfetch.v1" || !strings.HasSuffix(entry.Template, suffix) {
		s.renderStatus(w, http.StatusServiceUnavailable, "webfetch-connect.html", map[string]any{"NS": ns})
		return
	}
	origin := strings.TrimSuffix(entry.Template, suffix)
	if relayPublicOrigin(origin) != origin {
		http.Error(w, "invalid WebFetch discovery origin", http.StatusBadGateway)
		return
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		http.Error(w, "cannot generate a fresh connection", http.StatusInternalServerError)
		return
	}
	// The authenticated owner gets a unique bootstrap URL, not a grant. The AI
	// still creates its request and waits for the owner's separate approval.
	startURL := origin + "/webfetch/new/" + hex.EncodeToString(nonce[:])
	s.render(w, "webfetch-connect.html", map[string]any{"NS": ns, "Relay": origin, "StartURL": startURL, "HelpURL": s.requestOrigin(r) + "/webfetch/help"})
}

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
	out, status, err := s.loadDelegationRequest(ns, id)
	if err != nil {
		http.Error(w, err.Error(), status)
		return delegation.Request{}, false
	}
	return out, true
}

func (s *Server) loadDelegationRequest(ns, id string) (delegation.Request, int, error) {
	var out delegation.Request
	if !validDelegationRequestID(id) {
		return out, http.StatusBadRequest, errors.New("invalid request id")
	}
	resp, err := s.adminReq(http.MethodGet, "/admin/delegations/request", url.Values{"namespace": {ns}, "id": {id}}, nil)
	if err != nil {
		return out, http.StatusBadGateway, errors.New("relay unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, resp.StatusCode, errors.New(strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, http.StatusBadGateway, errors.New("invalid relay response")
	}
	// Pending requests have not selected an owner. Once decided, only that
	// owner can read the resulting grant through the human portal.
	if out.Namespace != "" && out.Namespace != ns {
		return delegation.Request{}, http.StatusForbidden, errors.New("forbidden")
	}
	return out, http.StatusOK, nil
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
		s.renderStatus(w, http.StatusBadRequest, "delegation-error.html", map[string]any{"Status": http.StatusBadRequest})
		return
	}
	next := "/webfetch/approve?" + url.Values{"request": {id}}.Encode()
	ns, ok := s.pageAuth(w, r, next)
	if !ok {
		return
	}
	req, statusCode, err := s.loadDelegationRequest(ns, id)
	if err != nil {
		// Never render the other owner's grant or raw relay response here.
		s.renderStatus(w, statusCode, "delegation-error.html", map[string]any{"NS": ns, "Status": statusCode})
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
