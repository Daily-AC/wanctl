package relay

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/notify"
)

type channelNotifySender chan notify.Event

func (s channelNotifySender) Send(_ context.Context, _ notify.Destination, event notify.Event) (notify.Result, error) {
	s <- event
	return notify.Result{HTTPStatus: http.StatusNoContent, Attempts: 1, Event: event.Event}, nil
}

func newReportingRelay(includeDetail bool) (*Relay, channelNotifySender) {
	store := &memNotifyAdmin{
		webhooks: map[string]NotifyWebhook{"alice": {
			Namespace: "alice", URL: "https://hooks.example/topic", Format: notify.FormatJSON,
			OnApproval: true, OnExec: true, OnSecurity: true, IncludeDetail: includeDetail,
		}},
		devices: map[string]DeviceNotify{notifyDeviceKey("alice", "legion"): {
			Namespace: "alice", Device: "legion", Enabled: true,
		}},
	}
	sender := make(channelNotifySender, 4)
	r := New(envTokens{"device-token": "alice"})
	r.SetAdmin(store)
	r.SetNotifySender(sender)
	r.agents["alice/legion"] = &agentConn{ns: "alice", device: "legion", inst: "instance-1"}
	return r, sender
}

func TestAgentEventCannotForgeAnotherDevice(t *testing.T) {
	r, _ := newReportingRelay(false)
	body := `{"id":"event-1","event":"approval.pending","ts":"2026-08-31T10:00:00Z"}`
	forged := relayRequest(t, r, http.MethodPost, "/agent/events?device=other&inst=instance-1", body, "device-token", "")
	if forged.Code != http.StatusForbidden {
		t.Fatalf("forged device response = %d %q", forged.Code, forged.Body.String())
	}
	bodyNamesDevice := `{"id":"event-2","event":"approval.pending","device":"other"}`
	unknown := relayRequest(t, r, http.MethodPost, "/agent/events?device=legion&inst=instance-1", bodyNamesDevice, "device-token", "")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("body device response = %d %q", unknown.Code, unknown.Body.String())
	}
}

func TestAgentEventIsDeduplicatedAcrossReplay(t *testing.T) {
	r, sender := newReportingRelay(true)
	body := `{"id":"approval-123","event":"approval.pending","detail":"deploy --token xxx","peer":"macbook"}`
	first := relayRequest(t, r, http.MethodPost, "/agent/events?device=legion&inst=instance-1", body, "device-token", "")
	second := relayRequest(t, r, http.MethodPost, "/agent/events?device=legion&inst=instance-1", body, "device-token", "")
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("responses = first %d %q; second %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	select {
	case event := <-sender:
		if event.Detail != "deploy --token xxx" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("reported event was not delivered")
	}
	select {
	case event := <-sender:
		t.Fatalf("duplicate event delivered: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAgentPolicyContainsNoWebhookCredentials(t *testing.T) {
	r, _ := newReportingRelay(true)
	rr := relayRequest(t, r, http.MethodGet, "/agent/notify-policy?device=legion&inst=instance-1", "", "device-token", "")
	if rr.Code != http.StatusOK || rr.Body.String() != `{"include_detail":true}`+"\n" {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "hooks.example") || strings.Contains(rr.Body.String(), "url") || strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("policy leaked credentials: %s", rr.Body.String())
	}
}

func TestAgentEventDedupeHasHardCapacity(t *testing.T) {
	r := New(envTokens{})
	now := time.Now()
	oldest := notifyDedupeKey{namespace: "alice", device: "legion", id: "oldest"}
	r.notifyDedupe[oldest] = now.Add(-time.Minute)
	for i := 1; i < agentEventDedupeMax; i++ {
		r.notifyDedupe[notifyDedupeKey{namespace: "alice", device: "legion", id: string(rune(i))}] = now
	}
	if r.notifyEventDuplicate("alice", "legion", "new", now) {
		t.Fatal("new event was marked duplicate")
	}
	if len(r.notifyDedupe) != agentEventDedupeMax {
		t.Fatalf("dedupe size = %d, want %d", len(r.notifyDedupe), agentEventDedupeMax)
	}
	if _, found := r.notifyDedupe[oldest]; found {
		t.Fatal("oldest dedupe entry was not evicted")
	}
}
