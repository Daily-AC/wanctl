// webfetch-demo is an isolated browser acceptance fixture, never a production
// authentication configuration. The owner portal is loopback-only and supplies
// a fixed test identity; expose only /webfetch on the relay to the web chat.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/config"
	"wanctl/internal/limits"
	"wanctl/internal/policy"
	"wanctl/internal/portal"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
	"wanctl/internal/webfetch"
)

func secret(path string) []byte {
	if data, err := os.ReadFile(path); err == nil {
		return data
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Fatal(err)
	}
	return data
}

func main() {
	state := flag.String("state-dir", "", "private directory outside the source tree")
	public := flag.String("public-origin", "", "HTTPS origin exposing only relay /webfetch")
	relayAddr := flag.String("relay-addr", "127.0.0.1:18995", "loopback relay")
	portalAddr := flag.String("portal-addr", "127.0.0.1:18996", "loopback owner portal")
	flag.Parse()
	if *state == "" || *public == "" || !strings.HasPrefix(*relayAddr, "127.0.0.1:") || !strings.HasPrefix(*portalAddr, "127.0.0.1:") {
		log.Fatal("state-dir, public-origin and loopback listen addresses required")
	}
	if err := os.MkdirAll(*state, 0o700); err != nil {
		log.Fatal(err)
	}
	dsn := os.Getenv("WANCTL_TEST_POSTGRES")
	if dsn == "" {
		log.Fatal("WANCTL_TEST_POSTGRES must point to disposable PostgreSQL")
	}
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = base.Exec("CREATE SCHEMA IF NOT EXISTS webfetch_browser_demo"); err != nil {
		log.Fatal(err)
	}
	base.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		log.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", "webfetch_browser_demo")
	u.RawQuery = query.Encode()
	pg, err := relay.OpenPG(u.String())
	if err != nil {
		log.Fatal(err)
	}
	defer pg.Close()
	owner, err := pg.ResolveUser("webfetch-demo@example.invalid")
	if err != nil {
		log.Fatal(err)
	}
	deviceToken, err := pg.IssueToken(owner, "isolated-test-agent", 1)
	if err != nil {
		log.Fatal(err)
	}
	portalToken, err := pg.IssueToken("portal", "isolated-test-portal", 1)
	if err != nil {
		log.Fatal(err)
	}
	adminSecret := hex.EncodeToString(secret(filepath.Join(*state, "admin.key")))
	portalID, err := transport.IdentityFromSeed(secret(filepath.Join(*state, "portal.key")), "webfetch-test-portal")
	if err != nil {
		log.Fatal(err)
	}
	localRelay := "http://" + *relayAddr
	localPortal := "http://" + *portalAddr
	r := relay.New(pg)
	r.SetAdmin(pg)
	r.SetACL(pg)
	r.SetAuditor(pg)
	r.SetDocs(pg)
	r.SetAdminSecret(adminSecret)
	r.SetPortalNS("portal")
	h, err := webfetch.New(webfetch.Config{Store: pg, Jobs: pg, Seed: secret(filepath.Join(*state, "webfetch.key")), RelayURL: localRelay, PublicOrigin: *public, PortalOrigin: localPortal})
	if err != nil {
		log.Fatal(err)
	}
	defer h.Close()
	r.SetWebFetchHandler(h)
	deviceDir := filepath.Join(*state, "device")
	if err = os.MkdirAll(deviceDir, 0o700); err != nil {
		log.Fatal(err)
	}
	os.Setenv("WANCTL_CONFIG_DIR", deviceDir)
	if err = config.SaveSetting("portal", localPortal); err != nil {
		log.Fatal(err)
	}
	identity, err := transport.LoadOrCreateIdentity()
	if err != nil {
		log.Fatal(err)
	}
	sandbox := filepath.Join(*state, "sandbox")
	if err = os.MkdirAll(sandbox, 0o700); err != nil {
		log.Fatal(err)
	}
	rules, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		log.Fatal(err)
	}
	// This fixture uses the real device policy with only one test directory
	// and one harmless command. It never grants bypass mode.
	if len(rules.List()) == 0 {
		for _, kind := range []policy.Kind{policy.KindRead, policy.KindWrite} {
			if err = rules.Add(policy.Rule{Kind: kind, Pattern: sandbox, Scope: policy.ScopeDir}); err != nil {
				log.Fatal(err)
			}
		}
		if err = rules.Add(policy.Rule{Kind: policy.KindExec, Pattern: "printf wanctl-webfetch-ok", Scope: policy.ScopeGlobal}); err != nil {
			log.Fatal(err)
		}
	}
	ag, err := agent.New(agent.Options{RelayURL: localRelay, Token: deviceToken, Name: "WebFetch isolated test device", Mode: policy.ModeNormal, Transport: "http", PortalFP: portalID.Fingerprint, Version: "webfetch-dev"})
	if err != nil {
		log.Fatal(err)
	}
	known := transport.NewMemStore()
	if err = known.Pin(owner+"/"+ag.DeviceID(), identity.Fingerprint, false); err != nil {
		log.Fatal(err)
	}
	p := portal.New(portal.Config{RelayAdminURL: localRelay, AdminSecret: adminSecret, UserHeader: "X-Demo-User", RelayDialURL: localRelay, PortalToken: portalToken, Transport: "http", Identity: portalID, Known: known, PublicOrigin: localPortal})
	ph := p.Handler()
	portalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != *portalAddr {
			http.Error(w, "loopback fixture only", 403)
			return
		}
		r.Header.Del("X-Demo-User")
		r.Header.Set("X-Demo-User", "webfetch-demo@example.invalid")
		ph.ServeHTTP(w, r)
	})
	relayServer := limits.HTTPServer(*relayAddr, r.Handler())
	portalServer := limits.HTTPServer(*portalAddr, portalHandler)
	go func() {
		if err := relayServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go func() {
		if err := portalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go ag.Run(ctx)
	info := map[string]any{"relay": localRelay, "portal": localPortal, "public_webfetch": *public + "/webfetch", "owner": owner, "device": ag.DeviceID(), "device_fingerprint": identity.Fingerprint, "portal_fingerprint": portalID.Fingerprint, "sandbox": sandbox, "policy": "normal; test-directory read/write and printf wanctl-webfetch-ok only"}
	data, _ := json.MarshalIndent(info, "", "  ")
	if err = os.WriteFile(filepath.Join(*state, "info.json"), append(data, '\n'), 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	<-ctx.Done()
	ag.Close()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	relayServer.Shutdown(shutdown)
	portalServer.Shutdown(shutdown)
}
