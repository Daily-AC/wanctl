package limits

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerConfiguresAllTimeouts(t *testing.T) {
	s := HTTPServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Fatalf("HTTP server has missing timeout: %+v", s)
	}
}

func TestClearHijackedDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	s := HTTPServer(":0", http.NewServeMux())
	ctx := s.ConnContext(context.Background(), left)
	if err := left.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	ClearHijackedDeadline(ctx)
	read := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := right.Read(b[:])
		read <- err
	}()
	if _, err := left.Write([]byte{'x'}); err != nil {
		t.Fatalf("cleared connection still has expired deadline: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}
