// Command wanctl is a cross-internet remote-control CLI. The same binary runs as
// the relay (`wanctl relay`, on thunderbox), the controlled device
// (`wanctl agent`), and the controller (`wanctl exec/push/pull`). Endpoints meet
// through the relay's WebSocket broker and speak end-to-end mutual TLS, so the
// relay only sees ciphertext.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/eventlog"
	mcppkg "wanctl/internal/mcp"
	"wanctl/internal/policy"
	"wanctl/internal/portal"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

const usage = `wanctl — control a device across the internet over an encrypted, relayed channel

USAGE
 DEVICE LIFECYCLE (run on the box you want to control)
  wanctl                                      log in (Feishu) if needed, then run the agent detached in the background
  wanctl start                                (re)start the background agent without re-login; records its pid
  wanctl stop                                 stop the background agent
  wanctl status                               show whether the agent is running + credential state
  wanctl logout                               stop the agent and forget the saved login
  wanctl agent [flags]                        run the agent in the FOREGROUND (what 'wanctl'/'start' spawn; use this for a service/Task unit)
  Persistence: the background agent survives this terminal closing but NOT a reboot.
  For reboot-survival, run 'wanctl agent' from a systemd unit / Windows Scheduled Task (the installer can set this up).

 CONTROLLER (run where you / the AI drive from)
  wanctl login                                log in (Feishu) and save the token — no daemon (use this on AI / controller boxes)
  wanctl update                               download the latest binary from the relay and swap it in
  wanctl mcp                                  run a stdio MCP server (per-process, single-user) for an AI host's child process
  wanctl mcp --http :ADDR                     run a public HTTP/Streamable MCP server (multi-user; needs WANCTL_MCP_SEED env)
  wanctl docs ls [--group SLUG]               list documentation articles
  wanctl docs get <slug>                      print one article's body
  wanctl docs new --slug S --title T --group G [--file F | --editor | < stdin]
  wanctl docs edit <slug> [--file F | --editor | < stdin]
  wanctl docs rm <slug>
  wanctl docs groups                          list documentation groups
  wanctl docs group new --slug S --title T [--position N]
  wanctl docs group rm <slug>
  wanctl exec  [--target NS/DEV] [--oneshot] <command...>
  wanctl push  [--target NS/DEV] <local> <remote>
  wanctl pull  [--target NS/DEV] <remote> <local>
  wanctl peers
  wanctl id
  wanctl pair  <device>                       check device trust state; if not yet paired print the URL the device owner clicks to approve
  wanctl trust [clients|servers]
  wanctl agent [--name N] [--relay URL] [--token T] [--yes] [--shell S] [--portal-pk FP]
  wanctl relay  [--addr :8080]                run the relay (thunderbox); DATABASE_URL or WANCTL_TOKENS
  wanctl portal [--addr :8080]                run the team portal (thunderbox, internal SSO)

Defaults: relay=` + defaultRelay + `  transport=` + defaultTransport + ` (override with WANCTL_RELAY/WANCTL_TRANSPORT)
ENV (controller): WANCTL_TOKEN=... (or run 'wanctl' to log in)  WANCTL_RELAY=...
ENV (relay):      WANCTL_TOKENS="token:namespace,token2:ns2"  WANCTL_ADMIN_SECRET=...  WANCTL_PORTAL_NS=...
ENV (portal):     RELAY_ADMIN_URL=...  WANCTL_ADMIN_SECRET=...  PORTAL_USER_HEADER=...
              WANCTL_RELAY=...  WANCTL_PORTAL_TOKEN=...  WANCTL_TRANSPORT=http
ENV (agent):      WANCTL_PORTAL_PK=SHA256:...
`

// Compile-time defaults live in internal/config so the controller package shares
// them. Aliased here for terse use in flag definitions.
const (
	defaultRelay     = config.DefaultRelay
	defaultTransport = config.DefaultTransport
	defaultPortal    = config.DefaultPortal
)

