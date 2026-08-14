// Command wanctl is a cross-internet remote-control CLI. The same binary runs as
// the relay (`wanctl relay`, on thunderbox), the controlled device
// (`wanctl agent`), and the controller (`wanctl exec/push/pull`). Endpoints meet
// through the relay's WebSocket broker and speak end-to-end mutual TLS, so the
// relay only sees ciphertext.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wanctl/internal/agent"
	// Android has no /etc/resolv.conf, so a CGO_ENABLED=0 binary resolves
	// nothing until this package's init points the Go resolver somewhere real.
	// Imported for that side effect; it compiles to nothing elsewhere.
	_ "wanctl/internal/androiddns"
	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/eventlog"
	"wanctl/internal/limits"
	mcppkg "wanctl/internal/mcp"
	"wanctl/internal/policy"
	"wanctl/internal/portal"
	"wanctl/internal/relay"
	"wanctl/internal/script"
	"wanctl/internal/serverlog"
	"wanctl/internal/transport"
)

const usage = `wanctl — control a device across the internet over an encrypted, relayed channel

USAGE
 DEVICE LIFECYCLE (run on the box you want to control)
  wanctl                                      log in (Feishu) if needed, then run the agent detached in the background
  wanctl start                                (re)start the background agent without re-login; records its pid
  wanctl stop                                 stop the background agent
  wanctl status [-target NS/DEV]              show local agent/credential status, or remote agent mode + version
  wanctl logout                               stop the agent and forget the saved login
  wanctl service install [--name N] [--portal-fps FP[,FP]] [--mode M]
                                              install an OS-native always-on service (systemd/launchd/Scheduled Task);
                                              --name/--portal-fps are baked into the unit (a unit restarts unattended);
                                              omit --mode so the persisted mode and portal switches survive a restart
  wanctl service uninstall                     remove that service
  wanctl service status                        show whether the service is installed + active
  wanctl agent [flags]                         run the agent in the FOREGROUND (what 'wanctl'/'start'/the service spawn)
  Persistence: 'wanctl start' survives THIS terminal but may die on logout/reboot.
  'wanctl service install' adds OS-native autostart. Linux needs user lingering
  for boot-without-login; Windows starts the limited-user task at the next logon.

 CONTROLLER (run where you / the AI drive from)
  wanctl login [--code CODE]                  log in (Feishu) and save the token — no daemon (use this on AI / controller boxes);
                                              --code skips the prompt for a front-end that already has the enrollment code
  wanctl update                               download the latest binary from the relay and swap it in
  wanctl update --fetch-apk DIR               Android: download+verify the APK there and print its path (the app installs it)
  wanctl version                              print the immutable release version (or dev)
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
  wanctl exec  [--target NS/DEV] [--oneshot] [NS/DEV|DEV] <command...>
  wanctl exec  [--target NS/DEV] --script <local-file> [--interp powershell|sh] [NS/DEV|DEV]
                                              run a LOCAL script on the device — no shell quoting or encoding hazards
  wanctl exec  --target ANDROID-DEV -- battery report fresh battery state from an Android APK agent as JSON
  wanctl exec  --target ANDROID-DEV --elevate [--via su|shizuku|adb] <command...>
                                              run with elevated privilege on Android, which is what pm / am /
                                              input / screencap / dumpsys / settings need. Off by default on the
                                              device, and elevated commands need their own policy rule —
                                              bypass mode does not cover them.
  wanctl screenshot [DEVICE] [-o file.png] [--via su|shizuku|adb]
                                              capture an Android screen to a local PNG (implies --elevate;
                                              "-o -" writes the image to stdout instead)
  wanctl push  [--target NS/DEV] <local> <remote>
  wanctl pull  [--target NS/DEV] <remote> <local>
  wanctl peers
  wanctl net [wan|lan|auto|status]           switch which relay the controller uses: public (wan), intranet
                                              fast-path (lan, real-time WS), or probe-and-pick (auto)
  wanctl label ["<who you are>"]              show or set this controller's self-description; devices refuse to
                                              raise a pairing request from a controller without one
  wanctl id
  wanctl pair  <device>                       check device trust state; if not yet paired print the URL the device owner clicks to approve
  wanctl trust [clients|servers]
  wanctl trust server --target NS/DEV --fingerprint SHA256:... [--replace]
  wanctl logs [--target NS/DEV] [--type T] [--since RFC3339] [--grep STR] [--limit N]
                                              read the existing local/remote device activity log
  wanctl logs --service portal|relay [--since 15m] [--grep STR] [--limit N]
                                              read recent server logs; --follow is not yet supported
  wanctl portal-admins [list|add|remove]        manage local portal root fingerprints
  wanctl agent [--name N] [--relay URL] [--token T] [--yes] [--shell S] [--portal-fps FP[,FP]]
  wanctl relay  [--addr :8080]                run the relay (thunderbox); DATABASE_URL or WANCTL_TOKENS
  wanctl portal [--addr :8080]                run the team portal (thunderbox, internal SSO)

Defaults: relay=` + defaultRelay + `  transport=` + defaultTransport + ` (override with WANCTL_RELAY/WANCTL_TRANSPORT)
ENV (controller): WANCTL_TOKEN=... (or run 'wanctl' to log in)  WANCTL_RELAY=...
                  WANCTL_PORTAL=... WANCTL_ADMIN_SECRET=... (server logs only)
ENV (relay):      WANCTL_TOKENS="token:namespace,token2:ns2"  WANCTL_ADMIN_SECRET=...  WANCTL_PORTAL_NS=...
ENV (portal):     RELAY_ADMIN_URL=...  WANCTL_ADMIN_SECRET=...  PORTAL_USER_HEADER=...
              PORTAL_PUBLIC_ORIGIN=https://portal.example  PORTAL_DEBUG_WHOAMI=1 (diagnostics only)
              WANCTL_RELAY=...  WANCTL_PORTAL_TOKEN=...  WANCTL_TRANSPORT=ws
              WANCTL_LARK_APP_ID=...  WANCTL_LARK_APP_SECRET=... (optional; enables Feishu approvals)
ENV (agent):      WANCTL_PORTAL_FPS=SHA256:...[,SHA256:...]  (WANCTL_PORTAL_FP is a legacy alias)
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
	case "screenshot":
		err = cmdScreenshot(ctx, os.Args[2:])
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
	case "portal-admins":
		err = cmdPortalAdmins(os.Args[2:])
	case "rules":
		err = cmdRules(os.Args[2:])
	case "logs":
		err = cmdLogs(ctx, os.Args[2:])
	case "net":
		err = cmdNet(os.Args[2:])
	case "label":
		err = cmdLabel(os.Args[2:])
	case "up":
		err = cmdUp(ctx)
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "docs":
		err = cmdDocs(ctx, os.Args[2:])
	case "start":
		err = cmdStart()
	case "stop":
		err = cmdStop()
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "logout":
		err = cmdLogout()
	case "update":
		err = cmdUpdate(ctx, os.Args[2:])
	case "__restart-managed":
		err = cmdRestartManaged(os.Args[2:])
	case "__supervise":
		err = cmdSupervise(ctx, os.Args[2:])
	case "version":
		fmt.Println(buildVersion)
		return
	case "service":
		err = cmdService(ctx, os.Args[2:])
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
	logs := serverlog.NewDefault()
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	log.SetFlags(log.LstdFlags)

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
		log.Print("wanctl relay: token store = postgres (hashed tokens + ACL + audit)")
	} else {
		spec := os.Getenv("WANCTL_TOKENS")
		upstream := os.Getenv("WANCTL_UPSTREAM_RELAY")
		var stores relay.ChainTokenStore
		if spec != "" {
			stores = append(stores, relay.EnvTokenStore(spec))
		}
		if upstream != "" {
			sec := os.Getenv("WANCTL_ADMIN_SECRET")
			if sec == "" {
				return fmt.Errorf("WANCTL_UPSTREAM_RELAY needs WANCTL_ADMIN_SECRET (shared with the upstream relay)")
			}
			stores = append(stores, relay.NewUpstreamTokenStore(strings.TrimRight(upstream, "/"), sec))
		}
		if len(stores) == 0 {
			return fmt.Errorf("set DATABASE_URL (postgres), WANCTL_TOKENS=\"token:namespace,...\", or WANCTL_UPSTREAM_RELAY")
		}
		r = relay.New(stores)
		if upstream != "" {
			log.Printf("wanctl relay: token store = env + upstream (%s)", upstream)
		} else {
			log.Print("wanctl relay: token store = env (WANCTL_TOKENS)")
		}
	}
	// The admin secret gates /admin/* (portal access + satellite-relay token
	// resolution). Set it regardless of the token-store backend: a satellite
	// relay may itself be asked to resolve for another one, and the resolve
	// endpoint only needs the token store.
	if sec := os.Getenv("WANCTL_ADMIN_SECRET"); sec != "" {
		r.SetAdminSecret(sec)
		log.Print("wanctl relay: admin API enabled (secret-gated)")
	}
	r.SetLogBuffer(logs)
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
		log.Print("wanctl relay: MCP server enabled at /wanctl-mcp (Streamable HTTP)")
	}
	log.Printf("wanctl relay listening on %s", *addr)
	return limits.HTTPServer(*addr, r.Handler()).ListenAndServe()
}

func cmdPortal(args []string) error {
	fs := flag.NewFlagSet("portal", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	logs := serverlog.NewDefault()
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	log.SetFlags(log.LstdFlags)
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
		PublicOrigin:  os.Getenv("PORTAL_PUBLIC_ORIGIN"),
		DebugWhoami:   os.Getenv("PORTAL_DEBUG_WHOAMI") == "1",
	})
	p.SetLogBuffer(logs)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p.Start(ctx)
	defer p.Close()
	log.Printf("wanctl portal on %s\n  identity:      %s\n  identity hdr:  %q\n  relay(admin):  %q\n  relay(dial):   %q",
		*addr, id.Fingerprint, envOr("PORTAL_USER_HEADER", "X-Auth-Request-Email"),
		os.Getenv("RELAY_ADMIN_URL"), os.Getenv("WANCTL_RELAY"))
	server := limits.HTTPServer(*addr, p.Handler())
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
	managed := fs.Bool("managed", false, "agent is owned by an external supervisor")
	portalFPS := fs.String("portal-fps", config.PortalFingerprintsEnv(), "comma-separated portal admin fingerprints to seed locally")
	portalPK := fs.String("portal-pk", "", "deprecated alias for one --portal-fps entry")
	lanRelay := fs.String("lan-relay", config.LanRelay(), "intranet fast-path relay (ws://...); empty disables the second uplink")
	fs.Parse(args)
	portalRaw := *portalFPS
	if *portalPK != "" {
		if portalRaw != "" {
			portalRaw += ","
		}
		portalRaw += *portalPK
	}
	parsedPortalFPs, err := config.ParsePortalFingerprints(portalRaw)
	if err != nil {
		return fmt.Errorf("portal fingerprints: %w", err)
	}
	if *relayURL == "" || *token == "" {
		return fmt.Errorf("provide --relay and --token (or WANCTL_RELAY/WANCTL_TOKEN)")
	}
	ag, err := agent.New(agent.Options{RelayURL: *relayURL, Token: *token, Name: *name, Shell: *shell, AutoYes: *yes, Transport: *tr, Mode: policy.Mode(*mode), PortalFPs: parsedPortalFPs, LanRelay: *lanRelay, Version: buildVersion})
	if err != nil {
		return err
	}
	// Warn on the EFFECTIVE mode (which may be a persisted bypass, not just an
	// explicit --mode bypass flag).
	if ag.Mode() == policy.ModeBypass {
		fmt.Fprintln(os.Stderr, "wanctl: BYPASS mode — every command and file op is auto-allowed. Use only on trusted, isolated devices.")
	}
	lock, err := config.AcquireAgentLock()
	if err != nil {
		if config.IsAgentLockHeld(err) {
			fmt.Fprintf(os.Stderr, "wanctl: another agent is already running for this config dir (pid %d); exiting\n", config.ReadPID())
			return nil
		}
		return err
	}
	defer lock.Close()
	// Self-register the pid so `wanctl status`/`stop` see this agent no matter how
	// it was launched (bare `wanctl`, a keeper task, a systemd/launchd service),
	// not just the child that `wanctl start` spawns.
	pid := os.Getpid()
	_ = config.WritePID(pid)
	if *managed {
		_ = config.WriteManagedPID(pid)
	} else {
		_ = config.RemoveManagedPID(config.ManagedPID())
	}
	defer func() {
		_ = config.RemoveManagedPID(pid)
		_ = config.RemovePID()
	}()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ag.Run(ctx)
}

func cmdExec(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	target := fs.String("target", "", "device (NS/DEV or DEV)")
	oneShot := fs.Bool("oneshot", false, "fresh shell, no session state")
	cwd := fs.String("cwd", "", "working directory on the device (also the policy scope)")
	scriptPath := fs.String("script", "", "run a local script file on the device instead of a command string;\n"+
		"\tno shell quoting or encoding hazards — the file is sent base64-encoded.\n"+
		"\tInterpreter comes from the extension (.ps1 -> PowerShell, .sh/none -> sh)")
	interp := fs.String("interp", "", "override the -script interpreter: powershell | sh")
	elevateFlag := fs.Bool("elevate", false, "run with elevated privilege on the device (Android: root, Shizuku,\n"+
		"\tor the device's own adbd — whichever is available). Needs its own policy\n"+
		"\trule: bypass mode does not cover elevated commands.")
	via := fs.String("via", "", "pin the elevation channel: su | shizuku | adb.\n"+
		"\tFails if that channel is unavailable rather than falling back.")
	fs.Parse(args)
	if *via != "" && !*elevateFlag {
		// -via without -elevate would otherwise be silently ignored, and the
		// command would run unprivileged while looking like it asked not to.
		*elevateFlag = true
	}
	commandArgs := fs.Args()

	var c *client.Client
	var err error
	if *target == "" && len(commandArgs) > 0 {
		c, err = client.New()
		if err != nil {
			return err
		}
		aliases, err := c.PeerAliases(ctx)
		if err != nil {
			return err
		}
		*target, commandArgs = inferExecTarget(*target, commandArgs, aliases)
	}
	command := strings.TrimSpace(strings.Join(commandArgs, " "))

	if *scriptPath != "" {
		if command != "" {
			return fmt.Errorf("give either -script or a command, not both")
		}
		var err error
		if command, err = buildScriptCommand(*scriptPath, *interp); err != nil {
			return err
		}
	}
	if command == "" {
		return fmt.Errorf("no command given (pass a command string, or -script <file>)")
	}
	// The command is source for the device's shell, so it gets parsed there
	// before anything else runs. A nested `powershell -Command "...$x..."` is
	// therefore parsed twice and loses its variables to the outer pass — a
	// failure that surfaces as a bogus "term is not recognized" from the inner
	// script. Say so at the moment it would happen rather than in the docs.
	if *scriptPath == "" && script.NestedPowerShellExpansion(command) {
		fmt.Fprintln(os.Stderr, "wanctl: warning: this command nests `powershell -Command \"...\"` with a $ inside the double quotes.")
		fmt.Fprintln(os.Stderr, "        The device's shell expands those variables before the inner PowerShell sees them.")
		fmt.Fprintln(os.Stderr, "        Use single quotes for the inner script, or `wanctl exec -script <file.ps1>`.")
	}
	warnPOSIXShellQuoteLoss(os.Stderr, *scriptPath, commandArgs)

	if c == nil {
		c, err = client.New()
		if err != nil {
			return err
		}
	}
	code, err := c.Exec(ctx, client.ExecRequest{
		Target: *target, Command: command, OneShot: *oneShot, Cwd: *cwd,
		Elevate: *elevateFlag, Via: *via,
	})
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

