package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"wanctl/internal/client"
	"wanctl/internal/config"
)

// cmdConfig shows or edits the persisted endpoint settings. Precedence at use
// time: command-line flag > environment variable > this config > build default.
func cmdConfig(args []string) error {
	if len(args) == 0 {
		return configShow()
	}
	switch args[0] {
	case "set":
		return configSet(args[1:])
	case "unset":
		return configUnset(args[1:])
	case "-h", "--help", "help":
		fmt.Println(configHelp)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], configHelp)
	}
}

const configHelp = `wanctl config                      查看生效配置与来源
wanctl config set key=value ...    持久化设置 (relay / portal / transport / release_base)
wanctl config unset key ...        删除持久化设置

例:
  wanctl config set relay=https://relay.example.com portal=https://portal.example.com
  wanctl config set transport=ws
  wanctl config set release_base=https://your-relay.example.com/dl
  wanctl config unset transport

优先级: 命令行 flag > 环境变量 > 这里的配置 > 编译期默认值。`

var configKeys = []string{"relay", "portal", "transport", "release_base"}

type kv struct{ k, v string }

func configShow() error {
	for _, k := range configKeys {
		v, source := config.Setting(k)
		if v == "" {
			fmt.Printf("%-12s = (未设置)\n", k)
			continue
		}
		note := source
		if stored := config.StoredSetting(k); stored != "" && stored != v {
			note += fmt.Sprintf("，覆盖了配置文件里的 %s", stored)
		}
		fmt.Printf("%-12s = %s  (%s)\n", k, v, note)
	}
	if dir := config.SettingsDir(); dir != "" {
		fmt.Printf("配置目录: %s\n", dir)
	}
	return nil
}

func configSet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wanctl config set key=value ...\n%s", configHelp)
	}
	var pending []kv
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return fmt.Errorf("want key=value, got %q", a)
		}
		if err := validateSetting(k, v); err != nil {
			return err
		}
		pending = append(pending, kv{k, v})
	}
	// Validate everything before writing anything, so a bad pair can't leave a
	// half-applied config.
	for _, p := range pending {
		if err := config.SaveSetting(p.k, p.v); err != nil {
			return err
		}
		fmt.Printf("✓ %s = %s\n", p.k, p.v)
	}
	warnEnvShadow(pending)
	return nil
}

func configUnset(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wanctl config unset key ...")
	}
	for _, k := range args {
		if !config.KnownSetting(k) {
			return fmt.Errorf("unknown setting %q (known: %s)", k, strings.Join(configKeys, ", "))
		}
		if err := config.RemoveSetting(k); err != nil {
			return err
		}
		fmt.Printf("✓ 已删除 %s\n", k)
	}
	return nil
}

// validateSetting rejects values that would fail at first use. relay accepts
// ws(s) as well as http(s): device units dial wss:// relays directly.
func validateSetting(key, value string) error {
	switch key {
	case "relay", "portal":
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			return fmt.Errorf("%s: %q is not a URL", key, value)
		}
		schemes := "http/https"
		okScheme := u.Scheme == "http" || u.Scheme == "https"
		if key == "relay" {
			schemes = "http/https/ws/wss"
			okScheme = okScheme || u.Scheme == "ws" || u.Scheme == "wss"
		}
		if !okScheme {
			return fmt.Errorf("%s: scheme %q not supported (want %s)", key, u.Scheme, schemes)
		}
		return nil
	case "release_base":
		// Where `wanctl update` fetches signed artifacts from. Only http(s):
		// the download code speaks nothing else, and the installers refuse
		// anything but https outright.
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			return fmt.Errorf("release_base: %q is not a URL", value)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("release_base: scheme %q not supported (want http/https)", u.Scheme)
		}
		return nil
	case "transport":
		if value != "ws" && value != "http" {
			return fmt.Errorf("transport: want ws or http, got %q", value)
		}
		return nil
	default:
		return fmt.Errorf("unknown setting %q (known: %s)", key, strings.Join(configKeys, ", "))
	}
}

