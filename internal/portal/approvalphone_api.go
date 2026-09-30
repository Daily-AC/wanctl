package portal

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// handleApprovalPhone reads (GET) or sets (POST {device}; an empty device
// withdraws it) the signed-in owner's approval phone (ADR 0015).
func (s *Server) handleApprovalPhone(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ns, ok := s.requireNS(w, r)
		if !ok {
			return
		}
		device, err := s.storedApprovalPhone(ns)
		if err != nil {
			http.Error(w, "relay unreachable", http.StatusBadGateway)
			return
		}
		online := false
		if phone := s.phoneSupervisor(); phone != nil && device != "" {
			if watched, up := phone.status(ns); watched == device {
				online = up
			}
		}
		writePortalJSON(w, map[string]any{"device": device, "online": online})
	case http.MethodPost:
		s.handleApprovalPhoneWrite(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleApprovalPhoneWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	var ns string
	if body.Device == "" {
		var ok bool
		if ns, ok = s.requireNS(w, r); !ok {
			return
		}
	} else {
		var ok bool
		if ns, ok = s.requireDeviceOwner(w, r, body.Device); !ok {
			return
		}
		phone := s.phoneSupervisor()
		if phone == nil {
			http.Error(w, "portal console not wired (set WANCTL_RELAY, WANCTL_PORTAL_TOKEN)", http.StatusServiceUnavailable)
			return
		}
		// The test card is both the owner's first look at a real
		// notification and the proof that this device can take approvals:
		// only an agent hosted by the Android app accepts one.
		if err := phone.testPush(r.Context(), ns, body.Device, body.Device); err != nil {
			if errors.Is(err, errPhoneIncapable) {
				http.Error(w, "这台设备收不了审批提醒：审批手机要装 wanctl 安卓 app，并更新到最新版。", http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, "设备没有响应：先在手机上打开 wanctl，等它显示已连接再设。", http.StatusConflict)
			return
		}
	}
	resp, err := s.adminReq(http.MethodPost, "/admin/approval-phone", nil, map[string]any{
		"namespace": ns, "device": body.Device,
	})
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if phone := s.phoneSupervisor(); resp.StatusCode >= 200 && resp.StatusCode < 300 && phone != nil {
		phone.triggerReconcile()
	}
	copyResp(w, resp)
}

func (s *Server) phoneSupervisor() *phoneSupervisor {
	s.larkMu.Lock()
	defer s.larkMu.Unlock()
	return s.phone
}

func (s *Server) storedApprovalPhone(ns string) (string, error) {
	resp, err := s.adminReq(http.MethodGet, "/admin/approval-phone", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("read approval phone", resp)
	}
	var out struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Device, nil
}

func writePortalJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
