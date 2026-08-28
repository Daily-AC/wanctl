// Package limits contains process-wide resource and network bounds.
package limits

import (
	"context"
	"net"
	"net/http"
	"time"
)

type connContextKey struct{}

const (
	RelayHTTPUploadBytes int64 = 1 << 20 // 1 MiB per HTTP tunnel write
	// RelayControlBodyBytes caps the JSON bodies of the relay's control
	// endpoints (/enroll/exchange, /u/*, /admin/*, /h/dial …). Before this
	// cap a single unauthenticated 1 GiB POST to /enroll/exchange took the
	// relay from 21 MB to 4.2 GB RSS (audit 2026-08-28, SEC-B-02): a JSON
	// string value has to be buffered whole before it can be rejected.
	RelayControlBodyBytes int64 = 64 << 10 // 64 KiB
	// RelayDocsBodyBytes is the cap for portal documentation articles, which
	// are Markdown written by an admin and legitimately larger than a token
	// request.
	RelayDocsBodyBytes int64 = 1 << 20 // 1 MiB
	// RelayMCPBodyBytes bounds one JSON-RPC request to the relay-hosted MCP
	// endpoint: wanctl_push_blob carries up to 8 MiB of base64.
	RelayMCPBodyBytes int64 = 16 << 20 // 16 MiB

	HTTPReadHeaderTimeout = 10 * time.Second
	HTTPReadTimeout       = 30 * time.Second
	HTTPWriteTimeout      = 15 * time.Minute // preserves long polls and ordinary long commands
	HTTPIdleTimeout       = 2 * time.Minute

	MaxConcurrentJobs = 4
	JobRunTimeout     = 30 * time.Minute
	MaxJobOutputBytes = 8 << 20 // 8 MiB merged stdout/stderr per job
	MaxRetainedJobs   = 64
	MaxRetainedBytes  = 64 << 20 // 64 MiB across completed jobs
	JobRetention      = time.Hour
)

// HTTPServer applies the same slow-client bounds to every public HTTP entrypoint.
func HTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: HTTPReadHeaderTimeout,
		ReadTimeout:       HTTPReadTimeout,
		WriteTimeout:      HTTPWriteTimeout,
		IdleTimeout:       HTTPIdleTimeout,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, connContextKey{}, conn)
		},
	}
}

// ClearHijackedDeadline removes HTTP request deadlines from a connection after
// a WebSocket upgrade. Relay WebSockets are intentionally long-lived.
func ClearHijackedDeadline(ctx context.Context) {
	if conn, ok := ctx.Value(connContextKey{}).(net.Conn); ok {
		_ = conn.SetDeadline(time.Time{})
	}
}
