package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"wanctl/internal/client"
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
	en, err := client.ExchangeCode(ctx, relay, code)
	if err != nil {
		return "", err
	}
	return applyEnrollment(en), nil
}

// applyEnrollment lands everything a successful exchange gives us. Split from
// enroll so the parts that are not stdin and network stay testable.
func applyEnrollment(en client.Enrollment) string {
	fmt.Printf("✓ 已绑定到空间 \"%s\"\n", en.Namespace)
	seedPortalAdmin(en.PortalFP)
	return en.Token
}

// seedPortalAdmin trusts the portal identity that came back with the enrollment,
// so the team portal's web console can reach this device without anyone passing
// --portal-fps by hand. Only at enrollment: an already-enrolled device never
// gains a console administrator from the wire, and the fingerprint is printed
// for comparison with the one the enroll page showed, because it travelled
// through the relay and a compromised relay could otherwise nominate itself.
func seedPortalAdmin(fp string) {
	if fp == "" {
		return
	}
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		fmt.Fprintf(os.Stderr, "! 无法打开门户管理员列表: %v\n", err)
		return
	}
	if admins.Contains(fp) {
		return
	}
	if err := admins.Add(fp); err != nil {
		fmt.Fprintf(os.Stderr, "! 信任门户身份失败: %v\n  网页控制台将连不上本机，可稍后手动执行: wanctl portal-admins add %s\n", err, fp)
		return
	}
	fmt.Printf("✓ 已信任门户身份 %s\n  （请核对它与授权网页上显示的一致）\n", fp)
}

// cmdLogin is the controller-side OAuth entrypoint: opens the portal, exchanges
// the pasted code for a namespace token, and stores it locally — without
// starting the device daemon. Used by AI controllers and humans who only need
// to drive other devices from this machine. Bare `wanctl` (no args) is still
// the device path (enroll → save → daemon).
func cmdLogin(ctx context.Context) error {
	if tok := os.Getenv("WANCTL_TOKEN"); tok != "" {
		fmt.Println("已通过 WANCTL_TOKEN 环境变量提供凭证；如需重新授权，先 unset 该变量再运行 wanctl login。")
		return nil
	}
	if existing := config.StoredToken(); existing != "" {
		fmt.Println("(覆盖已存在的本地凭证)")
	}
	t, err := enroll(ctx)
	if err != nil {
		return err
	}
	if err := config.SaveToken(t); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	dir, _ := config.TokenPath()
	fmt.Printf("✓ 凭证已保存到 %s\n", dir)
	fmt.Println("现在可以用 wanctl peers / wanctl exec / wanctl push / wanctl pull 控制你授权的设备。")
	return nil
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
