package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

// The act result for a failed engine download names the failure without the
// credentials, path or query of the URL the release redirected to.
func TestAndroidActResultOmitsDownloadURLDetails(t *testing.T) {
	host, _, _ := androidHost(t, "")
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://user:SYNTHETIC_PASSWORD@127.0.0.1:1/private/engine?signature=SYNTHETIC_SECRET", http.StatusFound)
	}))
	defer release.Close()
	host.Cache = opencu.NewAndroidCache(t.TempDir(), release.URL)
	s := &Service{}
	defer s.Close()
	res := s.DoOpenCU(context.Background(), "owner", NewID(), protocol.DesktopRequest{Discover: true}, func(bool) bool { return true }, host.StartOpenCU)
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_NOT_INSTALLED" || !strings.Contains(res.Error, "opencu-android.json download failed") {
		t.Fatalf("result %s", wire)
	}
	for _, secret := range []string{"SYNTHETIC", "private", "signature", "user:"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("act result keeps %q: %s", secret, wire)
		}
	}
}
