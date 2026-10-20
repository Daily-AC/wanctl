package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

func TestDeviceVersionsPostgresMigrationAndRegistration(t *testing.T) {
	db := deviceIDTestDB(t)
	old := fstest.MapFS{}
	paths, _ := fs.Glob(migrationFiles, "migrations/*.sql")
	for _, path := range paths {
		if path == "migrations/015_device_versions.sql" {
			continue
		}
		b, err := fs.ReadFile(migrationFiles, path)
		if err != nil {
			t.Fatal(err)
		}
		old[path] = &fstest.MapFile{Data: b}
	}
	if err := runMigrations(db, old); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}
	id := "12345678-1234-4234-8234-123456789abc"
	fp := transport.Fingerprint([]byte("device versions"))
	if _, err := p.RegisterDevice("alice", id, "desktop", fp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO acl(owner_namespace,device,grantee_namespace,perms,manage) VALUES ('alice',$1,'bob','exec',false)`, id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runMigrations(db, migrationFiles); err != nil {
			t.Fatal(err)
		}
	}
	check := func(wanctl, opencu string, reported bool) {
		t.Helper()
		for _, ns := range []string{"alice", "bob"} {
			rows, err := p.ListDevices(ns)
			if err != nil || len(rows) != 1 {
				t.Fatalf("ListDevices(%s): %v %v", ns, rows, err)
			}
			row := rows[0]
			if row["wanctl_version"] != wanctl || row["opencu_version"] != opencu || (row["versions_reported_at"] != nil) != reported {
				t.Fatalf("ListDevices(%s): %v", ns, row)
			}
			if reported {
				if stamp, ok := row["versions_reported_at"].(time.Time); !ok || time.Since(stamp) > time.Minute {
					t.Fatalf("report time: %v", row)
				}
			}
		}
	}
	if rows, err := p.ListDevices("charlie"); err != nil || len(rows) != 0 {
		t.Fatalf("unshared namespace: %v %v", rows, err)
	}
	check("", "", false)
	t.Setenv("WANCTL_DIST_DIR", signedDist(t))
	r := New(EnvTokenStore("tok:alice"))
	r.SetAdmin(p)
	r.SetAdminSecret("admin")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	if r.latestStable != "v1.0.0" {
		t.Fatalf("signed stable = %q", r.latestStable)
	}
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			for _, tc := range []struct {
				wanctl, opencu         any
				wantWanctl, wantOpenCU string
				reported               bool
			}{
				{"v0.9.0", "0.3.99", "v0.9.0", "0.3.99", true},
				{"v1.1.0-beta.2", "", "v1.1.0-beta.2", "", true},
				{strings.Repeat("x", 129), "<bad>", "", "", true},
				{"v1.2.0", nil, "v1.2.0", "", true},
				{nil, "0.3.99", "", "0.3.99", true},
				{"", "", "", "", true},
				{nil, nil, "", "", false},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				reg := map[string]any{"op": "register", "device": id, "device_id": id, "name": "desktop", "fingerprint": fp, "inst": tr}
				if tc.reported {
					if tc.wanctl != nil {
						reg["wanctl_version"] = tc.wanctl
					}
					if tc.opencu != nil {
						reg["opencu_version"] = tc.opencu
					}
				}
				if tr == "ws" && tc.opencu == "<bad>" {
					reg["wanctl_version"] = map[string]any{"not": "a string"}
					reg["opencu_version"] = false
				}
				r.versionsMu.Lock()
				var revision uint64
				if state := r.versions["alice/"+id]; state != nil {
					revision = state.revision
				}
				r.versionsMu.Unlock()
				accepted := func() bool {
					r.versionsMu.Lock()
					defer r.versionsMu.Unlock()
					state := r.versions["alice/"+id]
					return state != nil && state.revision > revision && !state.running
				}
				waitForReport := func() {
					for {
						var got, gotOpenCU string
						var reported bool
						err := db.QueryRow(`SELECT wanctl_version,opencu_version,versions_reported_at IS NOT NULL FROM devices WHERE device_id=$1`, id).Scan(&got, &gotOpenCU, &reported)
						if err != nil {
							t.Fatal(err)
						}
						if got == tc.wantWanctl && gotOpenCU == tc.wantOpenCU && reported == tc.reported && accepted() {
							break
						}
						if ctx.Err() != nil {
							t.Fatal("registration not persisted")
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				if tr == "ws" {
					conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", admission.Header("tok"))
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					if err := json.NewEncoder(conn).Encode(reg); err != nil {
						t.Fatal(err)
					}
					waitForReport()
					conn.Close()
				} else {
					q := url.Values{}
					for k, v := range reg {
						q.Set(k, fmt.Sprint(v))
					}
					q.Set("fp", fp)
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/h/poll?"+q.Encode(), nil)
					if err != nil {
						t.Fatal(err)
					}
					admission.SetBearer(req, "tok")
					done := make(chan struct{})
					go func() {
						defer close(done)
						resp, err := srv.Client().Do(req)
						if err == nil {
							resp.Body.Close()
						}
					}()
					waitForReport()
					cancel()
					<-done
				}
				cancel()
				check(tc.wantWanctl, tc.wantOpenCU, tc.reported)
			}
		})
	}
	if err := p.RecordDeviceVersions(context.Background(), "alice", id, "v0.9.0", "0.3.99", true); err != nil {
		t.Fatal(err)
	}
	// Endpoint shape for a read-only share: versions and comparison need no manage grant.
	req := httptest.NewRequest(http.MethodGet, "/admin/devices?namespace=bob", nil)
	req.Header.Set("X-Admin-Secret", "admin")
	rec := httptest.NewRecorder()
	r.adminDevices(rec, req)
	var out struct {
		Devices []map[string]any `json:"devices"`
		Latest  string           `json:"latest_wanctl_version"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Devices) != 1 {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	if out.Latest != "v1.0.0" || out.Devices[0]["wanctl_status"] != "behind" || out.Devices[0]["opencu_version"] != "0.3.99" || out.Devices[0]["manage"] != false {
		t.Fatalf("shape: %s", rec.Body.String())
	}
	if rows, err := p.ListDevices("charlie"); err != nil || len(rows) != 0 {
		t.Fatalf("unshared namespace got versions: %v %v", rows, err)
	}
	if _, err := db.Exec(`UPDATE acl SET revoked_at=now() WHERE grantee_namespace='bob'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := p.ListDevices("bob"); err != nil || len(rows) != 0 {
		t.Fatalf("revoked share got versions: %v %v", rows, err)
	}

}
