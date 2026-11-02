package opencu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The engine handed out by Acquire carries the verified bytes: later changes to
// the cached file do not alter them, and a reader is never longer than the
// manifest says.
func TestAndroidEngineReaderIsTheVerifiedBytes(t *testing.T) {
	body := "verified engine bytes"
	m := androidManifest(body, "1.0.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(m)
		} else {
			fmt.Fprint(w, body)
		}
	}))
	defer server.Close()
	c := NewAndroidCache(t.TempDir(), server.URL)
	e, release, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.WriteFile(e.Path, []byte(body+" grown in place after acquisition"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := e.Reader()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != body {
		t.Fatalf("reader returned %q", b)
	}

	for name, bad := range map[string]AndroidEngine{
		"short":      {Manifest: m, jar: body[:3]},
		"long":       {Manifest: m, jar: body + "!"},
		"over limit": {Manifest: AndroidManifest{Size: MaxAndroidJar + 1}, jar: ""},
		"unverified": {Manifest: m, Path: e.Path},
	} {
		if _, err := bad.Reader(); err == nil {
			t.Errorf("%s: reader accepted bytes that do not match the manifest", name)
		}
	}
}
