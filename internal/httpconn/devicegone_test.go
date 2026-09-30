package httpconn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A session the relay ended because the device went away reads as
// ErrDeviceGone, not as a clean end; any other 410 is still io.EOF.
func TestReadReportsADeviceThatWentAway(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   error
	}{
		{"device-gone", ErrDeviceGone},
		{"", io.EOF},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set(DownAckCapabilityHeader, "1")
			if tc.header != "" {
				w.Header().Set(SessionEndHeader, tc.header)
			}
			http.Error(w, "session closed", http.StatusGone)
		}))
		c, err := Dial(context.Background(), srv.URL, "s1", "client", "tok")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ { // and again: the end is sticky
			if _, err := c.Read(make([]byte, 16)); !errors.Is(err, tc.want) {
				t.Fatalf("header %q read %d: got %v, want %v", tc.header, i, err, tc.want)
			}
		}
		c.Close()
		srv.Close()
	}
}
