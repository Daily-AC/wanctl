package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/serverlog"
)

func TestAdminLogsLocalAndRelayProxy(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/logs" || r.Header.Get("X-Admin-Secret") != "x" || r.URL.Query().Get("service") != "relay" {
			http.NotFound(w, r)
			return
		}
		serverlog.WriteJSON(w, serverlog.Response{Service: "relay", Logs: []string{"relay line"}})
	})
	logs := serverlog.NewDefault()
	logs.AppendAt(time.Now(), "portal secret=hush lark")
	s.SetLogBuffer(logs)
	h := s.Handler()

	for _, tc := range []struct {
		name, target, want string
	}{
		{"portal", "/admin/logs?service=portal&since=1m&grep=lark", "portal secret=[REDACTED] lark"},
		{"relay", "/admin/logs?service=relay&since=1m", "relay line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Header.Set("X-Admin-Secret", "x")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("status/body = %d %q", rr.Code, rr.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/logs?service=portal", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want 403", rr.Code)
	}
}