// warnEnvShadow points out when an environment variable will keep overriding
// what was just saved — otherwise `config set` looks ignored.
func warnEnvShadow(pending []kv) {
	envKey := map[string]string{
		"relay": "WANCTL_RELAY", "portal": "WANCTL_PORTAL",
		"transport": "WANCTL_TRANSPORT", "release_base": "WANCTL_RELEASE_BASE",
	}
	for _, p := range pending {
		if ev := os.Getenv(envKey[p.k]); ev != "" && ev != p.v {
			fmt.Fprintf(os.Stderr, "注意: 环境变量 %s=%s 仍会覆盖刚保存的值\n", envKey[p.k], ev)
		}
	}
}

// ensureEndpointsConfigured makes sure relay and portal resolve before an
// enroll/login flow needs them. On a terminal it asks once and persists the
// answers; anywhere else it fails with the exact command to run, because a
// hidden prompt would hang scripts and AI-driven shells.
func ensureEndpointsConfigured() error {
	_, relayErr := config.Relay()
	portal, portalErr := config.Portal()
	if relayErr != nil && portalErr == nil {
		// Half configured, and the half we have can name the other: the
		// portal publishes its relay. This is the only path on Android, where
		// the enrollment dialog collects a portal and nothing can collect a
		// relay (GitHub issue #3); on a desktop it just saves a question.
		if err := discoverRelayFromPortal(portal); err != nil {
			if runtime.GOOS == "android" {
				return err
			}
			fmt.Printf("未能从门户自动发现中继（%v），改为手动配置。\n", err)
		} else {
			relayErr = nil
		}
	}
	if relayErr == nil && portalErr == nil {
		return nil
	}
	if runtime.GOOS == "android" {
		// There is no shell to run the command in; the app's dialog is the
		// only place a portal can be typed.
		return fmt.Errorf("还没配置门户地址：在登录对话框里填写门户地址后重试")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("还没配置实例地址：运行 `wanctl config set relay=https://你的中继 portal=https://你的门户` 后重试")
	}
	fmt.Println("首次使用，先告诉我你的实例在哪（之后 `wanctl config` 可随时查看/修改）：")
	in := bufio.NewReader(os.Stdin)
	if relayErr != nil {
		v, err := promptSetting(in, "relay", "中继地址 (例 https://relay.example.com): ")
		if err != nil {
			return err
		}
		if err := config.SaveSetting("relay", v); err != nil {
			return err
		}
	}
	if portalErr != nil {
		v, err := promptSetting(in, "portal", "门户地址 (例 https://portal.example.com): ")
		if err != nil {
			return err
		}
		if err := config.SaveSetting("portal", v); err != nil {
			return err
		}
	}
	if dir := config.SettingsDir(); dir != "" {
		fmt.Printf("✓ 已保存到 %s\n", dir)
	}
	return nil
}

// discoverRelayFromPortal asks the portal for its relay and persists it as
// the `relay` setting, so every later command — including the agent the app
// starts next — finds it in the config file without being told again.
func discoverRelayFromPortal(portal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inst, err := client.DiscoverInstance(ctx, portal)
	if err != nil {
		return err
	}
	if err := config.SaveSetting("relay", inst.Relay); err != nil {
		return err
	}
	fmt.Printf("✓ 从门户发现中继 %s，已保存\n", inst.Relay)
	// The transport is only persisted when the portal named one and nothing
	// local already decided it; the build default is otherwise good enough
	// and a stored value would shadow a later change of that default.
	if inst.Transport != "" && config.StoredSetting("transport") == "" && os.Getenv("WANCTL_TRANSPORT") == "" {
		if inst.Transport != config.DefaultTransport {
			if err := config.SaveSetting("transport", inst.Transport); err != nil {
				return err
			}
		}
	}
	return nil
}

func promptSetting(in *bufio.Reader, key, prompt string) (string, error) {
	for tries := 0; tries < 3; tries++ {
		fmt.Print(prompt)
		line, err := in.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("读取输入失败: %w", err)
		}
		v := strings.TrimSpace(line)
		if v == "" {
			return "", fmt.Errorf("已取消；之后可用 `wanctl config set %s=...` 配置", key)
		}
		if err := validateSetting(key, v); err != nil {
			fmt.Println(err.Error())
			continue
		}
		return v, nil
	}
	return "", fmt.Errorf("连续三次输入无效；用 `wanctl config set %s=...` 配置", key)
}
