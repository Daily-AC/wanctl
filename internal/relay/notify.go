package relay

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/notify"
)

// Empty cannot be a registered device name and is the health key for relay-only
// account events and test notifications.
const relayNotifyHealthDevice = ""

type publicNotifyWebhook struct {
	Configured       bool          `json:"configured"`
	URL              string        `json:"url,omitempty"`
	Format           string        `json:"format"`
	Keyword          string        `json:"keyword,omitempty"`
	SecretSet        bool          `json:"secret_set"`
	OnApproval       bool          `json:"on_approval"`
	OnExec           bool          `json:"on_exec"`
	OnLifecycle      bool          `json:"on_lifecycle"`
	OnSecurity       bool          `json:"on_security"`
	ExecFailuresOnly bool          `json:"exec_failures_only"`
	IncludeDetail    bool          `json:"include_detail"`
	UpdatedAt        time.Time     `json:"updated_at,omitempty"`
	Health           *NotifyHealth `json:"health,omitempty"`
}

func publicNotifyConfig(cfg NotifyWebhook, configured, fullURL bool, health *NotifyHealth) publicNotifyWebhook {
	url := cfg.URL
	if configured && !fullURL {
		url = notify.MaskURL(url)
	}
	return publicNotifyWebhook{
		Configured: configured, URL: url, Format: cfg.Format, Keyword: cfg.Keyword,
		SecretSet: cfg.Secret != "", OnApproval: cfg.OnApproval, OnExec: cfg.OnExec,
		OnLifecycle: cfg.OnLifecycle, OnSecurity: cfg.OnSecurity,
		ExecFailuresOnly: cfg.ExecFailuresOnly, IncludeDetail: cfg.IncludeDetail,
		UpdatedAt: cfg.UpdatedAt, Health: health,
	}
}

