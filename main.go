// Command wanctl is a cross-internet remote-control CLI. The same binary runs as
// the relay (`wanctl relay`, on thunderbox), the controlled device
// (`wanctl agent`), and the controller (`wanctl exec/push/pull`). Endpoints meet
// through the relay's WebSocket broker and speak end-to-end mutual TLS, so the
// relay only sees ciphertext.
package main

import (
	"context"
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
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/portal"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

const usage = `wanctl — control a device across the internet over an encrypted, relayed channel

USAGE
  wanctl relay  [--addr :8080]                run the relay (thunderbox); DATABASE_URL or WANCTL_TOKENS
  wanctl portal [--addr :8080]                run the team portal (thunderbox, internal SSO); DATABASE_URL
  wanctl agent [--name N] [--relay URL] [--token T] [--yes] [--shell S]
  wanctl exec  [--target NS/DEV] [--oneshot] <command...>
  wanctl push  [--target NS/DEV] <local> <remote>
  wanctl pull  [--target NS/DEV] <remote> <local>
  wanctl peers
  wanctl id
  wanctl trust [clients|servers]

ENV (controller): WANCTL_RELAY=wss://wanctl-relay.***REMOVED***.***REMOVED***.com  WANCTL_TOKEN=...
ENV (relay):      WANCTL_TOKENS="token:namespace,token2:ns2"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
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
	case "trust":
		err = cmdTrust(os.Args[2:])
	case "rules":
		err = cmdRules(os.Args[2:])
	case "logs":
		err = cmdLogs(ctx, os.Args[2:])
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
		fmt.Println("wanctl relay: token store = postgres (hashed tokens + ACL + audit)")
	} else {
		spec := os.Getenv("WANCTL_TOKENS")
		if spec == "" {
			return fmt.Errorf("set DATABASE_URL (postgres) or WANCTL_TOKENS=\"token:namespace,...\"")
		}
		r = relay.New(relay.EnvTokenStore(spec))
		fmt.Println("wanctl relay: token store = env (WANCTL_TOKENS)")
	}
	fmt.Printf("wanctl relay listening on %s\n", *addr)
	return http.ListenAndServe(*addr, r.Handler())
}

func cmdPortal(args []string) error {
	fs := flag.NewFlagSet("portal", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("portal requires DATABASE_URL (shared relay Postgres)")
	}
	p, err := portal.New(dsn, os.Getenv("PORTAL_USER_HEADER"))
	if err != nil {
		return err
	}
	fmt.Printf("wanctl portal listening on %s (identity header: %q)\n", *addr, envOr("PORTAL_USER_HEADER", "X-Forwarded-User"))
	return http.ListenAndServe(*addr, p.Handler())
}

func cmdAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	name := fs.String("name", "", "device name (default hostname)")
	relayURL := fs.String("relay", os.Getenv("WANCTL_RELAY"), "relay ws(s) URL")
	token := fs.String("token", os.Getenv("WANCTL_TOKEN"), "access/registration token")
	shell := fs.String("shell", "", "shell (default powershell on Windows, /bin/sh elsewhere)")
	yes := fs.Bool("yes", false, "auto-trust new controllers (unattended)")
	tr := fs.String("transport", envOr("WANCTL_TRANSPORT", "ws"), "transport: ws or http (http is proxy-agnostic)")
	mode := fs.String("mode", "normal", "policy mode: normal (prompt on miss) or bypass (auto-allow, DANGEROUS)")
	guiPort := fs.Int("gui-port", 0, "enable local web GUI (approvals/monitor) on 127.0.0.1:PORT")
	fs.Parse(args)
	if *relayURL == "" || *token == "" {
		return fmt.Errorf("provide --relay and --token (or WANCTL_RELAY/WANCTL_TOKEN)")
	}
	if *mode == "bypass" {
		fmt.Fprintln(os.Stderr, "wanctl: ⚠ BYPASS mode — every command and file op is auto-allowed without prompting. Use only on trusted, isolated devices.")
	}
	ag, err := agent.New(agent.Options{RelayURL: *relayURL, Token: *token, Name: *name, Shell: *shell, AutoYes: *yes, Transport: *tr, Mode: policy.Mode(*mode), GUIPort: *guiPort})
	if err != nil {
		return err
	}
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