// cmdScreenshot captures the device's screen to a local PNG.
//
// This is `exec --elevate -- screenshot` with the one piece a shell pipeline
// gets wrong: `screencap -p` writes a PNG to stdout, and stdout here is a
// terminal. Writing to a file by default — and only writing to the terminal
// when explicitly asked with `-o -` — is the difference between a usable
// command and a screenful of binary.
func cmdScreenshot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("screenshot", flag.ExitOnError)
	target := fs.String("target", "", "device (NS/DEV or DEV)")
	out := fs.String("o", "", "local file to write (default screenshot-<device>-<time>.png; \"-\" writes to stdout)")
	via := fs.String("via", "", "pin the elevation channel: su | shizuku | adb")
	fs.Parse(args)
	rest := fs.Args()
	// Go's flag package stops at the first non-flag argument, so
	// `screenshot emu -o out.png` would otherwise leave -o unparsed and reject
	// it as a stray argument. Take the device name and resume parsing.
	if *target == "" && len(rest) > 0 {
		*target = rest[0]
		fs.Parse(rest[1:])
		rest = fs.Args()
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q (usage: wanctl screenshot [DEVICE] [-o file.png])", rest[0])
	}

	c, err := client.New()
	if err != nil {
		return err
	}

	// Buffer rather than stream: a failed capture must not leave a truncated
	// PNG on disk that looks like a real one. The device's stderr and any
	// policy rejection travel on separate frames, so they still reach the user.
	var png bytes.Buffer
	code, err := c.ExecTo(ctx, client.ExecRequest{
		Target: *target, Command: "screenshot", OneShot: true, Elevate: true, Via: *via,
	}, &png, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("screencap failed on the device (exit %d)", code)
	}
	if png.Len() == 0 {
		return fmt.Errorf("device returned an empty screenshot")
	}
	// `screencap -p` emits a PNG; anything else means the verb did not run
	// (an older agent, say) and the bytes are some tool's error text.
	if !bytes.HasPrefix(png.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		return fmt.Errorf("device did not return a PNG (%d bytes, starting %q)",
			png.Len(), firstBytes(png.Bytes(), 40))
	}

	if *out == "-" {
		_, err := os.Stdout.Write(png.Bytes())
		return err
	}
	path := *out
	if path == "" {
		name := *target
		if name == "" {
			name = "device"
		}
		name = strings.NewReplacer("/", "-", string(os.PathSeparator), "-").Replace(name)
		path = fmt.Sprintf("screenshot-%s-%s.png", name, time.Now().Format("20060102-150405"))
	}
	if err := os.WriteFile(path, png.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s (%d bytes)\n", path, png.Len())
	return nil
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.ToValidUTF8(string(b), "?")
}

