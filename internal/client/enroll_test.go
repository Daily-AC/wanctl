package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverInstanceRejectsBadRelay(t *testing.T) {
	for _, body := range []string{
		`{"relay":"ftp://relay.example"}`,
		`{"relay":"relay.example"}`,
		`{"relay":""}`,
		`{"relay":"https://relay.example","transport":"carrier-pigeon"}`,
		`not json`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		_, err := DiscoverInstance(context.Background(), srv.URL+"/")
		srv.Close()
		if err == nil {
			t.Fatalf("%s was accepted", body)
		}
	}
}

func TestDiscoverInstanceReadsRelayAndTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/instance" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"relay":"wss://relay.example","transport":"ws"}`))
	}))
	defer srv.Close()
	inst, err := DiscoverInstance(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Relay != "wss://relay.example" || inst.Transport != "ws" {
		t.Fatalf("got %+v", inst)
	}
}
