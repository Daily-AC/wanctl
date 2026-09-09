package portal

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Pair only the owner's installation, over its pinned console connection.
// Neither a destination host nor a shell command is accepted from the browser.
func (s *Server) handleDeviceADBPair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device string `json:"device"`
		Port   int    `json:"port"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ns, ok := s.requireDeviceConsole(w, r, body.Device)
	if !ok {
		return
	}
	if body.Port < 1 || body.Port > 65535 || len(body.Code) != 6 || strings.Trim(body.Code, "0123456789") != "" {
		http.Error(w, "enter the pairing port and six-digit code", http.StatusBadRequest)
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		s.connError(w, body.Device, err)
		return
	}
	state, err := d.state()
	if err != nil {
		s.connError(w, body.Device, err)
		return
	}
	if !state.Info.ADBPair {
		http.Error(w, "ADB pairing requires Android app v0.6.0 or later", http.StatusConflict)
		return
	}
	if err := d.pairADB(body.Port, body.Code); err != nil {
		http.Error(w, strings.ReplaceAll(err.Error(), body.Code, "[redacted]"), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"paired": true})
}
