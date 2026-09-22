// Command wanctl is a cross-internet remote-control CLI. The same binary runs as
// the relay (`wanctl relay`), the controlled device
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/catalog"
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
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/script"
	"wanctl/internal/serverlog"
	"wanctl/internal/transport"
	"wanctl/internal/webfetch"
)

// usage is the short index bare `wanctl` prints. It used to be sixty lines of
// every flag of every subcommand, which is the same as printing nothing: the
// reader who needed one command scrolled past it. The index names each command
// in one line and points at `wanctl help <command>`, which renders that
// command's full entry from internal/catalog — the same text the MCP server
// registers as its tool description.
var usage = catalog.Index(defaultRelay, defaultPortal)

// withHelp points a FlagSet's -h at its catalog entry instead of Go's raw flag
// dump. A subcommand whose FlagSet is named after it ("exec") resolves
// directly; a nested one ("docs ls") falls back to its parent's entry, which is
// where the subcommand's own syntax is written.
func withHelp(fs *flag.FlagSet) *flag.FlagSet {
	name := fs.Name()
	c, ok := catalog.Lookup(name)
	if !ok {
		if i := strings.Index(name, " "); i > 0 {
			c, ok = catalog.Lookup(name[:i])
		}
	}
	if !ok {
		return fs
	}
	fs.Usage = func() { fmt.Fprint(fs.Output(), catalog.Entry(c)) }
	return fs
}

