package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wanctl/internal/serverlog"
)

func TestAdminLogsAuthFiltersRedactsAndTruncates(t *testing.T) {
	logs := serverlog.NewDefault()
	now := time.Now()
	logs.AppendAt(now, "old lark")
	logs.AppendAt(now, "lark token=topsecret first")
	logs.AppendAt(now, "ignore this")
	logs.AppendAt(now, "lark second")
	r := New(EnvTokenStore("token:ns"))
	r.SetAdminSecret("secret")
	r.SetLogBuffer(logs)
	h := r.Handler()

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/logs?service=relay", nil))
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want 403", unauthorized.Code)
	}

	q := url.Values{"service": {"relay"}, "since": {"1m"}, "limit": {"2"}, "grep": {"lark"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/logs?"+q.Encode(), nil)
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got serverlog.Response
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Logs) != 2 || !got.Truncated {
		t.Fatalf("response = %#v", got)
	}
	if strings.Contains(strings.Join(got.Logs, "\n"), "topsecret") {
		t.Fatalf("secret leaked: %#v", got.Logs)
	}
}
