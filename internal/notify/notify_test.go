package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return r[host], nil
}

func publicResolver() staticResolver {
	return staticResolver{
		"hook.example":      {{IP: net.ParseIP("203.0.113.8")}},
		"open.feishu.cn":    {{IP: net.ParseIP("203.0.113.9")}},
		"oapi.dingtalk.com": {{IP: net.ParseIP("203.0.113.10")}},
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func fixedEvent() Event {
	exit := 7
	return Event{
		Event: "approval.pending", Namespace: "alice", Device: "legion",
		TS:      time.Date(2026, 8, 31, 10, 40, 0, 0, time.UTC),
		Message: "legion has a command waiting for approval", Detail: "deploy --token xxx", Peer: "macbook", Exit: &exit,
	}
}

func TestJSONBodyUsesGenericSchemaAndRedactsDetail(t *testing.T) {
	req, err := buildRequest(Destination{URL: "https://hook.example/topic", Format: FormatJSON}, fixedEvent(), "uuid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	var got Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != "approval.pending" || got.Namespace != "alice" || got.Device != "legion" || got.Detail != "deploy --token [REDACTED]" {
		t.Fatalf("payload = %+v; body = %s", got, body)
	}
}

func TestFeishuInteractiveCardShape(t *testing.T) {
	req, err := buildRequest(Destination{
		URL: "https://open.feishu.cn/open-apis/bot/v2/hook/id", Format: FormatFeishu, Keyword: "WANCTL",
	}, fixedEvent(), "uuid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MsgType string `json:"msg_type"`
		Card    struct {
			Config struct {
				Wide bool `json:"wide_screen_mode"`
			} `json:"config"`
			Header struct {
				Title struct {
					Content string `json:"content"`
				} `json:"title"`
				Template string `json:"template"`
			} `json:"header"`
			Elements []any `json:"elements"`
		} `json:"card"`
	}
	if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.MsgType != "interactive" || !got.Card.Config.Wide || got.Card.Header.Template != "red" ||
		!strings.Contains(got.Card.Header.Title.Content, "WANCTL") || len(got.Card.Elements) != 2 {
		t.Fatalf("Feishu body = %+v", got)
	}
}

func TestDingTalkMarkdownShapeIncludesMessageUUID(t *testing.T) {
	req, err := buildRequest(Destination{
		URL: "https://oapi.dingtalk.com/robot/send?access_token=abc", Format: FormatDingTalk, Keyword: "WANCTL",
	}, fixedEvent(), "same-uuid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MsgType  string `json:"msgtype"`
		MsgUUID  string `json:"msgUuid"`
		Markdown struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"markdown"`
	}
	if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.MsgType != "markdown" || got.MsgUUID != "same-uuid" || !strings.Contains(got.Markdown.Title, "WANCTL") || !strings.Contains(got.Markdown.Text, "legion") {
		t.Fatalf("DingTalk body = %+v", got)
	}
}

func TestProviderErrorsInsideHTTP200AreFailures(t *testing.T) {
	tests := []struct {
		name, format, url, body, wantCode, wantMessage string
	}{
		{"Feishu", FormatFeishu, "https://open.feishu.cn/open-apis/bot/v2/hook/id", `{"code":19024,"msg":"Key Words Not Found"}`, "19024", "Key Words"},
		{"DingTalk", FormatDingTalk, "https://oapi.dingtalk.com/robot/send?access_token=abc", `{"errcode":310000,"errmsg":"keywords not in content"}`, "310000", "keywords"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tt.body), nil
			})}
			s := NewSender(Options{Client: client, Resolver: publicResolver(), MaxAttempts: 1})
			result, err := s.Send(context.Background(), Destination{URL: tt.url, Format: tt.format}, fixedEvent())
			if err == nil || result.ProviderCode != tt.wantCode || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
		})
	}
}

func TestDingTalkRetriesReuseMessageUUID(t *testing.T) {
	var uuids []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			MsgUUID string `json:"msgUuid"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		uuids = append(uuids, body.MsgUUID)
		if len(uuids) == 1 {
			return response(http.StatusBadGateway, "temporary"), nil
		}
		return response(http.StatusOK, `{"errcode":0,"errmsg":"ok"}`), nil
	})}
	s := NewSender(Options{Client: client, Resolver: publicResolver(), MaxAttempts: 2, RetryBackoff: time.Millisecond})
	_, err := s.Send(context.Background(), Destination{
		URL: "https://oapi.dingtalk.com/robot/send?access_token=abc", Format: FormatDingTalk,
	}, fixedEvent())
	if err != nil {
		t.Fatal(err)
	}
	if len(uuids) != 2 || uuids[0] == "" || uuids[0] != uuids[1] {
		t.Fatalf("retry UUIDs = %#v", uuids)
	}
}

