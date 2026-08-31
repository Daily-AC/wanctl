package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/notify"
)

const agentEventDedupeTTL = 10 * time.Minute

type agentEventReport struct {
	ID     string    `json:"id"`
	Event  string    `json:"event"`
	TS     time.Time `json:"ts"`
	Detail string    `json:"detail,omitempty"`
	Peer   string    `json:"peer,omitempty"`
	Exit   *int      `json:"exit,omitempty"`
}

func (r *Relay) handleAgentNotifyPolicy(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	namespace, device, ok := r.authAgentInstance(w, req)
	if !ok {
		return
	}
	includeDetail := false
	if r.notifyStore != nil {
		deviceCfg, deviceFound, err := r.notifyStore.GetDeviceNotify(namespace, device)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		includeDetail = deviceFound && deviceCfg.Enabled && configured && cfg.IncludeDetail
	}
	writeJSON(w, map[string]bool{"include_detail": includeDetail})
}

func (r *Relay) handleAgentEvent(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	namespace, device, ok := r.authAgentInstance(w, req)
	if !ok {
		return
	}
	var report agentEventReport
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := validateAgentEvent(report); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.notifyEventDuplicate(namespace, device, report.ID, time.Now()) {
		writeJSON(w, map[string]bool{"accepted": true, "duplicate": true})
		return
	}
	r.emitReportedDeviceEvent(namespace, device, report)
	w.WriteHeader(http.StatusAccepted)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func validateAgentEvent(report agentEventReport) error {
	if len(report.ID) == 0 || len(report.ID) > 128 {
		return errors.New("event id required and must be at most 128 bytes")
	}
	switch report.Event {
	case "approval.pending", "exec.finished", "pairing.requested", "trust.changed":
	default:
		return fmt.Errorf("unsupported agent event %q", report.Event)
	}
	if len(report.Detail) > 64<<10 || len(report.Peer) > 8<<10 {
		return errors.New("event detail too large")
	}
	return nil
}

func (r *Relay) authAgentInstance(w http.ResponseWriter, req *http.Request) (namespace, device string, ok bool) {
	namespace, ok = r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	device = strings.TrimSpace(req.URL.Query().Get("device"))
	inst := strings.TrimSpace(req.URL.Query().Get("inst"))
	if device == "" || inst == "" {
		http.Error(w, "device and inst required", http.StatusBadRequest)
		return "", "", false
	}
	if !r.agentInstanceLive(namespace, device, inst) {
		http.Error(w, "event source does not match a live agent instance", http.StatusForbidden)
		return "", "", false
	}
	return namespace, device, true
}

func (r *Relay) agentInstanceLive(namespace, device, inst string) bool {
	key := namespace + "/" + device
	r.mu.Lock()
	ws := r.agents[key]
	wsLive := ws != nil && ws.inst == inst
	r.mu.Unlock()
	if wsLive {
		return true
	}
	r.hmu.Lock()
	httpAgent := r.hagents[key]
	httpLive := httpAgent != nil && httpAgent.inst == inst && time.Since(httpAgent.lastSeen) <= httpAgentTTL
	r.hmu.Unlock()
	return httpLive
}

func (r *Relay) notifyEventDuplicate(namespace, device, id string, now time.Time) bool {
	key := namespace + "/" + device + "/" + id
	r.notifyDedupeMu.Lock()
	defer r.notifyDedupeMu.Unlock()
	cutoff := now.Add(-agentEventDedupeTTL)
	for existing, seen := range r.notifyDedupe {
		if seen.Before(cutoff) {
			delete(r.notifyDedupe, existing)
		}
	}
	if seen, found := r.notifyDedupe[key]; found && seen.After(cutoff) {
		return true
	}
	r.notifyDedupe[key] = now
	return false
}

func (r *Relay) emitReportedDeviceEvent(namespace, device string, report agentEventReport) {
	if r.notifyStore == nil || r.notifySend == nil {
		return
	}
	go func() {
		deviceCfg, found, err := r.notifyStore.GetDeviceNotify(namespace, device)
		if err != nil || !found || !deviceCfg.Enabled {
			return
		}
		cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
		if err != nil || !configured {
			return
		}
		event := reportedEvent(device, report, cfg.IncludeDetail)
		event.Namespace = namespace
		if !notifyEventEnabled(cfg, event) {
			return
		}
		if _, err := r.deliver(context.Background(), cfg, device, event); err != nil {
			logDeliveryFailure(cfg, event, namespace, device, err)
		}
	}()
}

func reportedEvent(device string, report agentEventReport, includeDetail bool) notify.Event {
	ts := report.TS
	if ts.IsZero() || time.Since(ts) > 24*time.Hour || time.Until(ts) > 5*time.Minute {
		ts = time.Now().UTC()
	}
	event := notify.Event{Event: report.Event, Device: device, TS: ts, Exit: report.Exit}
	switch report.Event {
	case "approval.pending":
		event.Message = device + " has a command waiting for approval"
	case "exec.finished":
		event.Message = device + " command finished"
		if report.Exit != nil {
			event.Message += fmt.Sprintf(" with exit status %d", *report.Exit)
		}
	case "pairing.requested":
		event.Message = device + " received a controller pairing request"
	case "trust.changed":
		event.Message = "controller trust changed on " + device
	}
	if includeDetail {
		event.Detail = report.Detail
		event.Peer = report.Peer
	}
	return event
}

func logDeliveryFailure(cfg NotifyWebhook, event notify.Event, namespace, device string, err error) {
	log.Printf("relay: webhook %s for %s/%s failed: %s", event.Event, namespace, device, deliveryErrorSummary(cfg, err))
}
