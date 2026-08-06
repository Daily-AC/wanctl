package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Enrollment is what the relay hands back for a valid one-time code.
type Enrollment struct {
	Token     string
	Namespace string
	// PortalFP is the portal's own controller fingerprint, declared by the
	// portal when it minted the code. A device seeds it as a console
	// administrator so the team portal can reach it without anyone having to
	// pass --portal-fps by hand. It travels through the relay, so it is echoed
	// to the human alongside the code for comparison rather than trusted
	// silently: the relay could otherwise nominate itself as console admin on
	// devices enrolling from now on.
	PortalFP string
}

// ExchangeCode trades a one-time enrollment code (minted by the portal /enroll
// page after Feishu SSO) for a namespace token at the relay's public exchange
// endpoint. Shared by the CLI `wanctl login`, the MCP `wanctl_login` tool, and
// anything else that needs to drive the OAuth-style device-code flow.
func ExchangeCode(ctx context.Context, relay, code string) (Enrollment, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, "POST", relay+"/enroll/exchange", bytes.NewReader(body))
	if err != nil {
		return Enrollment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return Enrollment{}, fmt.Errorf("连接 relay 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Enrollment{}, fmt.Errorf("授权失败（code 无效或已过期，请重新获取）")
	}
	var out struct {
		Token     string `json:"token"`
		Namespace string `json:"namespace"`
		PortalFP  string `json:"portal_fp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Enrollment{}, err
	}
	if out.Token == "" {
		return Enrollment{}, fmt.Errorf("relay 未返回 token")
	}
	return Enrollment{Token: out.Token, Namespace: out.Namespace, PortalFP: out.PortalFP}, nil
}
