package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memLarkAdmin struct {
	noopAdmin
	configs []DeviceLarkApproval
	upsert  DeviceLarkApproval
}

func (m *memLarkAdmin) ListLarkApproval(namespace string) ([]DeviceLarkApproval, error) {
	out := []DeviceLarkApproval{}
	for _, cfg := range m.configs {
		if cfg.Namespace == namespace {
			out = append(out, cfg)
		}
	}
	return out, nil
}

func (m *memLarkAdmin) UpsertLarkApproval(cfg DeviceLarkApproval) (DeviceLarkApproval, error) {
	cfg.UpdatedAt = time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	m.upsert = cfg
	return cfg, nil
}

func larkAdminRequest(t *testing.T, r *Relay, method, target, body, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Admin-Secret", secret)
	}
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminDevicesLarkGetAndPost(t *testing.T) {
	store := &memLarkAdmin{configs: []DeviceLarkApproval{{
		Namespace: "alice", Device: "legion", ApprovalEnabled: true,
		NotifyEmail: "alice@example.com", UpdatedAt: time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
	}}}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(store)

	get := larkAdminRequest(t, r, http.MethodGet, "/admin/devices/lark?namespace=alice", "", "secret")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body = %s", get.Code, get.Body.String())
	}
	raw := get.Body.Bytes()
	var listed struct {
		Devices []DeviceLarkApproval `json:"devices"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Devices) != 1 || listed.Devices[0].Device != "legion" || !listed.Devices[0].ApprovalEnabled {
		t.Fatalf("GET devices = %+v", listed.Devices)
	}
	if strings.Contains(string(raw), "namespace") {
		t.Fatalf("GET leaked internal namespace field: %s", raw)
	}
	empty := larkAdminRequest(t, r, http.MethodGet, "/admin/devices/lark?namespace=bob", "", "secret")
	var emptyList struct {
		Devices []DeviceLarkApproval `json:"devices"`
	}
	if empty.Code != http.StatusOK || json.NewDecoder(empty.Body).Decode(&emptyList) != nil {
		t.Fatalf("empty GET status = %d; body = %s", empty.Code, empty.Body.String())
	}
	if emptyList.Devices == nil || len(emptyList.Devices) != 0 {
		t.Fatalf("empty GET devices = %#v, want []", emptyList.Devices)
	}

	post := larkAdminRequest(t, r, http.MethodPost, "/admin/devices/lark",
		`{"namespace":"alice","device":"legion","approval_enabled":true,"pairing_from_card":true,"notify_email":"alice@example.com"}`,
		"secret")
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d; body = %s", post.Code, post.Body.String())
	}
	var posted DeviceLarkApproval
	if err := json.NewDecoder(post.Body).Decode(&posted); err != nil {
		t.Fatal(err)
	}
	if posted.Device != "legion" || !posted.ApprovalEnabled || !posted.PairingFromCard || posted.UpdatedAt.IsZero() {
		t.Fatalf("POST response = %+v", posted)
	}
	if store.upsert.Namespace != "alice" || store.upsert.Device != "legion" || !store.upsert.ApprovalEnabled ||
		!store.upsert.PairingFromCard || store.upsert.NotifyEmail != "alice@example.com" {
		t.Fatalf("persisted config = %+v", store.upsert)
	}
}

func TestAdminDevicesLarkSecretGated(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&memLarkAdmin{})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			target, body := "/admin/devices/lark?namespace=alice", ""
			if method == http.MethodPost {
				target = "/admin/devices/lark"
				body = `{"namespace":"alice","device":"legion","notify_email":"alice@example.com"}`
			}
			rr := larkAdminRequest(t, r, method, target, body, "")
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAdminDevicesLarkWithoutPostgres(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	forbidden := larkAdminRequest(t, r, http.MethodGet, "/admin/devices/lark?namespace=alice", "", "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("missing secret without Postgres = %d, want 403", forbidden.Code)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			rr := larkAdminRequest(t, r, method, "/admin/devices/lark?namespace=alice", `{}`, "secret")
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "Postgres") {
				t.Fatalf("missing readable unavailable message: %q", rr.Body.String())
			}
		})
	}
}

func TestPGStoreLarkApprovalSQL(t *testing.T) {
	p := newAdminTestPGStore(t)

	listed, err := p.ListLarkApproval("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Device != "legion" || listed[0].NotifyEmail != "alice@example.com" {
		t.Fatalf("listed configs = %+v", listed)
	}

	got, err := p.UpsertLarkApproval(DeviceLarkApproval{
		Namespace: "alice", Device: "legion", ApprovalEnabled: true, NotifyEmail: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "alice" || got.Device != "legion" || !got.ApprovalEnabled || got.UpdatedAt.IsZero() {
		t.Fatalf("upserted config = %+v", got)
	}
}
