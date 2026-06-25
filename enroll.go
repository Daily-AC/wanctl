package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"wanctl/internal/config"
)

// enroll runs the OAuth device-enrollment flow: open the portal in a browser
// (Feishu SSO authenticates the human and shows a one-time code), read the code
// the user pastes back, and exchange it at the relay for a namespace-scoped
// token. Returns the token; the caller persists it. Claude-code-style.
func enroll(ctx context.Context) (string, error) {
	portal := config.EnvOr("WANCTL_PORTAL", config.DefaultPortal)
	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	enrollURL := strings.TrimRight(portal, "/") + "/enroll"

	fmt.Println("正在将本设备授权到 wanctl 空间…")
	fmt.Printf("→ 浏览器打开: %s （飞书登录后会显示一次性授权 code）\n", enrollURL)
	openBrowser(enrollURL)

	fmt.Print("输入授权 code: ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		return "", fmt.Errorf("未输入 code")
	}

	fmt.Println("正在验证…")
	token, ns, err := exchangeCode(ctx, relay, code)
	if err != nil {
		return "", err
	}
	fmt.Printf("✓ 已绑定到空间 \"%s\"\n", ns)
	return token, nil
}

// exchangeCode trades a one-time enrollment code for a token at the relay.
func exchangeCode(ctx context.Context, relay, code string) (token, namespace string, err error) {
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

// openBrowser best-effort opens url in the platform browser. Failure is fine —
// the user can open the printed URL manually.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
