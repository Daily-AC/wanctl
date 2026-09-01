package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/notify"
)

type memNotifyAdmin struct {
	noopAdmin
	webhooks map[string]NotifyWebhook
	devices  map[string]DeviceNotify
	health   map[string]NotifyHealth
}

func (m *memNotifyAdmin) GetNotifyWebhook(namespace string) (NotifyWebhook, bool, error) {
	cfg, ok := m.webhooks[namespace]
	if !ok {
		return defaultNotifyWebhook(namespace), false, nil
	}
	return cfg, true, nil
}

func (m *memNotifyAdmin) UpsertNotifyWebhook(cfg NotifyWebhook) (NotifyWebhook, error) {
	if m.webhooks == nil {
		m.webhooks = map[string]NotifyWebhook{}
	}
	cfg.UpdatedAt = time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	m.webhooks[cfg.Namespace] = cfg
	return cfg, nil
}

func (m *memNotifyAdmin) DeleteNotifyWebhook(namespace string) error {
	delete(m.webhooks, namespace)
	return nil
}

func notifyDeviceKey(namespace, device string) string { return namespace + "/" + device }

func (m *memNotifyAdmin) GetDeviceNotify(namespace, device string) (DeviceNotify, bool, error) {
	cfg, ok := m.devices[notifyDeviceKey(namespace, device)]
	if !ok {
		return DeviceNotify{Namespace: namespace, Device: device}, false, nil
	}
	return cfg, true, nil
}

func (m *memNotifyAdmin) UpsertDeviceNotify(cfg DeviceNotify) (DeviceNotify, error) {
	if m.devices == nil {
		m.devices = map[string]DeviceNotify{}
	}
	cfg.UpdatedAt = time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	m.devices[notifyDeviceKey(cfg.Namespace, cfg.Device)] = cfg
	return cfg, nil
}

func (m *memNotifyAdmin) GetNotifyHealth(namespace, device string) (NotifyHealth, bool, error) {
	health, ok := m.health[notifyDeviceKey(namespace, device)]
	return health, ok, nil
}

func (m *memNotifyAdmin) RecordNotifyHealth(health NotifyHealth) error {
	if m.health == nil {
		m.health = map[string]NotifyHealth{}
	}
	m.health[notifyDeviceKey(health.Namespace, health.Device)] = health
	return nil
}

type capturedNotifySender struct {
	result notify.Result
	err    error
	events []notify.Event
}

func (s *capturedNotifySender) Send(_ context.Context, _ notify.Destination, event notify.Event) (notify.Result, error) {
	s.events = append(s.events, event)
	return s.result, s.err
}

