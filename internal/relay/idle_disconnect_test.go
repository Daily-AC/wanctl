package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Cancelling an idle controller's single poll ends its session, but ordinary
// prefetch cancellation and a failed data-bearing response must keep the
// bytes the HTTP carrier's existing retry protocol promises to resend.
func TestCancelledHTTPPollPreservesDataAndPrefetch(t *testing.T) {
	for _, tc := range []struct {
		name, role, want string
		data             bool
	}{
		{name: "data-bearing controller response", role: "client", data: true},
		{name: "numbered controller prefetch", role: "client", want: "&want=1"},
		{name: "agent poll while its reply is in flight", role: "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, sid := tunnelSession(t)
			queue := s.toClient
			if tc.role == "agent" {
				queue = s.toAgent
			}
			poll := func(ctx context.Context) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/h/down?session="+sid+"&role="+tc.role+"&ack=0"+tc.want, nil).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer tok-alice")
				out := httptest.NewRecorder()
				r.Handler().ServeHTTP(out, req)
				return out
			}
			if tc.data && !queue.push([]byte("keep this chunk")) {
				t.Fatal("queue closed before test")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			first := poll(ctx)
			if r.session(sid) == nil {
				t.Fatal("ordinary poll cancellation ended the session")
			}
			if !tc.data {
				if first.Code != http.StatusNoContent {
					t.Fatalf("empty poll = %d", first.Code)
				}
				if !queue.push([]byte("keep this chunk")) {
					t.Fatal("cancellation closed the queue")
				}
			}
			again := poll(context.Background())
			if again.Code != http.StatusOK || again.Body.String() != "keep this chunk" {
				t.Fatalf("retry = %d %q", again.Code, again.Body.String())
			}
			if tc.data && (first.Body.String() != again.Body.String() || first.Header().Get("X-Wanctl-Down-Seq") != again.Header().Get("X-Wanctl-Down-Seq")) {
				t.Fatal("data-bearing response was not replayed with its original sequence")
			}
		})
	}
}
