package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"wanctl/internal/config"
	"wanctl/internal/serverlog"
)

func TestMCPServerLogs(t *testing.T) {
	oldSessions := sessions
	sessions = &sessionStore{stdio: &localFsSession{}}
	t.Cleanup(func() { sessions = oldSessions })
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "controller-token")
	t.Setenv("WANCTL_RELAY", "https://relay.test")
	oldClient := serverLogsHTTPClient
	serverLogsHTTPClient = &http.Client{Transport: mcpServerLogsRoundTrip(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Secret") != "secret" || r.URL.Query().Get("service") != "relay" || r.URL.Query().Get("limit") != "2000" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		serverlog.WriteJSON(w, serverlog.Response{Service: "relay", Logs: []string{"relay ready"}})
	})}
	t.Cleanup(func() { serverLogsHTTPClient = oldClient })
	t.Setenv("WANCTL_PORTAL", "https://portal.test")
	t.Setenv("WANCTL_ADMIN_SECRET", "secret")

	res, err := mcpServerLogs(context.Background(), execReq(map[string]any{
		"service": "relay", "since": "30m", "limit": float64(9000),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); !strings.Contains(got, "relay ready") {
		t.Fatalf("result = %q", got)
	}
}

type mcpServerLogsRoundTrip func(http.ResponseWriter, *http.Request)

func (f mcpServerLogsRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	var body bytes.Buffer
	rw := &mcpResponseWriter{header: h, body: &body}
	f(rw, req)
	if rw.code == 0 {
		rw.code = http.StatusOK
	}
	return &http.Response{StatusCode: rw.code, Status: http.StatusText(rw.code), Header: h, Body: io.NopCloser(bytes.NewReader(body.Bytes()))}, nil
}

type mcpResponseWriter struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (w *mcpResponseWriter) Header() http.Header         { return w.header }
func (w *mcpResponseWriter) WriteHeader(code int)        { w.code = code }
func (w *mcpResponseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func TestMCPServerLogsValidatesService(t *testing.T) {
	res, err := mcpServerLogs(context.Background(), execReq(map[string]any{"service": "device"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); !strings.Contains(got, "portal or relay") {
		t.Fatalf("result = %q", got)
	}
}

func TestMCPServerLogsRequiresLogin(t *testing.T) {
	oldSessions := sessions
	sessions = &sessionStore{stdio: &localFsSession{}}
	t.Cleanup(func() { sessions = oldSessions })
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "")

	res, err := mcpServerLogs(context.Background(), execReq(map[string]any{"service": "portal"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(res); !strings.Contains(got, "LOGIN REQUIRED") {
		t.Fatalf("result = %q", got)
	}
}

func TestMCPToolReportsMissingRelay(t *testing.T) {
	oldSessions := sessions
	sessions = &sessionStore{stdio: &localFsSession{}}
	t.Cleanup(func() { sessions = oldSessions })
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "controller-token")
	t.Setenv("WANCTL_RELAY", "")
	oldDefault := config.DefaultRelay
	config.DefaultRelay = ""
	t.Cleanup(func() { config.DefaultRelay = oldDefault })

	res, err := mcpPeers(context.Background(), execReq(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "wanctl config set relay=") {
		t.Fatalf("result = %+v", res)
	}
}