func TestDingTalkSignatureFixedVector(t *testing.T) {
	got, err := DingTalkSignedURL(
		"https://oapi.dingtalk.com/robot/send?access_token=abc", "SECtest", 1591703124881,
	)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("timestamp") != "1591703124881" || u.Query().Get("sign") != "4Qmv7JLNsOMwUBIhVo+QS5TmIKKh7pr/0qLhEkh2x1Q=" {
		t.Fatalf("signed URL = %s", got)
	}
}
func TestSenderRejectsPrivateDestination(t *testing.T) {
	resolver := staticResolver{"hook.example": {{IP: net.ParseIP("10.0.0.8")}}}
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusNoContent, ""), nil
	})}
	s := NewSender(Options{Resolver: resolver, Client: client, MaxAttempts: 1})
	if _, err := s.Send(context.Background(), Destination{URL: "https://hook.example/x", Format: FormatJSON}, fixedEvent()); err == nil {
		t.Fatal("sender accepted a private destination")
	}
	if calls != 0 {
		t.Fatalf("private destination reached transport %d times", calls)
	}
}

func TestSenderLimitsAccountToFifteenEventsAndOneNotice(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var events []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var e Event
		_ = json.NewDecoder(req.Body).Decode(&e)
		mu.Lock()
		events = append(events, e.Event)
		mu.Unlock()
		return response(http.StatusNoContent, ""), nil
	})}
	s := NewSender(Options{Client: client, Resolver: publicResolver(), Now: func() time.Time { return now }, MaxAttempts: 1})
	for i := 0; i < 20; i++ {
		device := "one"
		if i%2 == 1 {
			device = "two"
		}
		result, err := s.Send(context.Background(), Destination{URL: "https://hook.example/x", Format: FormatJSON}, Event{
			Event: "exec.finished", Namespace: "alice", Device: device, Message: "done",
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 15 && (!result.Dropped || result.Event != "notify.throttled") {
			t.Fatalf("16th result = %+v", result)
		}
	}
	if len(events) != 16 {
		t.Fatalf("delivered events = %d, want 16", len(events))
	}
	throttled := 0
	for _, event := range events {
		if event == "notify.throttled" {
			throttled++
		}
	}
	if throttled != 1 {
		t.Fatalf("throttle notices = %d, events = %v", throttled, events)
	}
}

// TestFeishuSignVector pins Feishu's signature construction against a vector
// computed outside this package (Python, following Feishu's documented
// algorithm). Feishu and DingTalk use opposite HMAC directions and different
// timestamp units, so a same-shaped mistake would still produce a plausible
// string; only an external vector catches it.
func TestFeishuSignVector(t *testing.T) {
	const want = "cwcFMwLmbD1sHo+u7QsHVT95ls+pZLcBESGTstkPpro="
	if got := FeishuSign("wanctl-test-secret", 1600000000); got != want {
		t.Fatalf("FeishuSign = %q, want %q", got, want)
	}
	// The two providers must not converge on the same construction.
	dingURL, err := DingTalkSignedURL(
		"https://oapi.dingtalk.com/robot/send?access_token=abc", "wanctl-test-secret", 1600000000000)
	if err != nil {
		t.Fatalf("DingTalkSignedURL: %v", err)
	}
	if strings.Contains(dingURL, url.QueryEscape(want)) {
		t.Error("DingTalk signature matches the Feishu construction; one of them is wrong")
	}
}

func TestFeishuBodyCarriesSignatureOnlyWhenConfigured(t *testing.T) {
	e := Event{Event: "device.online", Namespace: "alice", Device: "legion", TS: time.Unix(1600000000, 0)}
	signed, err := buildRequest(Destination{
		URL: "https://open.feishu.cn/open-apis/bot/v2/hook/xxx", Format: FormatFeishu,
		Secret: "wanctl-test-secret",
	}, e, "uuid", time.Unix(1600000000, 0))
	if err != nil {
		t.Fatalf("signed buildRequest: %v", err)
	}
	b, _ := io.ReadAll(signed.Body)
	var got struct {
		Timestamp string `json:"timestamp"`
		Sign      string `json:"sign"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Timestamp != "1600000000" {
		t.Errorf("timestamp = %q, want seconds 1600000000", got.Timestamp)
	}
	if got.Sign != "cwcFMwLmbD1sHo+u7QsHVT95ls+pZLcBESGTstkPpro=" {
		t.Errorf("sign = %q, want the documented vector", got.Sign)
	}

	plain, err := buildRequest(Destination{
		URL: "https://open.feishu.cn/open-apis/bot/v2/hook/xxx", Format: FormatFeishu,
	}, e, "uuid", time.Unix(1600000000, 0))
	if err != nil {
		t.Fatalf("plain buildRequest: %v", err)
	}
	pb, _ := io.ReadAll(plain.Body)
	if bytes.Contains(pb, []byte("\"sign\"")) || bytes.Contains(pb, []byte("\"timestamp\"")) {
		t.Errorf("unsigned Feishu body must omit sign/timestamp, got %s", pb)
	}
}
