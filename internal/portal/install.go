package portal

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"wanctl/internal/protocol"
	"wanctl/internal/release"
	"wanctl/internal/versionmeta"
)

const oldInstallAgent = "This device's wanctl is too old to be upgraded from the web. Run `wanctl update` on the device; if auto-update is on, it will also update itself."
const installDeadline = 10 * time.Minute

type deviceInstall struct {
	protocol.InstallProgress
	Started  time.Time `json:"started"`
	Accepted bool      `json:"-"`
}
type installStore struct {
	mu      sync.Mutex
	devices map[string]*deviceInstall
}

func (i *deviceInstall) active() bool { return i.Stage == "downloading" || i.Stage == "installing" }
func (i *deviceInstall) expire(now time.Time) {
	if i.active() && now.Sub(i.Started) >= installDeadline {
		i.Stage = "failed"
		i.Reason = "installation did not finish within 10 minutes; check wanctl logs on the device"
	}
}

func (s *Server) handleDeviceInstall(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	access, ok := s.requireDevice(w, r, device)
	if !ok {
		return
	}
	if access.Shared {
		http.Error(w, "only the device owner can install packages", http.StatusForbidden)
		return
	}
	key := access.Owner + "/" + device
	s.installs.mu.Lock()
	if s.installs.devices == nil {
		s.installs.devices = map[string]*deviceInstall{}
	}
	current := s.installs.devices[key]
	if current != nil {
		current.expire(time.Now())
		// Only a new online registration of the target proves a wanctl handover.
		if current.Accepted && current.active() && current.Package == "wanctl" && access.Online && access.WanctlVersion == current.Version {
			current.Stage = "done"
		}
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		if current == nil {
			w.Write([]byte("{}"))
		} else {
			json.NewEncoder(w).Encode(current)
		}
		s.installs.mu.Unlock()
		return
	}
	if current != nil && current.active() {
		s.installs.mu.Unlock()
		http.Error(w, "an install is already running", http.StatusConflict)
		return
	}
	if !access.Online {
		s.installs.mu.Unlock()
		http.Error(w, "device is offline", http.StatusConflict)
		return
	}
	var req struct {
		Package string `json:"package"`
		Version string `json:"version"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || (req.Package != "wanctl" && req.Package != "opencu") || (req.Package == "opencu" && req.Version != "latest") || (req.Package == "wanctl" && (len(req.Version) > 128 || release.ValidateVersion(req.Version) != nil)) {
		s.installs.mu.Unlock()
		http.Error(w, "choose wanctl with vX.Y.Z or vX.Y.Z-beta.N, or opencu with latest", http.StatusBadRequest)
		return
	}
	current = &deviceInstall{InstallProgress: protocol.InstallProgress{Package: req.Package, Version: req.Version, Stage: "downloading"}, Started: time.Now()}
	s.installs.devices[key] = current
	s.installs.mu.Unlock()
	update := func(p protocol.InstallProgress) {
		p = cleanInstallProgress(p)
		s.installs.mu.Lock()
		defer s.installs.mu.Unlock()
		if s.installs.devices[key] != current || !current.active() || p.Package != current.Package {
			return
		}
		current.expire(time.Now())
		if !current.active() {
			return
		}
		if p.Stage != "downloading" && p.Stage != "installing" && p.Stage != "done" && p.Stage != "failed" {
			return
		}
		if p.Package == "wanctl" {
			if p.Version != current.Version {
				return
			}
			if p.Stage == "done" {
				p.Stage = "installing"
			}
		}
		current.InstallProgress = p
	}
	dc, err := s.deviceConnFor(r.Context(), access.Owner, device)
	if err == nil {
		dc.notifMu.Lock()
		dc.installProgress = update
		dc.notifMu.Unlock()
		_, err = dc.rpc(protocol.Message{Kind: protocol.KindPackageInstall, Package: req.Package, Version: req.Version})
		if err == nil {
			s.installs.mu.Lock()
			current.Accepted = true
			s.installs.mu.Unlock()
		}
	}
	if err != nil {
		reason := err.Error()
		var refused *deviceRefusedError
		if errors.As(err, &refused) && refused.reason == "unknown RPC kind" {
			reason = oldInstallAgent
		}
		update(protocol.InstallProgress{Package: req.Package, Version: req.Version, Stage: "failed", Reason: reason})
	}
	s.installs.mu.Lock()
	defer s.installs.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(current)
}

// A device is not trusted to enforce the official helper's output limits.
// Copy at most 400 printable runes, so a short stored reason cannot retain a
// large backing string from the incoming protocol frame.
func cleanInstallProgress(p protocol.InstallProgress) protocol.InstallProgress {
	p.Version = versionmeta.Clean(p.Version)
	var b strings.Builder
	n := 0
	for _, r := range p.Reason {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
		n++
		if n == 400 {
			break
		}
	}
	p.Reason = strings.TrimSpace(b.String())
	return p
}