func main() {
	if len(os.Args) < 2 {
		// Bare `wanctl`: onboard if needed, then ensure the agent runs in the
		// background — the claude-code-style "just works" entrypoint.
		if err := cmdUp(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "wanctl: "+err.Error())
			os.Exit(1)
		}
		return
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "relay":
		err = cmdRelay(os.Args[2:])
	case "portal":
		err = cmdPortal(os.Args[2:])
	case "agent":
		err = cmdAgent(ctx, os.Args[2:])
	case "exec":
		err = cmdExec(ctx, os.Args[2:])
	case "push":
		err = cmdPush(ctx, os.Args[2:])
	case "pull":
		err = cmdPull(ctx, os.Args[2:])
	case "peers":
		err = cmdPeers(ctx)
	case "id":
		err = cmdID()
	case "pair":
		err = cmdPair(ctx, os.Args[2:])
	case "trust":
		err = cmdTrust(os.Args[2:])
	case "rules":
		err = cmdRules(os.Args[2:])
	case "logs":
		err = cmdLogs(ctx, os.Args[2:])
	case "up":
		err = cmdUp(ctx)
	case "login":
		err = cmdLogin(ctx)
	case "docs":
		err = cmdDocs(ctx, os.Args[2:])
	case "start":
		err = cmdStart()
	case "stop":
		err = cmdStop()
	case "status":
		err = cmdStatus()
	case "logout":
		err = cmdLogout()
	case "update":
		err = cmdUpdate(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wanctl: "+err.Error())
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func cmdRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)

	var r *relay.Relay
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pg, err := relay.OpenPG(dsn)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		r = relay.New(pg)
		r.SetACL(pg)
		r.SetAuditor(pg)
		r.SetAdmin(pg)
		r.SetDocs(pg)
		if sec := os.Getenv("WANCTL_ADMIN_SECRET"); sec != "" {
			r.SetAdminSecret(sec)
			fmt.Println("wanctl relay: admin API enabled (portal access)")
		}
		fmt.Println("wanctl relay: token store = postgres (hashed tokens + ACL + audit)")
	} else {
		spec := os.Getenv("WANCTL_TOKENS")
		if spec == "" {
			return fmt.Errorf("set DATABASE_URL (postgres) or WANCTL_TOKENS=\"token:namespace,...\"")
		}
		r = relay.New(relay.EnvTokenStore(spec))
		fmt.Println("wanctl relay: token store = env (WANCTL_TOKENS)")
	}
	if pns := os.Getenv("WANCTL_PORTAL_NS"); pns != "" {
		r.SetPortalNS(pns)
	}
	if seedHex := os.Getenv("WANCTL_MCP_SEED"); seedHex != "" {
		seed, err := hex.DecodeString(seedHex)
		if err != nil {
			return fmt.Errorf("WANCTL_MCP_SEED must be hex-encoded: %w", err)
		}
		h, err := mcppkg.Handler(seed, "/wanctl-mcp")
		if err != nil {
			return fmt.Errorf("mcp handler: %w", err)
		}
		r.SetMCPHandler(h)
		fmt.Println("wanctl relay: MCP server enabled at /wanctl-mcp (Streamable HTTP)")
	}
	fmt.Printf("wanctl relay listening on %s\n", *addr)
	return http.ListenAndServe(*addr, r.Handler())
}

func cmdPortal(args []string) error {
	fs := flag.NewFlagSet("portal", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return err
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		return err
	}
	p := portal.New(portal.Config{
		RelayAdminURL: os.Getenv("RELAY_ADMIN_URL"),
		AdminSecret:   os.Getenv("WANCTL_ADMIN_SECRET"),
		UserHeader:    os.Getenv("PORTAL_USER_HEADER"),
		RelayDialURL:  os.Getenv("WANCTL_RELAY"),
		PortalToken:   os.Getenv("WANCTL_PORTAL_TOKEN"),
		Transport:     envOr("WANCTL_TRANSPORT", "http"),
		Identity:      id,
		Known:         known,
	})
	fmt.Printf("wanctl portal on %s\n  identity:      %s\n  identity hdr:  %q\n  relay(admin):  %q\n  relay(dial):   %q\n",
		*addr, id.Fingerprint, envOr("PORTAL_USER_HEADER", "X-Auth-Request-Email"),
		os.Getenv("RELAY_ADMIN_URL"), os.Getenv("WANCTL_RELAY"))
	return http.ListenAndServe(*addr, p.Handler())
}

func cmdAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	name := fs.String("name", "", "device name (default hostname)")
	relayURL := fs.String("relay", envOr("WANCTL_RELAY", defaultRelay), "relay ws(s) URL")
	token := fs.String("token", envOr("WANCTL_TOKEN", config.StoredToken()), "access/registration token")
	shell := fs.String("shell", "", "shell (default powershell on Windows, /bin/sh elsewhere)")
	yes := fs.Bool("yes", false, "auto-trust new controllers (unattended)")
	tr := fs.String("transport", envOr("WANCTL_TRANSPORT", defaultTransport), "transport: ws or http (http is proxy-agnostic)")
	mode := fs.String("mode", "", "policy mode: normal (prompt on miss) or bypass (auto-allow, DANGEROUS). Empty = keep the last persisted mode (default normal).")
	portalPK := fs.String("portal-pk", envOr("WANCTL_PORTAL_PK", config.DefaultPortalFP), "pre-trust this portal fingerprint (enrolled at install time)")
	fs.Parse(args)
	if *relayURL == "" || *token == "" {
		return fmt.Errorf("provide --relay and --token (or WANCTL_RELAY/WANCTL_TOKEN)")
	}
	ag, err := agent.New(agent.Options{RelayURL: *relayURL, Token: *token, Name: *name, Shell: *shell, AutoYes: *yes, Transport: *tr, Mode: policy.Mode(*mode), PortalFP: *portalPK})
	if err != nil {
		return err
	}
	// Warn on the EFFECTIVE mode (which may be a persisted bypass, not just an
	// explicit --mode bypass flag).
	if ag.Mode() == policy.ModeBypass {
		fmt.Fprintln(os.Stderr, "wanctl: BYPASS mode — every command and file op is auto-allowed. Use only on trusted, isolated devices.")
	}
	// Self-register the pid so `wanctl status`/`stop` see this agent no matter how
	// it was launched (bare `wanctl`, a keeper task, a systemd/launchd service),
	// not just the child that `wanctl start` spawns.
	_ = config.WritePID(os.Getpid())
	defer config.RemovePID()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ag.Run(ctx)
}

func cmdExec(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	target := fs.String("target", "", "device (NS/DEV or DEV)")
	oneShot := fs.Bool("oneshot", false, "fresh shell, no session state")
	cwd := fs.String("cwd", "", "working directory on the device (also the policy scope)")
	fs.Parse(args)
	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if command == "" {
		return fmt.Errorf("no command given")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	code, err := c.Exec(ctx, *target, command, *oneShot, *cwd)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	target := fs.String("target", "", "device")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: wanctl push <local> <remote>")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	return c.Push(ctx, *target, fs.Arg(0), fs.Arg(1))
}

func cmdPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	target := fs.String("target", "", "device")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: wanctl pull <remote> <local>")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	return c.Pull(ctx, *target, fs.Arg(0), fs.Arg(1))
}

func cmdPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	target := fs.String("target", "", "device (NS/DEV or DEV); positional <device> also accepted")
	fs.Parse(args)
	if *target == "" && fs.NArg() > 0 {
		*target = fs.Arg(0)
	}
	if *target == "" {
		return fmt.Errorf("usage: wanctl pair <device>")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	trusted, pairingURL, err := c.Pair(ctx, *target)
	if err != nil {
		return err
	}
	if trusted {
		fmt.Printf("✓ %s 已经信任本机, 无需操作. 直接 `wanctl exec --target %s ...` 即可.\n", *target, *target)
		return nil
	}
	fmt.Printf("待审批 — 把下面这条链接交给 %s 的所有者, 他在浏览器打开并点「信任并继续」即可:\n\n  %s\n\n之后再跑 `wanctl exec/push/pull` 就能通了 (链接 5 分钟内有效).\n", *target, pairingURL)
	return nil
}

func cmdPeers(ctx context.Context) error {
	c, err := client.New()
	if err != nil {
		return err
	}
	devs, err := c.Peers(ctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no devices online for this token")
		return nil
	}
	for _, d := range devs {
		fmt.Println(d)
	}
	return nil
}

