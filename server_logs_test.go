package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/serverlog"
)

func TestFetchServerLogsCLI(t *testing.T) {
	hc := &http.Client{Transport: serverLogsRoundTrip(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/logs" || r.Header.Get("X-Admin-Secret") != "secret" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("service") != "portal" || r.URL.Query().Get("since") != "30m0s" || r.URL.Query().Get("grep") != "lark" {
			http.Error(w, "wrong query", http.StatusBadRequest)
			return
		}
		serverlog.WriteJSON(w, serverlog.Response{Service: "portal", Logs: []string{"lark line"}, Truncated: true})
	})}

	var out bytes.Buffer
	err := fetchServerLogs(context.Background(), hc, "https://portal.test", "secret",
		serverlog.Query{Service: "portal", Since: 30 * time.Minute, Limit: 10, Grep: "lark"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "lark line") || !strings.Contains(got, "results truncated") {
		t.Fatalf("output = %q", got)
	}
}

type serverLogsRoundTrip func(http.ResponseWriter, *http.Request)

func (f serverLogsRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	rr := newResponseRecorder()
	f(rr, req)
	return rr.response(), nil
}

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newResponseRecorder() *responseRecorder     { return &responseRecorder{header: make(http.Header)} }
func (r *responseRecorder) Header() http.Header  { return r.header }
func (r *responseRecorder) WriteHeader(code int) { r.code = code }
func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *responseRecorder) response() *http.Response {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return &http.Response{StatusCode: r.code, Status: http.StatusText(r.code), Header: r.header, Body: io.NopCloser(bytes.NewReader(r.body.Bytes()))}
}
