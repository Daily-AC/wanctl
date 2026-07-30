package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendCardCachesTokenAndSendsStringContent(t *testing.T) {
	var tokenCalls atomic.Int32
	card := map[string]any{"schema": "2.0", "title": "approval"}
	server := newHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tokenPath:
			tokenCalls.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("token Authorization = %q, want empty", got)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode token request: %v", err)
			}
			if body["app_id"] != "app" || body["app_secret"] != "secret" {
				t.Errorf("token request = %#v", body)
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t-cached", "expire": 7200})
		case "/open-apis/im/v1/messages":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if got := r.URL.Query().Get("receive_id_type"); got != "email" {
				t.Errorf("receive_id_type = %q, want email", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer t-cached" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
				Content   string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode send request: %v", err)
			}
			if body.ReceiveID != "owner@example.test" || body.MsgType != "interactive" {
				t.Errorf("send request = %#v", body)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(body.Content), &decoded); err != nil {
				t.Errorf("content is not serialized card JSON: %v", err)
			}
			if decoded["schema"] != "2.0" || decoded["title"] != "approval" {
				t.Errorf("decoded content = %#v", decoded)
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_test", "chat_id": "oc_test"}})
		default:
			http.NotFound(w, r)
		}
	}))
	client := newTestClient(server)
	for i := 0; i < 2; i++ {
		messageID, chatID, err := client.SendCard(context.Background(), "owner@example.test", card)
		if err != nil {
			t.Fatal(err)
		}
		if messageID != "om_test" || chatID != "oc_test" {
			t.Fatalf("SendCard = %q, %q", messageID, chatID)
		}
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}

func TestTokenRefreshesFiveMinutesBeforeExpiry(t *testing.T) {
	var tokenCalls atomic.Int32
	server := newHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			call := tokenCalls.Add(1)
			writeJSON(t, w, map[string]any{
				"code": 0, "tenant_access_token": fmt.Sprintf("t-%d", call), "expire": 301,
			})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om", "chat_id": "oc"}})
	}))
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	client := newTestClient(server)
	client.now = func() time.Time { return now }
	if _, _, err := client.SendCard(context.Background(), "owner@example.test", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, _, err := client.SendCard(context.Background(), "owner@example.test", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := tokenCalls.Load(); got != 2 {
		t.Fatalf("token calls = %d, want 2", got)
	}
}

func TestConcurrentSendFetchesTokenOnce(t *testing.T) {
	var tokenCalls atomic.Int32
	server := newHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			tokenCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "t-one", "expire": 7200})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om", "chat_id": "oc"}})
	}))
	client := newTestClient(server)
	const goroutines = 12
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := client.SendCard(context.Background(), "owner@example.test", map[string]any{})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}

func TestUpdateCardUsesPatchAndMessageIDPath(t *testing.T) {
	var updateSeen bool
	server := newHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "t-update", "expire": 7200})
			return
		}
		updateSeen = true
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/open-apis/im/v1/messages/om_test" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer t-update" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode update request: %v", err)
		}
		if _, ok := body["content"].(string); !ok {
			t.Errorf("content type = %T, want string", body["content"])
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	if err := newTestClient(server).UpdateCard(context.Background(), "om_test", map[string]any{"schema": "2.0"}); err != nil {
		t.Fatal(err)
	}
	if !updateSeen {
		t.Fatal("update request was not received")
	}
}

func TestAPIErrorIncludesLarkMessage(t *testing.T) {
	server := newHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "t-error", "expire": 7200})
			return
		}
		writeJSON(t, w, map[string]any{"code": 230099, "msg": "unknown property font_color"})
	}))
	_, _, err := newTestClient(server).SendCard(context.Background(), "owner@example.test", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unknown property font_color") {
		t.Fatalf("error = %v", err)
	}
}

type httpTestServer struct {
	URL    string
	client *http.Client
}

func newHTTPTestServer(handler http.Handler) *httpTestServer {
	return &httpTestServer{
		URL: "https://lark.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			return recorder.Result(), nil
		})},
	}
}

func (s *httpTestServer) Client() *http.Client { return s.client }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newTestClient(server *httpTestServer) *Client {
	client := NewClient("app", "secret")
	client.HTTPClient = server.Client()
	client.BaseURL = server.URL
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
