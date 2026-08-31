// Package notify builds and delivers relay-side webhook notifications.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/eventlog"
)

const (
	FormatJSON     = "json"
	FormatFeishu   = "feishu"
	FormatDingTalk = "dingtalk"
)

// Destination is relay-only configuration. URL and Secret must never be
// returned by a read API without masking.
type Destination struct {
	URL     string
	Format  string
	Keyword string
	Secret  string
}

// Event is the transport-neutral notification payload.
type Event struct {
	Event     string    `json:"event"`
	Namespace string    `json:"namespace"`
	Device    string    `json:"device"`
	TS        time.Time `json:"ts"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail,omitempty"`
	Peer      string    `json:"peer,omitempty"`
	Exit      *int      `json:"exit"`
}

// Result describes the last delivery attempt, including provider-level codes
// returned inside an HTTP 200 response.
type Result struct {
	HTTPStatus      int    `json:"http_status"`
	ProviderCode    string `json:"provider_code,omitempty"`
	ProviderMessage string `json:"provider_message,omitempty"`
	Attempts        int    `json:"attempts"`
	Dropped         bool   `json:"dropped,omitempty"`
	Event           string `json:"event"`
}

// ValidateURL applies the write-time URL policy shared by portal and relay.
func ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("webhook URL must be an https:// URL without user info")
	}
	return nil
}

// ValidateDestination validates provider-specific configuration. Feishu
// signing is deliberately rejected: the official signing formula could not be
// verified in the implementation environment, and Feishu and DingTalk reverse
// the HMAC key/data roles. Shipping a guessed signature is worse than requiring
// the verified IP allow-list or keyword controls.
func ValidateDestination(dst Destination) error {
	if err := ValidateURL(dst.URL); err != nil {
		return err
	}
	switch dst.Format {
	case FormatJSON, FormatFeishu:
	case FormatDingTalk:
		u, _ := url.Parse(dst.URL)
		if !strings.EqualFold(u.Hostname(), "oapi.dingtalk.com") || u.Path != "/robot/send" || u.Query().Get("access_token") == "" {
			return errors.New("DingTalk webhook URL must be https://oapi.dingtalk.com/robot/send?access_token=...")
		}
	default:
		return fmt.Errorf("unsupported webhook format %q", dst.Format)
	}
	if dst.Format == FormatFeishu && dst.Secret != "" {
		return errors.New("Feishu signing is unavailable; use an IP allow-list or keyword security setting")
	}
	return nil
}

// MaskURL preserves enough of a destination to recognize it without returning
// the credential-bearing path or query.
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	tail := raw
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return u.Scheme + "://" + u.Host + "/..." + tail
}

func sanitize(e Event) Event {
	e.Message = eventlog.RedactText(e.Message)
	e.Detail = eventlog.RedactText(e.Detail)
	e.Peer = eventlog.RedactText(e.Peer)
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	} else {
		e.TS = e.TS.UTC()
	}
	return e
}

