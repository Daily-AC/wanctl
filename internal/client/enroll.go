package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// Instance is what a portal reveals about the deployment it fronts, so a
// client that was only told the portal can find the relay by itself.
type Instance struct {
	Relay     string // public relay origin, e.g. https://relay.example.com
	Transport string // "ws" or "http"; empty when the portal did not say
}

// DiscoverInstance asks the portal at origin where its relay is
// (GET /api/instance, no session needed). Used when a front-end collected a
// portal but has no way to collect a relay — the Android enrollment dialog —
// and by `wanctl login` when only the portal was configured.
func DiscoverInstance(ctx context.Context, portal string) (Instance, error) {
	origin := strings.TrimRight(strings.TrimSpace(portal), "/")
	req, err := http.NewRequestWithContext(ctx, "GET", origin+"/api/instance", nil)
	if err != nil {
		return Instance{}, err
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return Instance{}, fmt.Errorf("连接门户失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Instance{}, fmt.Errorf("门户 %s 不支持自动发现中继（门户版本过旧），请手动配置 relay", origin)
	}
	if resp.StatusCode != http.StatusOK {
		return Instance{}, fmt.Errorf("门户 %s 未能给出中继地址 (HTTP %d)", origin, resp.StatusCode)
	}
	var out struct {
		Relay     string `json:"relay"`
		Transport string `json:"transport"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return Instance{}, fmt.Errorf("门户回应无法解析: %w", err)
	}
	u, err := url.Parse(out.Relay)
	if err != nil || u.Host == "" {
		return Instance{}, fmt.Errorf("门户给出的中继地址无效: %q", out.Relay)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return Instance{}, fmt.Errorf("门户给出的中继地址无效: %q", out.Relay)
	}
	switch out.Transport {
	case "", "ws", "http":
	default:
		return Instance{}, fmt.Errorf("门户给出的传输方式无效: %q", out.Transport)
	}
	return Instance{Relay: out.Relay, Transport: out.Transport}, nil
}