func cmdID() error {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return err
	}
	dir, _ := transport.ConfigDir()
	fmt.Printf("fingerprint: %s\nconfig dir:  %s\n", id.Fingerprint, dir)
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	target := fs.String("target", "", "pull from this device over the relay (omit to read local device log)")
	logType := fs.String("type", "", "filter: connect | exec | file")
	grep := fs.String("grep", "", "filter: substring of the detail field")
	since := fs.String("since", "", "filter: RFC3339 timestamp lower bound")
	limit := fs.Int("limit", 0, "keep only the last N matching events")
	fs.Parse(args)

	if *target != "" {
		c, err := client.New()
		if err != nil {
			return err
		}
		return c.Logs(ctx, *target, *logType, *grep, *since, *limit)
	}
	// Local read (run on the device itself).
	lg, err := eventlog.Open("events.jsonl")
	if err != nil {
		return err
	}
	f := eventlog.Filter{Type: *logType, Grep: *grep, Limit: *limit}
	if *since != "" {
		if ts, perr := time.Parse(time.RFC3339, *since); perr == nil {
			f.Since = ts
		}
	}
	events, err := lg.Read(f)
	if err != nil {
		return err
	}
	for _, e := range events {
		b, _ := json.Marshal(e)
		fmt.Println(string(b))
	}
	return nil
}

func cmdRules(args []string) error {
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list":
		rules := eng.List()
		fmt.Printf("policy rules: %d\n", len(rules))
		for i, r := range rules {
			scope := string(r.Scope)
			if r.Scope == policy.ScopeDir {
				scope = "dir:" + r.Dir
				if r.Kind != policy.KindExec {
					scope = "dir:" + r.Pattern
				}
			}
			fmt.Printf("  [%d] %-5s %-30q %s\n", i, r.Kind, r.Pattern, scope)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("rules add", flag.ExitOnError)
		kind := fs.String("kind", "exec", "exec | read | write")
		pattern := fs.String("pattern", "", "exec: command (+arg prefix, trailing * ok); file: directory")
		dir := fs.String("dir", "", "for exec dir-scope: the working directory")
		fs.Parse(args)
		if *pattern == "" && *dir == "" {
			return fmt.Errorf("usage: wanctl rules add --kind exec|read|write --pattern P [--dir D]")
		}
		r := policy.Rule{Kind: policy.Kind(*kind), Pattern: *pattern, Scope: policy.ScopeGlobal}
		switch policy.Kind(*kind) {
		case policy.KindExec:
			if *dir != "" { // exec dir-scope: command pattern restricted to a working dir
				r.Scope = policy.ScopeDir
				r.Dir = *dir
			}
		case policy.KindRead, policy.KindWrite:
			if *pattern != "" { // a file pattern is itself a directory restriction
				r.Scope = policy.ScopeDir
			}
		default:
			return fmt.Errorf("invalid --kind %q (want exec|read|write)", *kind)
		}
		if err := eng.Add(r); err != nil {
			return err
		}
		fmt.Println("rule added")
		return nil
	case "rm":
		if len(args) != 1 {
			return fmt.Errorf("usage: wanctl rules rm <index>")
		}
		var i int
		if _, err := fmt.Sscanf(args[0], "%d", &i); err != nil {
			return fmt.Errorf("invalid index %q", args[0])
		}
		if err := eng.Remove(i); err != nil {
			return fmt.Errorf("remove rule %d: %w", i, err)
		}
		fmt.Println("rule removed")
		return nil
	default:
		return fmt.Errorf("usage: wanctl rules [list|add|rm]")
	}
}

func cmdTrust(args []string) error {
	which := "clients"
	if len(args) > 0 {
		which = args[0]
	}
	file, label := "known_clients.json", "trusted controllers"
	if which == "servers" {
		file, label = "known_servers.json", "pinned devices"
	}
	store, err := transport.OpenStore(file)
	if err != nil {
		return err
	}
	peers := store.List()
	fmt.Printf("%s: %d\n", label, len(peers))
	for _, p := range peers {
		fmt.Printf("  %-20s %s  (added %s)\n", p.Name, transport.ShortFingerprint(p.Fingerprint), p.Added.Format("2006-01-02"))
	}
	return nil
}
