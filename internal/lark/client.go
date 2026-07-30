// Package lark provides the dependency-free outbound Feishu API surface used
// by the portal's approval workflow.
package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://open.feishu.cn"
	tokenPath      = "/open-apis/auth/v3/tenant_access_token/internal"
	tokenRefresh   = 5 * time.Minute
)

// Client sends and updates interactive cards through Feishu's HTTP API.
// HTTPClient and BaseURL may be replaced before first use, primarily for tests.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string

	appID     string
	appSecret string
	now       func() time.Time

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewClient constructs a client using Feishu's public API endpoint.
func NewClient(appID, appSecret string) *Client {
	return &Client{
		HTTPClient: http.DefaultClient,
		BaseURL:    defaultBaseURL,
		appID:      appID,
		appSecret:  appSecret,
		now:        time.Now,
	}
}

// SendCard sends a V2 interactive card to the 1:1 chat resolved from email.
func (c *Client) SendCard(ctx context.Context, email string, card any) (messageID, chatID string, err error) {
	content, err := json.Marshal(card)
	if err != nil {
		return "", "", fmt.Errorf("marshal card: %w", err)
	}
	body := struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{
		ReceiveID: email,
		MsgType:   "interactive",
		Content:   string(content),
	}
	var result struct {
		Data struct {
			MessageID string `json:"message_id"`
			ChatID    string `json:"chat_id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=email", body, true, &result); err != nil {
		return "", "", err
	}
	if result.Data.MessageID == "" || result.Data.ChatID == "" {
		return "", "", fmt.Errorf("lark send card: response missing message_id or chat_id")
	}
	return result.Data.MessageID, result.Data.ChatID, nil
}

// UpdateCard replaces an existing message's complete card content.
func (c *Client) UpdateCard(ctx context.Context, messageID string, card any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	body := struct {
		Content string `json:"content"`
	}{Content: string(content)}
	path := "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	return c.do(ctx, http.MethodPatch, path, body, true, nil)
}

func (c *Client) tenantToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	now := c.now()
	if c.token != "" && now.Before(c.tokenExpiry) {
		return c.token, nil
	}
	body := struct {
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
	}{AppID: c.appID, AppSecret: c.appSecret}
	var result struct {
		Token  string `json:"tenant_access_token"`
		Expire int64  `json:"expire"`
	}
	if err := c.do(ctx, http.MethodPost, tokenPath, body, false, &result); err != nil {
		return "", err
	}
	if result.Token == "" || result.Expire <= 0 {
		return "", fmt.Errorf("lark tenant token: response missing token or valid expiry")
	}
	refreshAfter := time.Duration(result.Expire)*time.Second - tokenRefresh
	if refreshAfter < 0 {
		refreshAfter = 0
	}
	c.token = result.Token
	c.tokenExpiry = now.Add(refreshAfter)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, auth bool, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal lark request: %w", err)
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create lark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if auth {
		token, err := c.tenantToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lark %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	responsePayload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read lark response: %w", err)
	}
	var envelope struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(responsePayload, &envelope); err != nil {
		return fmt.Errorf("decode lark response: %w", err)
	}
	if envelope.Code == nil {
		return fmt.Errorf("decode lark response: missing code")
	}
	if *envelope.Code != 0 {
		return fmt.Errorf("lark API error %d: %s", *envelope.Code, envelope.Msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(responsePayload, out); err != nil {
		return fmt.Errorf("decode lark response: %w", err)
	}
	return nil
}