type feishuCard struct {
	MsgType string `json:"msg_type"`
	Card    struct {
		Config struct {
			WideScreenMode bool `json:"wide_screen_mode"`
		} `json:"config"`
		Header struct {
			Title struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"title"`
			Template string `json:"template"`
		} `json:"header"`
		Elements []any `json:"elements"`
	} `json:"card"`
}

func titleFor(e Event, keyword string) string {
	title := "wanctl " + e.Event
	if e.Device != "" {
		title += " - " + e.Device
	}
	keyword = strings.TrimSpace(keyword)
	if keyword != "" && !strings.Contains(title, keyword) {
		title = keyword + " - " + title
	}
	return title
}

func severe(event string) bool {
	return strings.HasPrefix(event, "approval.") || event == "pairing.requested" ||
		strings.HasPrefix(event, "trust.") || strings.HasPrefix(event, "enroll.") ||
		strings.HasPrefix(event, "friend.") || event == "notify.throttled"
}

func feishuBody(e Event, keyword string) ([]byte, error) {
	var card feishuCard
	card.MsgType = "interactive"
	card.Card.Config.WideScreenMode = true
	card.Card.Header.Title.Tag = "plain_text"
	card.Card.Header.Title.Content = titleFor(e, keyword)
	if severe(e.Event) {
		card.Card.Header.Template = "red"
	} else if strings.HasPrefix(e.Event, "exec.") {
		card.Card.Header.Template = "grey"
	} else {
		card.Card.Header.Template = "blue"
	}
	message := e.Message
	if e.Detail != "" {
		message += "\n\n" + e.Detail
	}
	card.Card.Elements = []any{
		map[string]any{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": message}},
		map[string]any{"tag": "note", "elements": []any{
			map[string]string{"tag": "plain_text", "content": eventNote(e)},
		}},
	}
	return json.Marshal(card)
}

func eventNote(e Event) string {
	device := e.Device
	if device == "" {
		device = "relay"
	}
	return device + " · " + e.TS.Local().Format("2006-01-02 15:04:05 MST")
}

func dingtalkBody(e Event, keyword, msgUUID string) ([]byte, error) {
	title := titleFor(e, keyword)
	text := "### " + title + "\n\n" + e.Message
	if e.Detail != "" {
		text += "\n\n```\n" + e.Detail + "\n```"
	}
	text += "\n\n> " + eventNote(e)
	return json.Marshal(map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": title, "text": text},
		"msgUuid":  msgUUID,
	})
}

func jsonBody(e Event) ([]byte, error) { return json.Marshal(e) }

// DingTalkSignedURL implements DingTalk's documented robot signature:
// HMAC-SHA256(key=secret, data="{timestamp}\n{secret}"), Base64, then URL encode.
func DingTalkSignedURL(raw, secret string, timestampMS int64) (string, error) {
	if err := ValidateURL(raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestampMS, 10) + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	q := u.Query()
	q.Set("timestamp", strconv.FormatInt(timestampMS, 10))
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func newMessageUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func buildRequest(dst Destination, event Event, msgUUID string, now time.Time) (*http.Request, error) {
	if err := ValidateDestination(dst); err != nil {
		return nil, err
	}
	event = sanitize(event)
	rawURL := dst.URL
	var body []byte
	var err error
	switch dst.Format {
	case FormatFeishu:
		body, err = feishuBody(event, dst.Keyword)
	case FormatDingTalk:
		body, err = dingtalkBody(event, dst.Keyword, msgUUID)
		if err == nil && dst.Secret != "" {
			rawURL, err = DingTalkSignedURL(rawURL, dst.Secret, now.UnixMilli())
		}
	case FormatJSON:
		body, err = jsonBody(event)
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// Resolver is the subset of net.Resolver used by the SSRF guard.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type netResolver struct{ *net.Resolver }

// Options configures deterministic tests. Production defaults always apply the
// private-address guard both before the request and at the actual dial.
type Options struct {
	Client       *http.Client
	Resolver     Resolver
	Now          func() time.Time
	MaxAttempts  int
	RetryBackoff time.Duration
}

// Sender applies account-wide rate limiting, SSRF protection, and retries.
type Sender struct {
	client       *http.Client
	resolver     Resolver
	now          func() time.Time
	maxAttempts  int
	retryBackoff time.Duration
	limiter      *limiter
}

// NewSender builds the relay-only sender. Defaults are a five-second timeout
// and three total attempts (the initial request plus two retries).
func NewSender(opts Options) *Sender {
	resolver := opts.Resolver
	if resolver == nil {
		resolver = netResolver{net.DefaultResolver}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	attempts := opts.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := opts.RetryBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	client := opts.Client
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = checkedDialContext(resolver)
		client = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	return &Sender{
		client: client, resolver: resolver, now: now,
		maxAttempts: attempts, retryBackoff: backoff,
		limiter: newLimiter(15, time.Minute),
	}
}

// Send posts an event. A DingTalk msgUuid is generated once and reused by all
// retries, preventing a lost response from duplicating a group message.
func (s *Sender) Send(ctx context.Context, dst Destination, event Event) (Result, error) {
	now := s.now()
	allowed, throttleNotice := s.limiter.allow(event.Namespace, now)
	dropped := !allowed
	if !allowed {
		if !throttleNotice {
			return Result{Dropped: true, Event: event.Event}, nil
		}
		event = Event{
			Event: "notify.throttled", Namespace: event.Namespace, Device: event.Device, TS: now,
			Message: "wanctl notifications throttled after 15 events in one minute",
		}
	}
	if err := ValidateDestinationURL(ctx, dst.URL, s.resolver); err != nil {
		return Result{Dropped: dropped, Event: event.Event}, err
	}
	msgUUID, err := newMessageUUID()
	if err != nil {
		return Result{Dropped: dropped, Event: event.Event}, err
	}
	result := Result{Dropped: dropped, Event: event.Event}
	var lastErr error
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		req, err := buildRequest(dst, event, msgUUID, now)
		if err != nil {
			return result, err
		}
		req = req.WithContext(ctx)
		resp, err := s.client.Do(req)
		result.Attempts = attempt
		retry := true
		if err == nil {
			result.HTTPStatus = resp.StatusCode
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else {
				lastErr, retry = interpretResponse(dst.Format, resp.StatusCode, body, &result)
				if lastErr == nil {
					return result, nil
				}
			}
		} else {
			lastErr = err
		}
		if !retry || attempt == s.maxAttempts {
			break
		}
		timer := time.NewTimer(s.retryBackoff << (attempt - 1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
	return result, lastErr
}

func interpretResponse(format string, status int, body []byte, result *Result) (error, bool) {
	if status < 200 || status > 299 {
		return fmt.Errorf("webhook returned HTTP %d: %s", status, truncate(string(body), 200)), status >= 500 || status == http.StatusTooManyRequests
	}
	switch format {
	case FormatJSON:
		return nil, false
	case FormatFeishu:
		var response struct {
			Code *int   `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("Feishu returned invalid JSON: %s", truncate(string(body), 200)), false
		}
		if response.Code != nil {
			result.ProviderCode = strconv.Itoa(*response.Code)
			result.ProviderMessage = response.Msg
			if *response.Code != 0 {
				return fmt.Errorf("Feishu code %d: %s", *response.Code, response.Msg), false
			}
		}
		return nil, false
	case FormatDingTalk:
		var response struct {
			ErrCode *int   `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("DingTalk returned invalid JSON: %s", truncate(string(body), 200)), false
		}
		if response.ErrCode == nil {
			return errors.New("DingTalk response is missing errcode"), false
		}
		result.ProviderCode = strconv.Itoa(*response.ErrCode)
		result.ProviderMessage = response.ErrMsg
		if *response.ErrCode != 0 {
			return fmt.Errorf("DingTalk errcode %d: %s", *response.ErrCode, response.ErrMsg), false
		}
		return nil, false
	default:
		return fmt.Errorf("unsupported webhook format %q", format), false
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}

// ValidateDestinationURL resolves an HTTPS target and rejects addresses a
// public relay must not reach.
func ValidateDestinationURL(ctx context.Context, rawURL string, resolver Resolver) error {
	if err := ValidateURL(rawURL); err != nil {
		return err
	}
	u, _ := url.Parse(rawURL)
	_, err := checkedIPs(ctx, resolver, u.Hostname())
	return err
}

func checkedIPs(ctx context.Context, resolver Resolver, host string) ([]net.IPAddr, error) {
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		var err error
		ips, err = resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve webhook host: %w", err)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("webhook host resolved to no addresses")
	}
	for _, addr := range ips {
		if forbiddenIP(addr.IP) {
			return nil, fmt.Errorf("webhook host resolves to forbidden address %s", addr.IP)
		}
	}
	return ips, nil
}

var carrierNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func forbiddenIP(ip net.IP) bool {
	return ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || carrierNAT.Contains(ip)
}

func checkedDialContext(resolver Resolver) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := checkedIPs(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, addr := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

type limitWindow struct {
	start     time.Time
	count     int
	announced bool
}

type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	keys   map[string]limitWindow
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, keys: map[string]limitWindow{}}
}

func (l *limiter) allow(key string, now time.Time) (allowed, throttleNotice bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.keys[key]
	if w.start.IsZero() || now.Sub(w.start) >= l.window || now.Before(w.start) {
		w = limitWindow{start: now}
	}
	if w.count < l.limit {
		w.count++
		l.keys[key] = w
		return true, false
	}
	if !w.announced {
		w.announced = true
		l.keys[key] = w
		return false, true
	}
	l.keys[key] = w
	return false, false
}
