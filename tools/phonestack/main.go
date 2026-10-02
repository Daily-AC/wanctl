// Command phonestack runs a disposable wanctl stack for testing the Android
// app on an emulator: relay and portal on the host (the emulator reaches them
// at 10.0.2.2), one target agent in normal mode, and a controller token. The
// portal signs everyone in as one test user, so nothing here may face a
// network. Data lives in its own schema of a throwaway PostgreSQL.
//
//	docker run -d --name phonestack-pg -e POSTGRES_PASSWORD=pg -p 127.0.0.1:55433:5432 postgres:16-alpine
//	WANCTL_TEST_POSTGRES='postgres://postgres:pg@127.0.0.1:55433/postgres?sslmode=disable' go run ./tools/phonestack <state dir>
//
// It writes <state dir>/info.json (relay, portal, target, controller token);
// home_test.py drives the app against it.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"wanctl/internal/agent"
	"wanctl/internal/config"
	"wanctl/internal/limits"
	"wanctl/internal/policy"
	"wanctl/internal/portal"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

const (
	user       = "phonestack@example.invalid"
	relayAddr  = "127.0.0.1:18995"
	portalAddr = "127.0.0.1:18996"
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
	if len(os.Args) != 2 {
		log.Fatal("usage: phonestack <state dir>")
	}
	state := os.Args[1]
	if err := os.MkdirAll(state, 0o700); err != nil {
		log.Fatal(err)
	}
	dsn := os.Getenv("WANCTL_TEST_POSTGRES")
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = base.Exec("CREATE SCHEMA IF NOT EXISTS phonestack"); err != nil {
		log.Fatal(err)
	}
	base.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		log.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", "phonestack")
	u.RawQuery = q.Encode()
	pg, err := relay.OpenPG(u.String())
	if err != nil {
		log.Fatal(err)
	}
	owner, err := pg.ResolveUser(user)
	if err != nil {
		log.Fatal(err)
	}
	deviceToken, _ := pg.IssueToken(owner, "phonestack-target", 1)
	ctlToken, _ := pg.IssueToken(owner, "phonestack-controller", 1)
	portalToken, _ := pg.IssueToken("portal", "phonestack-portal", 1)
	adminSecret := hex.EncodeToString(secret(filepath.Join(state, "admin.key")))
	portalID, err := transport.IdentityFromSeed(secret(filepath.Join(state, "portal.key")), "phonestack-portal")
	if err != nil {
		log.Fatal(err)
	}
	relayURL, portalURL := "http://"+relayAddr, "http://"+portalAddr
	r := relay.New(pg)
	r.SetAdmin(pg)
	r.SetACL(pg)
	r.SetAuditor(pg)
	r.SetDocs(pg)
	r.SetAdminSecret(adminSecret)
	r.SetPortalNS("portal")

	targetDir := filepath.Join(state, "target")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		log.Fatal(err)
	}
	os.Setenv("WANCTL_CONFIG_DIR", targetDir)
	if err := config.SaveSetting("portal", portalURL); err != nil {
		log.Fatal(err)
	}
	identity, err := transport.LoadOrCreateIdentity()
	if err != nil {
		log.Fatal(err)
	}
	ag, err := agent.New(agent.Options{RelayURL: relayURL, Token: deviceToken, Name: "phonestack-target", Mode: policy.ModeNormal, Transport: "http", PortalFP: portalID.Fingerprint, Version: "phonestack"})
	if err != nil {
		log.Fatal(err)
	}
	known := transport.NewMemStore()
	known.Pin(owner+"/"+ag.DeviceID(), identity.Fingerprint, false)
	p := portal.New(portal.Config{RelayAdminURL: relayURL, AdminSecret: adminSecret, UserHeader: "X-Phonestack-User", RelayDialURL: relayURL, PortalToken: portalToken, Transport: "http", Identity: portalID, Known: known, PublicOrigin: "http://10.0.2.2:18996"})
	ph := p.Handler()
	signedIn := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set("X-Phonestack-User", user)
		ph.ServeHTTP(w, req)
	})
	// Loopback only: the emulator's 10.0.2.2 is the host's loopback.
	go func() { log.Fatal(limits.HTTPServer(relayAddr, r.Handler()).ListenAndServe()) }()
	go func() { log.Fatal(limits.HTTPServer(portalAddr, signedIn).ListenAndServe()) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go ag.Run(ctx)
	p.Start(ctx)
	info := map[string]any{"relay": relayURL, "portal": portalURL, "owner": owner, "target": ag.DeviceID(), "controller_token": ctlToken}
	data, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(state, "info.json"), append(data, '\n'), 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	<-ctx.Done()
}
