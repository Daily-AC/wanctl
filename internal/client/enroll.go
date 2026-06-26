package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ExchangeCode trades a one-time enrollment code (minted by the portal /enroll
// page after Feishu SSO) for a namespace token at the relay's public exchange
// endpoint. Shared by the CLI `wanctl login`, the MCP `wanctl_login` tool, and
// anything else that needs to drive the OAuth-style device-code flow.
func ExchangeCode(ctx context.Context, relay, code string) (token, namespace string, err error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, "POST", relay+"/enroll/exchange", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("连接 relay 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("授权失败（code 无效或已过期，请重新获取）")
	}
	var out struct{ Token, Namespace string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("relay 未返回 token")
	}
	return out.Token, out.Namespace, nil
}