func relayRequest(t *testing.T, r *Relay, method, target, body, token, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if secret != "" {
		req.Header.Set("X-Admin-Secret", secret)
	}
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAgentNotifyURLRouteIsGone(t *testing.T) {
	r := New(envTokens{"alice-token": "alice"})
	rr := relayRequest(t, r, http.MethodGet, "/agent/notify?device=legion", "", "alice-token", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
}

func TestSharedDeviceFriendCannotReadOwnerWebhookOnRelayRoutes(t *testing.T) {
	const secretURL = "https://hooks.example/owner-secret"
	store := &memNotifyAdmin{
		webhooks: map[string]NotifyWebhook{"alice": {
			Namespace: "alice", URL: secretURL, Format: notify.FormatJSON, OnApproval: true,
		}},
		devices: map[string]DeviceNotify{notifyDeviceKey("alice", "legion"): {
			Namespace: "alice", Device: "legion", Enabled: true,
		}},
	}
	r := New(envTokens{"friend-token": "bob"})
	r.SetAdmin(store)
	for _, target := range []string{
		"/u/notify", "/u/devices/notify?device=legion",
	} {
		rr := relayRequest(t, r, http.MethodGet, target, "", "friend-token", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s response = %d %q", target, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), secretURL) || strings.Contains(rr.Body.String(), "owner-secret") {
			t.Fatalf("%s leaked owner URL: %s", target, rr.Body.String())
		}
	}
}

func TestNotifyWriteRejectsNonHTTPSAndGetMasksURL(t *testing.T) {
	store := &memNotifyAdmin{}
	r := New(envTokens{"alice-token": "alice"})
	r.SetAdmin(store)
	body := `{"url":"http://hooks.example/topic","format":"json","on_approval":true}`
	rejected := relayRequest(t, r, http.MethodPost, "/u/notify", body, "alice-token", "")
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("HTTP URL response = %d %q", rejected.Code, rejected.Body.String())
	}

	body = `{"url":"https://hooks.example/secret-path-abcd","format":"json","secret":"top-secret","on_approval":true,"on_lifecycle":true,"on_security":true,"exec_failures_only":true}`
	posted := relayRequest(t, r, http.MethodPost, "/u/notify", body, "alice-token", "")
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), "secret-path-abcd") {
		t.Fatalf("POST response = %d %q", posted.Code, posted.Body.String())
	}
	got := relayRequest(t, r, http.MethodGet, "/u/notify", "", "alice-token", "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET response = %d %q", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "secret-path") || !strings.Contains(got.Body.String(), "https://hooks.example/...abcd") {
		t.Fatalf("GET URL was not masked: %s", got.Body.String())
	}
	if strings.Contains(got.Body.String(), "top-secret") || !strings.Contains(got.Body.String(), `"secret_set":true`) {
		t.Fatalf("GET leaked secret or omitted secret_set: %s", got.Body.String())
	}
}

func TestAdminNotifyTestReturnsReceiverStatus(t *testing.T) {
	store := &memNotifyAdmin{webhooks: map[string]NotifyWebhook{"alice": {
		Namespace: "alice", URL: "https://hooks.example/topic", Format: notify.FormatJSON,
	}}}
	sender := &capturedNotifySender{result: notify.Result{HTTPStatus: 202, ProviderCode: "0", Attempts: 1, Event: "notify.test"}}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(store)
	r.SetNotifySender(sender)
	rr := relayRequest(t, r, http.MethodPost, "/admin/notify/test", `{"namespace":"alice"}`, "", "secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["http_status"] != float64(202) || got["provider_code"] != "0" || len(sender.events) != 1 || sender.events[0].Event != "notify.test" {
		t.Fatalf("response = %#v, events = %#v", got, sender.events)
	}
	health, ok := store.health[notifyDeviceKey("alice", relayNotifyHealthDevice)]
	if !ok || health.Result != "success" || health.HTTPStatus != 202 || health.ProviderCode != "0" {
		t.Fatalf("health = %+v, found = %v", health, ok)
	}
}

func TestNotifyFailureSummaryRedactsURLSecretAndProviderToken(t *testing.T) {
	store := &memNotifyAdmin{webhooks: map[string]NotifyWebhook{"alice": {
		Namespace: "alice", URL: "https://hooks.example/robot/private-hook?access_token=query-token",
		Format: notify.FormatJSON, Secret: "signing-secret",
	}}}
	sender := &capturedNotifySender{
		result: notify.Result{HTTPStatus: 200, ProviderCode: "310000", ProviderMessage: "query-token rejected"},
		err:    errors.New("private-hook query-token signing-secret rejected"),
	}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(store)
	r.SetNotifySender(sender)
	rr := relayRequest(t, r, http.MethodPost, "/admin/notify/test", `{"namespace":"alice"}`, "", "secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
	for _, credential := range []string{"private-hook", "query-token", "signing-secret"} {
		if strings.Contains(rr.Body.String(), credential) {
			t.Fatalf("test response leaked %q: %s", credential, rr.Body.String())
		}
	}
	health := store.health[notifyDeviceKey("alice", relayNotifyHealthDevice)]
	if health.Result != "failure" || health.ProviderCode != "310000" || strings.Contains(health.Error, "query-token") {
		t.Fatalf("health = %+v", health)
	}
}

var _ NotifyStore = (*memNotifyAdmin)(nil)