// isHelpFlag reports whether an argument is a request for help rather than
// input. `help` itself is deliberately absent: it is a plausible thing to run
// on a device, and `wanctl exec help` must stay a command.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// cmdHelp renders the contract: the index, one command's entry, or the whole
// catalog as Markdown (which is what docs/contract.md is generated from).
// Either spelling resolves, so an AI that only knows the MCP tool name can run
// `wanctl help wanctl_read`.
func cmdHelp(args []string) error {
	if len(args) > 0 && (args[0] == "--markdown" || args[0] == "-markdown") {
		fmt.Print(catalog.Markdown())
		return nil
	}
	// The same text an MCP host is handed in its initialize response. Printing
	// it here is how anything that is not an MCP client — the discovery page, a
	// person deciding what this thing will do to their machine — reads the
	// instructions the agent is working from, without a second copy existing.
	if len(args) > 0 && (args[0] == "--instructions" || args[0] == "-instructions") {
		fmt.Print(catalog.Instructions())
		return nil
	}
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	name := strings.Join(args, " ")
	c, ok := catalog.Lookup(name)
	if !ok {
		// Exiting non-zero matters: `wanctl help typo` in a script should fail
		// rather than look like it documented something.
		fmt.Fprintf(os.Stderr, "wanctl: no such command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	fmt.Print(catalog.Entry(c))
	return nil
}

// Deployment defaults live in internal/config so they can be injected with
// -ldflags while environment variables still take precedence at runtime.
var (
	defaultRelay  = settingValue("relay")
	defaultPortal = settingValue("portal")
)

const defaultTransport = config.DefaultTransport

func configuredDisplay(value, empty string) string {
	if value == "" {
		return empty
	}
	return value
}

func main() {
	if len(os.Args) < 2 {
		// Bare `wanctl` explains itself and does nothing else. It used to
		// enroll and start an agent, so someone on a controller-only machine
		// who ran it to see what it does turned that machine into a controlled
		// device. `wanctl start` is the device command; `wanctl login` is the
		// controller one.
		fmt.Print(usage)
		fmt.Println(localStatusLine())
		return
	}
	ctx := context.Background()
	var err error
	// `wanctl exec -h` is a question about wanctl, not a use of it, so it is
	// answered before the relay gate below: a binary that does not yet know
	// which instance it talks to must still be able to explain itself. It also
	// reaches the commands that read a subcommand before any FlagSet exists,
	// where a -h would otherwise land as a bad argument.
	if len(os.Args) == 3 && isHelpFlag(os.Args[2]) {
		if _, ok := catalog.Lookup(os.Args[1]); ok {
			if err := cmdHelp(os.Args[1:2]); err != nil {
				fmt.Fprintln(os.Stderr, "wanctl: "+err.Error())
				os.Exit(1)
			}
			return
		}
	}
	if relayCommands[os.Args[1]] {
		// The first-run question happens before the command's own work, so a
		// binary that does not know where to connect asks instead of failing
		// deep inside a dial (GitHub issue #11).
		if gateErr := ensureRelayConfigured(""); gateErr != nil {
			fmt.Fprintln(os.Stderr, "wanctl: "+gateErr.Error())
			os.Exit(1)
		}
	}
	switch os.Args[1] {
	case "relay":
		err = cmdRelay(os.Args[2:])
	case "portal":
		err = cmdPortal(os.Args[2:])
	case "agent":
		err = cmdAgent(ctx, os.Args[2:])
	case "exec":
		err = cmdExec(ctx, os.Args[2:])
	case "workspace":
		err = cmdWorkspace(ctx, os.Args[2:])
	case "screenshot":
		err = cmdScreenshot(ctx, os.Args[2:])
	case "push":
		err = cmdPush(ctx, os.Args[2:])
	case "pull":
		err = cmdPull(ctx, os.Args[2:])
	case "read":
		err = cmdRead(ctx, os.Args[2:])
	case "edit":
		err = cmdEdit(ctx, os.Args[2:])
	case "write":
		err = cmdWrite(ctx, os.Args[2:])
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
	case "label":
		err = cmdLabel(os.Args[2:])
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "docs":
		err = cmdDocs(ctx, os.Args[2:])
	case "friends":
		err = cmdFriends(ctx, os.Args[2:])
	case "share":
		err = cmdShare(ctx, os.Args[2:])
	case "start":
		err = cmdStart(ctx)
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
	case "admin":
		err = cmdAdmin(os.Args[2:])
	case "service":
		err = cmdService(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "-h", "--help", "help":
		err = cmdHelp(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wanctl: "+err.Error())
		os.Exit(1)
	}
}

// relayCommands cannot do anything without a relay, and take no --relay of
// their own, so the first-run gate runs for them before dispatch. Deliberately
// absent: `update` (an official build bakes a release page and needs no
// relay), `logs` (reads the local device log when no target is given),
// `service` (has its own --relay, and status/uninstall need none), `status`
// (a diagnostic that reports the missing relay itself), and the servers
// `relay`/`portal`. `agent` runs the same gate itself, after parsing --relay.
var relayCommands = map[string]bool{
	"start": true, "login": true,
	"exec": true, "screenshot": true, "push": true, "pull": true,
	"read": true, "edit": true, "write": true, "workspace": true,
	"peers": true, "pair": true, "friends": true, "share": true,
	"docs": true, "admin": true,
}

// settingValue is config.Setting without the source, for flag defaults and
// display lines.
func settingValue(key string) string {
	v, _ := config.Setting(key)
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func cmdRelay(args []string) error {
	fs := withHelp(flag.NewFlagSet("relay", flag.ExitOnError))
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	logs := serverlog.NewDefault()
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	log.SetFlags(log.LstdFlags)

	adminSecret := os.Getenv("WANCTL_ADMIN_SECRET")
	if err := validateAdminSecret(adminSecret); err != nil {
		return err
	}
	var r *relay.Relay
	var pgStore *relay.PGStore
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pg, err := relay.OpenPG(dsn)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		r = relay.New(pg)
		pgStore = pg
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
			sec := adminSecret
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
	if adminSecret != "" {
		r.SetAdminSecret(adminSecret)
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
		opts := mcppkg.Options{Seed: seed, EndpointPath: "/mcp"}
		// OAuth needs three things the session path does not: somewhere durable
		// to keep clients and refresh tokens, a public origin to publish as the
		// issuer, and a portal to host the consent page. Without all three the
		// endpoint keeps working exactly as before, on Mcp-Session-Id.
		switch {
		case pgStore == nil:
			log.Print("wanctl relay: MCP OAuth off (needs DATABASE_URL for clients and refresh tokens)")
		case os.Getenv("WANCTL_PUBLIC_ORIGIN") == "":
			log.Print("wanctl relay: MCP OAuth off (needs WANCTL_PUBLIC_ORIGIN as the issuer)")
		case os.Getenv("WANCTL_PORTAL") == "":
			log.Print("wanctl relay: MCP OAuth off (needs WANCTL_PORTAL to host the consent page)")
		default:
			r.SetMCPOAuth(seed, pgStore)
			opts.OAuth = &mcppkg.OAuthConfig{
				ResourceMetadataURL: strings.TrimRight(os.Getenv("WANCTL_PUBLIC_ORIGIN"), "/") +
					"/.well-known/oauth-protected-resource",
				Live:   r.ResolveOAuthToken,
				Revoke: r.RevokeOAuthRelayToken,
			}
			log.Print("wanctl relay: MCP OAuth enabled (authorize on the portal, tokens at /oauth/token)")
		}
		h, err := mcppkg.HandlerWithOptions(opts)
		if err != nil {
			return fmt.Errorf("mcp handler: %w", err)
		}
		r.SetMCPHandler(h)
		// The MCP server keeps its pinned device identities in this process's
		// memory. Unbinding a device has to reach them, the same way it reaches
		// the portal's own store (ADR 0002).
		r.SetPinForgetter(mcppkg.ForgetPinnedDevice)
		log.Print("wanctl relay: MCP server enabled at /mcp (alias /wanctl-mcp, Streamable HTTP)")
	}
	if seedHex := os.Getenv("WANCTL_WEBFETCH_SEED"); seedHex != "" {
		if pgStore == nil {
			return fmt.Errorf("WebFetch requires DATABASE_URL for durable delegation and job records")
		}
		seed, err := hex.DecodeString(seedHex)
		if err != nil {
			return fmt.Errorf("WANCTL_WEBFETCH_SEED must be hex-encoded")
		}
		publicOrigin := os.Getenv("WANCTL_PUBLIC_ORIGIN")
		relayURL := os.Getenv("WANCTL_WEBFETCH_RELAY_URL")
		if relayURL == "" {
			relayURL = publicOrigin
		}
		h, err := webfetch.New(webfetch.Config{
			Store: pgStore, Jobs: pgStore, Seed: seed,
			PublicOrigin: publicOrigin, RelayURL: relayURL,
			PortalOrigin: os.Getenv("WANCTL_WEBFETCH_PORTAL_ORIGIN"),
		})
		if err != nil {
			return fmt.Errorf("webfetch: %w", err)
		}
		defer h.Close()
		r.SetWebFetchHandler(h)
		log.Print("wanctl relay: WebFetch enabled at /webfetch (owner-approved device delegation)")
	}
	log.Printf("wanctl relay listening on %s", *addr)
	return limits.HTTPServer(*addr, r.Handler()).ListenAndServe()
}

func validateAdminSecret(secret string) error {
	if secret != "" && len(secret) < 32 {
		return fmt.Errorf("WANCTL_ADMIN_SECRET must be at least 32 bytes when set (have %d)", len(secret))
	}
	return nil
}

func cmdPortal(args []string) error {
	fs := withHelp(flag.NewFlagSet("portal", flag.ExitOnError))
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
	ghClientID := os.Getenv("WANCTL_GITHUB_CLIENT_ID")
	sessionSecret := os.Getenv("WANCTL_SESSION_SECRET")
	if ghClientID != "" {
		// The two login modes must not coexist: with OAuth active the identity
		// header is ignored, and a deployment that sets both is confused about
		// which proxy it trusts.
		if os.Getenv("PORTAL_USER_HEADER") != "" {
			return fmt.Errorf("set either WANCTL_GITHUB_CLIENT_ID (OAuth login) or PORTAL_USER_HEADER (trusted proxy), not both")
		}
		if os.Getenv("WANCTL_GITHUB_CLIENT_SECRET") == "" {
			return fmt.Errorf("WANCTL_GITHUB_CLIENT_ID is set but WANCTL_GITHUB_CLIENT_SECRET is empty")
		}
		if len(sessionSecret) < 32 {
			return fmt.Errorf("WANCTL_SESSION_SECRET must be at least 32 bytes when OAuth login is enabled (have %d)", len(sessionSecret))
		}
	}
	githubTransport, err := portal.GitHubProxyTransport(os.Getenv("WANCTL_GITHUB_PROXY"))
	if err != nil {
		return err
	}
	p := portal.New(portal.Config{
		GitHubTransport: githubTransport,
		RelayAdminURL:   os.Getenv("RELAY_ADMIN_URL"),
		AdminSecret:     os.Getenv("WANCTL_ADMIN_SECRET"),
		UserHeader:      os.Getenv("PORTAL_USER_HEADER"),

		GitHubClientID:     ghClientID,
		GitHubClientSecret: os.Getenv("WANCTL_GITHUB_CLIENT_SECRET"),
		SessionSecret:      sessionSecret,
		GitHubAuthBase:     os.Getenv("WANCTL_GITHUB_AUTH_BASE"),
		GitHubAPIBase:      os.Getenv("WANCTL_GITHUB_API_BASE"),
		RelayDialURL:       config.EnvOr("WANCTL_RELAY", config.DefaultRelay),
		PortalToken:        os.Getenv("WANCTL_PORTAL_TOKEN"),
		Transport:          envOr("WANCTL_TRANSPORT", "http"),
		Identity:           id,
		Known:              known,
		PublicOrigin:       os.Getenv("PORTAL_PUBLIC_ORIGIN"),
		DebugWhoami:        os.Getenv("PORTAL_DEBUG_WHOAMI") == "1",
	})
	p.SetLogBuffer(logs)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p.Start(ctx)
	defer p.Close()
	loginMode := "header " + strconv.Quote(envOr("PORTAL_USER_HEADER", "X-Auth-Request-Email"))
	if ghClientID != "" {
		loginMode = "github oauth (client " + ghClientID + ")"
	}
	log.Printf("wanctl portal on %s\n  identity:      %s\n  login:         %s\n  relay(admin):  %q\n  relay(dial):   %q",
		*addr, id.Fingerprint, loginMode,
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
	fs := withHelp(flag.NewFlagSet("agent", flag.ExitOnError))
	name := fs.String("name", "", "display name (default hostname; does not change device ID)")
	relayURL := fs.String("relay", settingValue("relay"), "relay ws(s) URL")
	token := fs.String("token", envOr("WANCTL_TOKEN", config.StoredToken()), "access/registration token")
	shell := fs.String("shell", "", "shell (default powershell on Windows, /bin/sh elsewhere)")
	yes := fs.Bool("yes", false, "auto-trust new controllers (unattended)")
	tr := fs.String("transport", config.Transport(), "transport: ws or http (http is proxy-agnostic)")
	mode := fs.String("mode", "", "policy mode: normal (prompt on miss) or bypass (auto-allow, DANGEROUS). Empty = keep the last persisted mode (default normal).")
	managed := fs.Bool("managed", false, "agent is owned by an external supervisor")
	portalFPS := fs.String("portal-fps", config.PortalFingerprintsEnv(), "comma-separated portal admin fingerprints to seed locally")
	portalPK := fs.String("portal-pk", "", "deprecated alias for one --portal-fps entry")
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
	if *token == "" {
		return fmt.Errorf("provide --token (or WANCTL_TOKEN)")
	}
	if err := ensureRelayConfigured(*relayURL); err != nil {
		return err
	}
	if *relayURL == "" {
		if *relayURL, err = config.Relay(); err != nil {
			return err
		}
	}
	ag, err := agent.New(agent.Options{RelayURL: *relayURL, Token: *token, Name: *name, Shell: *shell, AutoYes: *yes, Transport: *tr, Mode: policy.Mode(*mode), PortalFPs: parsedPortalFPs, Version: buildVersion})
	if err != nil {
		return err
	}
	// Warn on the EFFECTIVE mode (which may be a persisted bypass, not just an
	// explicit --mode bypass flag).
	if ag.Mode() == policy.ModeBypass {
		fmt.Fprintln(os.Stderr, "wanctl: BYPASS mode — every command and file op is auto-allowed. Use only on trusted, isolated devices.")
	}
	lock, err := awaitAgentLock(config.AcquireAgentLock, agentLockAttempts, agentLockPoll, time.Sleep)
	if err != nil {
		if config.IsAgentLockHeld(err) {
			fmt.Fprintln(os.Stderr, "wanctl: "+lockHeldMessage(config.ReadPID(), os.Getpid()))
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
	// handedOver is set when a successor process — the one an auto-update
	// started — already owns these files. Removing them then would leave the
	// new agent invisible to `wanctl status` and `wanctl stop`.
	handedOver := false
	defer func() {
		if handedOver {
			return
		}
		_ = config.RemoveManagedPID(pid)
		_ = config.RemovePID()
	}()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Said before the relay is dialled, so a log that starts with a connection
	// failure still records which build produced it.
	fmt.Printf("wanctl agent %s 已启动\n", buildVersion)

	// The updater stops this agent by cancelling its context; Run returns, and
	// the restart below hands the device to the binary now on disk.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	updater := startAutoUpdate(runCtx, buildVersion, ag.Busy, stopRun)
	runErr := ag.Run(runCtx)
	if updater != nil && updater.updated() {
		over, err := restartAgentForUpdate(updater.binaryPath(), os.Args, lock)
		if err != nil {
			return err
		}
		handedOver = over
		return nil
	}
	return runErr
}

// agentLockAttempts/agentLockPoll bound the wait for a predecessor to let go of
// the config-dir lock. An agent started by `wanctl start` (or by the restart
// half of `wanctl update`) can arrive while the agent it replaces is still
// shutting down: the parent signalled it and did not wait. Exiting immediately
// then left the machine with no agent at all, after the parent had already told
// the user one was running. Five seconds covers an ordinary shutdown; beyond
// that something is wrong and saying so beats waiting.
const (
	agentLockAttempts = 50
	agentLockPoll     = 100 * time.Millisecond
)

// awaitAgentLock retries acquisition while the lock is merely held, and returns
// any other error at once. The clock is injected so the retry is testable.
func awaitAgentLock(acquire func() (*config.AgentLock, error), attempts int, poll time.Duration, sleep func(time.Duration)) (*config.AgentLock, error) {
	lock, err := acquire()
	for attempt := 0; attempt < attempts && err != nil && config.IsAgentLockHeld(err); attempt++ {
		sleep(poll)
		lock, err = acquire()
	}
	return lock, err
}

// lockHeldMessage explains a lock this agent could not take. The pid file is
// not proof of who holds it: `wanctl start` records the pid of the child it
// spawned before that child has locked anything, so an agent that lost this
// race read its *own* pid there and reported "another agent is already running
// (pid <itself>)" -- which sent the reader looking for a process that was the
// one printing the message.
func lockHeldMessage(recordedPID, self int) string {
	if recordedPID == self || recordedPID <= 0 {
		return "the previous agent has not released this config dir yet; exiting. Run `wanctl start` once it has stopped"
	}
	return fmt.Sprintf("another agent is already running for this config dir (pid %d); exiting", recordedPID)
}

func cmdExec(ctx context.Context, args []string) error {
	// Legacy exec forwards Ctrl-C to the device. Workspace exec only stops
	// waiting: its device-owned request can be polled or explicitly cancelled.
	// Both controller paths use the shell's conventional 128+SIGINT exit code.
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt)
	defer stopSignals()
	fs := withHelp(flag.NewFlagSet("exec", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	workspace := workspaceFlag(fs)
	requestID := fs.String("request-id", "", "workspace command ID; reuse unchanged after an uncertain result")
	async := fs.Bool("async", false, "workspace only: return JSON immediately; collect with workspace poll")
	oneShot := fs.Bool("oneshot", false, "fresh shell, no session state")
	cwd := fs.String("cwd", "", "working directory on the device (also the policy scope)")
	scriptPath := fs.String("script", "", "run a local script file on the device instead of a command string;\n"+
		"\tno shell quoting or encoding hazards — the file is sent base64-encoded.\n"+
		"\tInterpreter comes from the extension (.ps1 -> PowerShell, .sh/none -> sh)")
	interp := fs.String("interp", "", "override the -script interpreter: powershell | sh")
	elevateFlag := fs.Bool("elevate", false, "run with elevated privilege on the device (Android: root or the\n"+
		"\tdevice's own adbd — whichever is available). Its own policy class:\n"+
		"\tneeds an approval or an exec-elevated rule, unless the device is in\n"+
		"\tbypass mode AND has its elevation channel switched on.")
	via := fs.String("via", "", "pin the elevation channel: su | adb.\n"+
		"\tFails if that channel is unavailable rather than falling back.")
	fs.Parse(args)
	if *via != "" && !*elevateFlag {
		// -via without -elevate would otherwise be silently ignored, and the
		// command would run unprivileged while looking like it asked not to.
		*elevateFlag = true
	}
	commandArgs := fs.Args()
	ref, routeErr := workspaceRoute(*target, *workspace)
	if routeErr != nil {
		return routeErr
	}
	if ref.ID == "" && (*requestID != "" || *async) {
		return fmt.Errorf("--request-id and --async require a workspace")
	}

	var c *client.Client
	var err error
	if ref.ID == "" && *target == "" && len(commandArgs) > 0 {
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
		if ref.ID == "" {
			var err error
			if command, err = buildScriptCommand(*scriptPath, *interp); err != nil {
				return err
			}
		}
	}
	if command == "" && *scriptPath == "" {
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
	if ref.ID != "" {
		code, err := execWorkspace(ctx, c, ref, protocol.Message{
			Command: command, RequestID: *requestID, Cwd: *cwd,
			OneShot: *oneShot, Elevate: *elevateFlag, Via: *via,
		}, *scriptPath, *interp, *async, os.Stdout, os.Stderr)
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "wanctl: stopped waiting; the remote command may still be running. Use workspace poll with the request_id above, or workspace cancel to stop it")
			os.Exit(130)
		}
		if err != nil {
			return err
		}
		os.Exit(code)
	}
	code, err := c.Exec(ctx, client.ExecRequest{
		Target: *target, Command: command, OneShot: *oneShot, Cwd: *cwd,
		Elevate: *elevateFlag, Via: *via,
	})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "wanctl: interrupted — sent a cancel to the device")
			os.Exit(130) // 128 + SIGINT, what a shell reports for an interrupted command
		}
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
	fs := withHelp(flag.NewFlagSet("screenshot", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	out := fs.String("o", "", "local file to write (default screenshot-<device>-<time>.png; \"-\" writes to stdout)")
	via := fs.String("via", "", "pin the elevation channel: su | adb")
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
		Target: *target, Command: "screenshot", OneShot: true,
		// Asked for elevated because Android cannot capture without it and this
		// side cannot know what kind of device answers; a desktop gates it as
		// an ordinary command. ElevateOptional is what lets a laptop answer
		// without naming a channel it does not have.
		Elevate: true, ElevateOptional: true, Via: *via,
	}, &png, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the capture failed on the device (exit %d)", code)
	}
	if png.Len() == 0 {
		return fmt.Errorf("device returned an empty screenshot")
	}
	// A capture emits a PNG; anything else means the verb did not run and the
	// bytes are some tool's error text. On a device that is not Android, an
	// agent from before desktop capture is the usual reason.
	if !bytes.HasPrefix(png.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		return fmt.Errorf("device did not return a PNG (%d bytes, starting %q); if it is not Android, run `wanctl update` on it",
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
	fs := withHelp(flag.NewFlagSet("push", flag.ExitOnError))
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
	fs := withHelp(flag.NewFlagSet("pull", flag.ExitOnError))
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

// cmdRead prints a line range of a remote text file. The content goes to
// stdout so it can be piped or redirected byte for byte; everything about the
// read — which lines these are, how many there are in total, the file's hash —
// goes to stderr, where it does not contaminate that.
func cmdRead(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("read", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	workspace := workspaceFlag(fs)
	offset := fs.Int("offset", 0, "1-based line number to start at (default 1)")
	limit := fs.Int("limit", 0, "maximum number of lines to return (default 2000)")
	rest := parseAroundPositionals(fs, args)
	if len(rest) != 1 {
		return fmt.Errorf("usage: wanctl read [--target NS/DEV] <path> [--offset N] [--limit N]")
	}
	path := rest[0]
	ref, err := workspaceRoute(*target, *workspace)
	if err != nil {
		return err
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	res, err := c.ReadFile(ctx, client.ReadRequest{
		Target: ref.Target, WorkspaceID: ref.ID, Path: path, Offset: *offset, Limit: *limit,
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(os.Stdout, res.Content); err != nil {
		return err
	}
	truncated := "no"
	if res.Truncated {
		truncated = "yes"
	}
	fmt.Fprintf(os.Stderr, "lines %d-%d of %d, sha256 %s, truncated=%s\n",
		res.FirstLine, res.LastLine, res.TotalLines, res.SHA256, truncated)
	switch {
	case res.LongLine != 0:
		// Asking again from the next line would return this same line forever.
		fmt.Fprintf(os.Stderr, "line %d is larger than %d KiB; only its first part is shown — use exec with sed/cut to inspect it\n",
			res.LongLine, protocol.MaxReadBytes>>10)
	case res.Truncated:
		fmt.Fprintf(os.Stderr, "continue with --offset %d\n", res.LastLine+1)
	}
	return nil
}

// cmdEdit replaces a string inside a remote file. --old/--new take the text
// directly; --old-file/--new-file read it from a local file, which is how you
// pass a multi-line block without fighting the shell over quoting.
func cmdEdit(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("edit", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	workspace := workspaceFlag(fs)
	old := fs.String("old", "", "the exact text to find; must match once unless -all")
	oldFile := fs.String("old-file", "", "read the text to find from this local file instead of -old")
	newText := fs.String("new", "", "the text to put in its place (empty deletes)")
	newFile := fs.String("new-file", "", "read the replacement from this local file instead of -new")
	all := fs.Bool("all", false, "replace every occurrence instead of refusing when there is more than one")
	sha := fs.String("sha", "", "refuse the edit unless the file still has this sha256 (from wanctl read)")
	rest := parseAroundPositionals(fs, args)
	if len(rest) != 1 {
		return fmt.Errorf("usage: wanctl edit [--target NS/DEV] <path> (--old STR | --old-file F) (--new STR | --new-file F) [--all] [--sha SHA256]")
	}
	oldText, err := editText("old", *old, *oldFile)
	if err != nil {
		return err
	}
	replacement, err := editText("new", *newText, *newFile)
	if err != nil {
		return err
	}
	ref, err := workspaceRoute(*target, *workspace)
	if err != nil {
		return err
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	res, err := c.EditFile(ctx, client.EditRequest{
		Target: ref.Target, WorkspaceID: ref.ID, Path: rest[0],
		Old: oldText, New: replacement, All: *all, ExpectedSHA: *sha,
	})
	if err != nil {
		return err
	}
	fmt.Printf("replaced %d occurrence(s), sha256 %s\n", res.Replaced, res.SHA256)
	return nil
}

// cmdWrite creates a remote file or replaces one end to end. --content takes
// the text directly; --content-file reads it from a local file, which is how a
// whole config or script gets through without the shell touching it.
func cmdWrite(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("write", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	workspace := workspaceFlag(fs)
	content := fs.String("content", "", "the whole new text of the file")
	contentFile := fs.String("content-file", "", "read the content from this local file instead of -content")
	rest := parseAroundPositionals(fs, args)
	if len(rest) != 1 {
		return fmt.Errorf("usage: wanctl write [--target NS/DEV] <path> (--content STR | --content-file F)")
	}
	text, err := editText("content", *content, *contentFile)
	if err != nil {
		return err
	}
	ref, err := workspaceRoute(*target, *workspace)
	if err != nil {
		return err
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	res, err := c.WriteFile(ctx, client.WriteRequest{Target: ref.Target, WorkspaceID: ref.ID, Path: rest[0], Content: text})
	if err != nil {
		return err
	}
	verb := "overwrote"
	if res.Created {
		verb = "created"
	}
	fmt.Printf("%s %s (%d bytes, sha256 %s)\n", verb, rest[0], res.SizeBytes, res.SHA256)
	return nil
}

// parseAroundPositionals parses flags that may sit on either side of the
// positional arguments, which Go's flag package stops at. `wanctl read /path
// --limit 20` is the order a person types, and reading it as three positionals
// and no limit would be a silent wrong answer. Each round consumes the flags in
// front, takes the one positional it stops on, and resumes after it, so a flag's
// own value is never mistaken for a positional.
func parseAroundPositionals(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// editText resolves one of the -X / -X-file pairs. Giving both is an error
// rather than a precedence rule: the two disagree about what to write, and
// picking one silently is how the wrong text ends up in the file.
func editText(name, inline, path string) (string, error) {
	if inline != "" && path != "" {
		return "", fmt.Errorf("give either -%s or -%s-file, not both", name, name)
	}
	if path == "" {
		return inline, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func cmdPair(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("pair", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV); positional <device> also accepted")
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
	view, err := c.PeersAndShared(ctx)
	if err != nil {
		return err
	}
	devs, aliases, shared := view.Devices, view.Aliases, view.Shared
	if len(devs) == 0 && len(shared) == 0 {
		fmt.Println("no devices online for this token")
		return nil
	}
	if len(devs) == 0 {
		fmt.Println("no devices of your own are online")
	}
	for _, d := range devs {
		if alias := aliases[d]; alias != "" {
			fmt.Printf("%s  (%s)\n", d, alias)
		} else {
			fmt.Println(d)
		}
	}
	fmt.Print(sharedPeerLines(shared))
	return nil
}

// sharedPeerLines lists devices other people shared with this token. They are
// always printed in their owner/device form, because that is the only spelling
// --target accepts from anywhere, and a grantee otherwise has no way to learn
// the owner namespace at all.
func sharedPeerLines(shared []client.SharedDevice) string {
	if len(shared) == 0 {
		return ""
	}
	out := "\nshared with you (pass the whole owner/device to --target):\n"
	for _, s := range shared {
		line := s.Target
		if s.Label != "" && s.Label != s.Device {
			line += "  (" + s.Label + ")"
		}
		if !s.Online {
			line += "  [offline]"
		}
		out += line + "\n"
	}
	return out
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

func cmdID() error {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return err
	}
	dir, _ := transport.ConfigDir()
	deviceID, err := transport.LoadOrCreateDeviceID()
	if err != nil {
		return err
	}
	fmt.Printf("device ID:  %s\nfingerprint: %s\nconfig dir:  %s\n", deviceID, id.Fingerprint, dir)
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("logs", flag.ExitOnError))
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
		portalURL, err := config.Portal()
		if err != nil {
			return err
		}
		return fetchServerLogs(ctx, http.DefaultClient, portalURL, os.Getenv("WANCTL_ADMIN_SECRET"), q, os.Stdout)
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
				if r.Kind != policy.KindExec && r.Kind != policy.KindExecElevated {
					scope = "dir:" + r.Pattern
				}
			}
			fmt.Printf("  [%d] %-5s %-30q %s\n", i, r.Kind, r.Pattern, scope)
		}
		return nil
	case "add":
		fs := withHelp(flag.NewFlagSet("rules add", flag.ExitOnError))
		kind := fs.String("kind", "exec", "exec | exec-elevated | read | write | logs")
		pattern := fs.String("pattern", "", "exec: command (single-command arg prefix, trailing * ok); file: directory")
		dir := fs.String("dir", "", "for exec dir-scope: the working directory")
		fs.Parse(args)
		r := policy.Rule{Kind: policy.Kind(*kind), Pattern: *pattern, Scope: policy.ScopeGlobal}
		switch policy.Kind(*kind) {
		case policy.KindExec, policy.KindExecElevated:
			// exec-elevated is a kind the engine has always understood and the
			// CLI never offered, which left an elevated command with no way to
			// be pre-authorized anywhere (#63). For a `-script` payload the
			// pattern is the script token `wanctl` prints, not the base64.
			if *pattern == "" {
				return fmt.Errorf("%s rules require --pattern", *kind)
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
			return fmt.Errorf("invalid --kind %q (want exec|exec-elevated|read|write|logs)", *kind)
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
		fs := withHelp(flag.NewFlagSet("trust server", flag.ContinueOnError))
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
		fs := withHelp(flag.NewFlagSet("portal-admins "+sub, flag.ContinueOnError))
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