func warnPOSIXShellQuoteLoss(w io.Writer, scriptPath string, args []string) {
	if scriptPath != "" || !script.POSIXShellQuoteLoss(args) {
		return
	}
	fmt.Fprintln(w, "wanctl: warning: this looks like `sh -c '<script>'`, but your local shell already removed the quotes,")
	fmt.Fprintln(w, "        so the device receives separate words and may run only the first one.")
	fmt.Fprintln(w, "        Use `wanctl exec -script <file.sh>`; it is sent base64-encoded and cannot be re-split.")
}

// inferExecTarget supports the conventional `exec DEVICE COMMAND` spelling.
// An explicit -target is authoritative and disables positional inference, which
// is also the escape hatch for a command whose name matches an online device.
func inferExecTarget(target string, args, peerAliases []string) (string, []string) {
	if target != "" || len(args) == 0 {
		return target, args
	}
	for _, alias := range peerAliases {
		if args[0] == alias {
			return alias, args[1:]
		}
	}
	return target, args
}

// buildScriptCommand turns a local script file into a command string that
// carries the script without exposing it to shell parsing. See execscript.go for
// why this exists.
func buildScriptCommand(path, interpFlag string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("-script expects a local file path; cannot read %q: %w", path, err)
	}
	var in script.Interp
	if interpFlag != "" {
		in, err = script.ParseInterp(interpFlag)
	} else {
		in, err = script.ForPath(path)
	}
	if err != nil {
		return "", err
	}
	return script.Command(in, data)
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
	// push is byte-exact on purpose, so this warns rather than rewrites: a
	// BOM-less UTF-8 .ps1 is read by Windows PowerShell 5.1 as the ANSI code
	// page, which mangles non-ASCII text and can eat the closing quote of a
	// string literal — at which point PowerShell echoes the script instead of
	// running it.
	if data, err := os.ReadFile(fs.Arg(0)); err == nil && script.BomlessNonASCIIPowerShell(fs.Arg(1), data) {
		fmt.Fprintln(os.Stderr, "wanctl: warning: "+fs.Arg(0)+" is a .ps1 with non-ASCII text and no UTF-8 BOM.")
		fmt.Fprintln(os.Stderr, "        Windows PowerShell 5.1 will read it as the ANSI code page and mangle that text.")
		fmt.Fprintln(os.Stderr, "        Add a BOM before pushing, or run it with `wanctl exec -script` instead.")
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

// cmdLabel shows or sets this controller's self-description. A device asked to
// pair an unknown controller shows it to its owner, and refuses the request when
// it is missing: "trust SHA256:… from bogon?" is not a question anyone can
// answer, and unanswerable prompts get clicked through.
func cmdLabel(args []string) error {
	if len(args) == 0 {
		label := config.EnvOr("WANCTL_LABEL", config.StoredLabel())
		if label == "" {
			fmt.Println("控制端标签: (未设置)")
			fmt.Println("设备在配对时会拒绝没有标签的控制端。设置方法：")
			fmt.Println(`  wanctl label "张三的 MacBook / Claude Code"`)
			return nil
		}
		fmt.Printf("控制端标签: %s\n", label)
		if os.Getenv("WANCTL_LABEL") != "" {
			fmt.Println("(来自 WANCTL_LABEL 环境变量，覆盖已保存的值)")
		}
		return nil
	}
	label := strings.TrimSpace(strings.Join(args, " "))
	if err := config.SaveLabel(label); err != nil {
		return err
	}
	if label == "" {
		fmt.Println("✓ 已清除控制端标签")
		return nil
	}
	fmt.Printf("✓ 控制端标签: %s\n", label)
	return nil
}

func cmdNet(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "wan", "lan", "auto":
		if err := config.SaveNetMode(sub); err != nil {
			return err
		}
		fmt.Printf("network mode: %s\n", sub)
		if sub != "wan" {
			if client.LanReachable(800 * time.Millisecond) {
				fmt.Printf("intranet relay %s: reachable ✓\n", config.LanRelay())
			} else {
				fmt.Printf("intranet relay %s: NOT reachable — lan exec will fail%s\n",
					config.LanRelay(), map[bool]string{true: " (auto will fall back to wan)", false: ""}[sub == "auto"])
			}
		}
		return nil
	case "status":
		mode := config.StoredNetMode()
		fmt.Printf("network mode:   %s\n", mode)
		fmt.Printf("public relay:   %s\n", config.EnvOr("WANCTL_RELAY", config.DefaultRelay))
		reach := "not reachable"
		if client.LanReachable(800 * time.Millisecond) {
			reach = "reachable ✓"
		}
		fmt.Printf("intranet relay: %s (%s)\n", config.LanRelay(), reach)
		if os.Getenv("WANCTL_RELAY") != "" {
			fmt.Println("note: WANCTL_RELAY is set and overrides the network mode")
		}
		return nil
	default:
		return fmt.Errorf("usage: wanctl net [wan|lan|auto|status]")
	}
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
	service := fs.String("service", "", "server service: portal or relay (omit for device logs)")
	follow := fs.Bool("follow", false, "follow server logs (not yet supported)")
	fs.Parse(args)
	if *service != "" {
		if *service != "portal" && *service != "relay" {
			return fmt.Errorf("service must be portal or relay")
		}
		if *target != "" || *logType != "" || fs.NArg() != 0 {
			return fmt.Errorf("--service cannot be combined with --target, --type, or positional arguments")
		}
		if *follow {
			return fmt.Errorf("--follow is not yet supported for server logs")
		}
		duration := serverlog.DefaultSince
		if *since != "" {
			var err error
			duration, err = time.ParseDuration(*since)
			if err != nil || duration < 0 {
				return fmt.Errorf("--since must be a non-negative duration for server logs")
			}
		}
		n := *limit
		if n == 0 {
			n = serverlog.DefaultLimit
		}
		if n < 0 {
			return fmt.Errorf("--limit must be positive")
		}
		q := serverlog.Query{Service: *service, Since: duration, Limit: min(n, serverlog.MaxLimit), Grep: *grep}
		return fetchServerLogs(ctx, http.DefaultClient, config.EnvOr("WANCTL_PORTAL", defaultPortal), os.Getenv("WANCTL_ADMIN_SECRET"), q, os.Stdout)
	}
	if *follow {
		return fmt.Errorf("--follow requires --service and is not yet supported")
	}

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

func fetchServerLogs(ctx context.Context, hc *http.Client, adminURL, secret string, q serverlog.Query, out io.Writer) error {
	resp, err := serverlog.Fetch(ctx, hc, adminURL, secret, q)
	if err != nil {
		return err
	}
	return serverlog.Format(out, resp)
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
		kind := fs.String("kind", "exec", "exec | read | write | logs")
		pattern := fs.String("pattern", "", "exec: command (single-command arg prefix, trailing * ok); file: directory")
		dir := fs.String("dir", "", "for exec dir-scope: the working directory")
		fs.Parse(args)
		r := policy.Rule{Kind: policy.Kind(*kind), Pattern: *pattern, Scope: policy.ScopeGlobal}
		switch policy.Kind(*kind) {
		case policy.KindExec:
			if *pattern == "" {
				return fmt.Errorf("exec rules require --pattern")
			}
			if *dir != "" { // exec dir-scope: command pattern restricted to a working dir
				r.Scope = policy.ScopeDir
				r.Dir = *dir
			}
		case policy.KindRead, policy.KindWrite:
			if *pattern == "" {
				return fmt.Errorf("%s rules require --pattern", *kind)
			}
			if *pattern != "" { // a file pattern is itself a directory restriction
				r.Scope = policy.ScopeDir
			}
		case policy.KindLogs:
			if *pattern != "" || *dir != "" {
				return fmt.Errorf("logs rules do not take --pattern or --dir")
			}
			r.Pattern = "*"
		default:
			return fmt.Errorf("invalid --kind %q (want exec|read|write|logs)", *kind)
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
	if len(args) > 0 && args[0] == "server" {
		fs := flag.NewFlagSet("trust server", flag.ContinueOnError)
		target := fs.String("target", "", "canonical owner/device target")
		fingerprint := fs.String("fingerprint", "", "verified SHA256 device fingerprint")
		replace := fs.Bool("replace", false, "replace an existing pin after independent verification")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *target == "" || *fingerprint == "" {
			return fmt.Errorf("usage: wanctl trust server --target NS/DEV --fingerprint SHA256:... [--replace]")
		}
		c, err := client.New()
		if err != nil {
			return err
		}
		canonical, err := c.PinServer(context.Background(), *target, *fingerprint, *replace)
		if err != nil {
			return err
		}
		fmt.Printf("pinned device %q identity %s\n", canonical, *fingerprint)
		return nil
	}
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

func cmdPortalAdmins(args []string) error {
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		return err
	}
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		fingerprints := admins.List()
		fmt.Printf("portal admins: %d\n", len(fingerprints))
		for _, fp := range fingerprints {
			fmt.Println("  " + fp)
		}
		return nil
	case "add", "seed":
		fs := flag.NewFlagSet("portal-admins "+sub, flag.ContinueOnError)
		raw := fs.String("fingerprints", "", "comma-separated SHA256 fingerprints")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *raw == "" && fs.NArg() > 0 {
			*raw = strings.Join(fs.Args(), ",")
		}
		fingerprints, err := config.ParsePortalFingerprints(*raw)
		if err != nil || len(fingerprints) == 0 {
			if err == nil {
				err = fmt.Errorf("at least one fingerprint is required")
			}
			return err
		}
		if err := admins.Add(fingerprints...); err != nil {
			return err
		}
		for _, fp := range fingerprints {
			if err := known.Add(fp, "portal"); err != nil {
				return err
			}
		}
		fmt.Printf("portal admins seeded: %d\n", len(fingerprints))
		return nil
	case "remove", "rm":
		if len(args) != 1 {
			return fmt.Errorf("usage: wanctl portal-admins remove <SHA256:fingerprint>")
		}
		if err := admins.Remove(args[0]); err != nil {
			return err
		}
		if err := known.Remove(args[0]); err != nil {
			_ = admins.Add(args[0])
			return err
		}
		fmt.Println("portal admin removed")
		return nil
	default:
		return fmt.Errorf("usage: wanctl portal-admins [list|add|remove]")
	}
}