func (r *Relay) userNotify(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok {
		return
	}
	switch req.Method {
	case http.MethodGet:
		r.getNotifyWebhook(w, namespace)
	case http.MethodPost:
		r.writeNotifyWebhook(w, req, namespace)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Relay) userNotifyTest(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	r.testNotifyWebhook(req.Context(), w, namespace)
}

func (r *Relay) userDeviceNotify(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok {
		return
	}
	r.deviceNotify(w, req, namespace)
}

func (r *Relay) adminNotify(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	if req.Method == http.MethodGet {
		namespace := strings.TrimSpace(req.URL.Query().Get("namespace"))
		if namespace == "" {
			http.Error(w, "namespace required", http.StatusBadRequest)
			return
		}
		r.getNotifyWebhook(w, namespace)
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Namespace string `json:"namespace"`
	}
	data, err := io.ReadAll(req.Body)
	if err != nil || json.Unmarshal(data, &body) != nil || strings.TrimSpace(body.Namespace) == "" {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Body = io.NopCloser(strings.NewReader(string(data)))
	r.writeNotifyWebhook(w, req, strings.TrimSpace(body.Namespace))
}

func (r *Relay) adminNotifyTest(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || strings.TrimSpace(body.Namespace) == "" {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	r.testNotifyWebhook(req.Context(), w, strings.TrimSpace(body.Namespace))
}

func (r *Relay) adminDeviceNotify(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	namespace := strings.TrimSpace(req.URL.Query().Get("namespace"))
	if req.Method == http.MethodPost {
		var body struct {
			Namespace string `json:"namespace"`
		}
		data, err := io.ReadAll(req.Body)
		if err != nil || json.Unmarshal(data, &body) != nil || strings.TrimSpace(body.Namespace) == "" {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		namespace = strings.TrimSpace(body.Namespace)
		req.Body = io.NopCloser(strings.NewReader(string(data)))
	}
	if namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	r.deviceNotify(w, req, namespace)
}

func (r *Relay) getNotifyWebhook(w http.ResponseWriter, namespace string) {
	if r.notifyStore == nil {
		http.Error(w, "notification store is not configured", http.StatusServiceUnavailable)
		return
	}
	cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	health, found, err := r.notifyStore.GetNotifyHealth(namespace, relayNotifyHealthDevice)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var healthPtr *NotifyHealth
	if found {
		healthPtr = &health
	}
	writeJSON(w, publicNotifyConfig(cfg, configured, false, healthPtr))
}

type notifyWebhookUpdate struct {
	URL              *string `json:"url"`
	Format           *string `json:"format"`
	Keyword          *string `json:"keyword"`
	Secret           *string `json:"secret"`
	OnApproval       *bool   `json:"on_approval"`
	OnExec           *bool   `json:"on_exec"`
	OnLifecycle      *bool   `json:"on_lifecycle"`
	OnSecurity       *bool   `json:"on_security"`
	ExecFailuresOnly *bool   `json:"exec_failures_only"`
	IncludeDetail    *bool   `json:"include_detail"`
	Delete           bool    `json:"delete"`
}

func (r *Relay) writeNotifyWebhook(w http.ResponseWriter, req *http.Request, namespace string) {
	if r.notifyStore == nil {
		http.Error(w, "notification store is not configured", http.StatusServiceUnavailable)
		return
	}
	var body notifyWebhookUpdate
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Delete {
		if err := r.notifyStore.DeleteNotifyWebhook(namespace); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, publicNotifyConfig(defaultNotifyWebhook(namespace), false, false, nil))
		return
	}
	cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !configured {
		cfg = defaultNotifyWebhook(namespace)
	}
	applyNotifyUpdate(&cfg, body)
	if cfg.URL == "" {
		http.Error(w, "url required for a new webhook configuration", http.StatusBadRequest)
		return
	}
	dst := notify.Destination{URL: cfg.URL, Format: cfg.Format, Keyword: cfg.Keyword, Secret: cfg.Secret}
	if err := notify.ValidateDestination(dst); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err = r.notifyStore.UpsertNotifyWebhook(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, publicNotifyConfig(cfg, true, true, nil))
}

func applyNotifyUpdate(cfg *NotifyWebhook, body notifyWebhookUpdate) {
	if body.URL != nil {
		cfg.URL = strings.TrimSpace(*body.URL)
	}
	if body.Format != nil {
		cfg.Format = strings.TrimSpace(*body.Format)
	}
	if body.Keyword != nil {
		cfg.Keyword = strings.TrimSpace(*body.Keyword)
	}
	if body.Secret != nil {
		cfg.Secret = strings.TrimSpace(*body.Secret)
	}
	if body.OnApproval != nil {
		cfg.OnApproval = *body.OnApproval
	}
	if body.OnExec != nil {
		cfg.OnExec = *body.OnExec
	}
	if body.OnLifecycle != nil {
		cfg.OnLifecycle = *body.OnLifecycle
	}
	if body.OnSecurity != nil {
		cfg.OnSecurity = *body.OnSecurity
	}
	if body.ExecFailuresOnly != nil {
		cfg.ExecFailuresOnly = *body.ExecFailuresOnly
	}
	if body.IncludeDetail != nil {
		cfg.IncludeDetail = *body.IncludeDetail
	}
}

func (r *Relay) deviceNotify(w http.ResponseWriter, req *http.Request, namespace string) {
	if r.notifyStore == nil {
		http.Error(w, "notification store is not configured", http.StatusServiceUnavailable)
		return
	}
	switch req.Method {
	case http.MethodGet:
		device := strings.TrimSpace(req.URL.Query().Get("device"))
		if device == "" {
			http.Error(w, "device required", http.StatusBadRequest)
			return
		}
		cfg, _, err := r.notifyStore.GetDeviceNotify(namespace, device)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		health, found, err := r.notifyStore.GetNotifyHealth(namespace, device)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		response := struct {
			DeviceNotify
			Health *NotifyHealth `json:"health,omitempty"`
		}{DeviceNotify: cfg}
		if found {
			response.Health = &health
		}
		writeJSON(w, response)
	case http.MethodPost:
		var body struct {
			Device  string `json:"device"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || strings.TrimSpace(body.Device) == "" {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		cfg, err := r.notifyStore.UpsertDeviceNotify(DeviceNotify{
			Namespace: namespace, Device: strings.TrimSpace(body.Device), Enabled: body.Enabled,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, cfg)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Relay) testNotifyWebhook(ctx context.Context, w http.ResponseWriter, namespace string) {
	if r.notifyStore == nil || r.notifySend == nil {
		http.Error(w, "notification delivery is not configured", http.StatusServiceUnavailable)
		return
	}
	cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !configured {
		http.Error(w, "webhook is not configured", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	event := notify.Event{
		Event: "notify.test", Namespace: namespace, TS: time.Now().UTC(),
		Message: "wanctl test notification",
	}
	result, sendErr := r.deliver(ctx, cfg, relayNotifyHealthDevice, event)
	response := map[string]any{
		"http_status": result.HTTPStatus, "provider_code": result.ProviderCode,
		"provider_message": deliveryTextSummary(cfg, result.ProviderMessage), "attempts": result.Attempts,
	}
	if sendErr != nil {
		response["error"] = deliveryErrorSummary(cfg, sendErr)
	}
	writeJSON(w, response)
}

func (r *Relay) emitDeviceEvent(namespace, device string, event notify.Event) {
	if r.notifyStore == nil || r.notifySend == nil {
		return
	}
	go func() {
		deviceCfg, found, err := r.notifyStore.GetDeviceNotify(namespace, device)
		if err != nil || !found || !deviceCfg.Enabled {
			return
		}
		cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
		if err != nil || !configured || !notifyEventEnabled(cfg, event) {
			return
		}
		event.Namespace, event.Device = namespace, device
		if event.TS.IsZero() {
			event.TS = time.Now().UTC()
		}
		if _, err := r.deliver(context.Background(), cfg, device, event); err != nil {
			log.Printf("relay: webhook %s for %s/%s failed: %s", event.Event, namespace, device, deliveryErrorSummary(cfg, err))
		}
	}()
}

// emitAccountEvent handles events with no meaningful existing-device switch,
// namely a friend request and the first registration of a newly enrolled device.
func (r *Relay) emitAccountEvent(namespace string, event notify.Event) {
	if r.notifyStore == nil || r.notifySend == nil {
		return
	}
	go func() {
		cfg, configured, err := r.notifyStore.GetNotifyWebhook(namespace)
		if err != nil || !configured || !notifyEventEnabled(cfg, event) {
			return
		}
		event.Namespace = namespace
		if event.TS.IsZero() {
			event.TS = time.Now().UTC()
		}
		if _, err := r.deliver(context.Background(), cfg, relayNotifyHealthDevice, event); err != nil {
			log.Printf("relay: webhook %s for %s failed: %s", event.Event, namespace, deliveryErrorSummary(cfg, err))
		}
	}()
}

func (r *Relay) deliver(ctx context.Context, cfg NotifyWebhook, healthDevice string, event notify.Event) (notify.Result, error) {
	dst := notify.Destination{URL: cfg.URL, Format: cfg.Format, Keyword: cfg.Keyword, Secret: cfg.Secret}
	result, err := r.notifySend.Send(ctx, dst, event)
	if result.Dropped && result.Attempts == 0 {
		return result, err
	}
	health := NotifyHealth{
		Namespace: namespaceOr(event.Namespace, cfg.Namespace), Device: healthDevice,
		AttemptedAt: time.Now().UTC(), HTTPStatus: result.HTTPStatus,
		ProviderCode: result.ProviderCode, Result: "success",
	}
	if err != nil {
		health.Result = "failure"
		health.Error = deliveryErrorSummary(cfg, err)
		health.ConsecutiveFailures = 1
	}
	if recordErr := r.notifyStore.RecordNotifyHealth(health); recordErr != nil {
		log.Printf("relay: record webhook health for %s/%s: %v", health.Namespace, health.Device, recordErr)
	}
	return result, err
}

func namespaceOr(eventNS, configNS string) string {
	if eventNS != "" {
		return eventNS
	}
	return configNS
}

func deliveryErrorSummary(cfg NotifyWebhook, err error) string {
	if err == nil {
		return ""
	}
	return deliveryTextSummary(cfg, err.Error())
}

func deliveryTextSummary(cfg NotifyWebhook, text string) string {
	summary := text
	credentials := []string{cfg.URL, cfg.Secret}
	if parsed, err := url.Parse(cfg.URL); err == nil {
		for _, values := range parsed.Query() {
			credentials = append(credentials, values...)
		}
		credentials = append(credentials, path.Base(parsed.Path))
	}
	for _, credential := range credentials {
		if len(credential) >= 4 {
			summary = strings.ReplaceAll(summary, credential, "[REDACTED]")
		}
	}
	summary = eventlog.RedactText(summary)
	const max = 512
	if len(summary) > max {
		summary = strings.ToValidUTF8(summary[:max], "")
	}
	return summary
}

func notifyEventEnabled(cfg NotifyWebhook, event notify.Event) bool {
	switch {
	case strings.HasPrefix(event.Event, "approval."):
		return cfg.OnApproval
	case strings.HasPrefix(event.Event, "exec."):
		return cfg.OnExec && (!cfg.ExecFailuresOnly || event.Exit == nil || *event.Exit != 0)
	case strings.HasPrefix(event.Event, "device."):
		return cfg.OnLifecycle
	case event.Event == "pairing.requested", strings.HasPrefix(event.Event, "trust."),
		strings.HasPrefix(event.Event, "enroll."), strings.HasPrefix(event.Event, "friend."),
		strings.HasPrefix(event.Event, "access."):
		return cfg.OnSecurity
	case event.Event == "notify.test", event.Event == "notify.throttled":
		return true
	default:
		return false
	}
}

type deviceCreatedStore interface {
	UpsertDeviceCreated(namespace, name, fingerprint string) bool
}

func (r *Relay) recordDeviceRegistration(namespace, device, fingerprint string) bool {
	if r.admin == nil {
		return false
	}
	if store, ok := r.admin.(deviceCreatedStore); ok {
		return store.UpsertDeviceCreated(namespace, device, fingerprint)
	}
	r.admin.UpsertDevice(namespace, device, fingerprint)
	return false
}

func onlineEvent(device string) notify.Event {
	return notify.Event{Event: "device.online", Message: device + " is online"}
}

func offlineEvent(device string) notify.Event {
	return notify.Event{Event: "device.offline", Message: device + " is offline"}
}

func enrollEvent(device string) notify.Event {
	return notify.Event{Event: "enroll.completed", Device: device, Message: device + " joined the namespace"}
}
