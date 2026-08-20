package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"wanctl/internal/config"
)

// cmdAdmin manages admission invites against the relay's admin API. It is an
// operator tool: it authenticates with WANCTL_ADMIN_SECRET, the same secret
// the portal holds, so it works before any user exists — which is exactly when
// invites are needed.
func cmdAdmin(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: wanctl admin invite [--github LOGIN] | invites | invite-revoke ID")
	}
	switch args[0] {
	case "invite":
		fs := flag.NewFlagSet("admin invite", flag.ExitOnError)
		github := fs.String("github", "", "pre-approve this GitHub login instead of minting a code")
		fs.Parse(args[1:])
		body := map[string]string{}
		if *github != "" {
			body["github_login"] = *github
		}
		var out struct {
			ID          int    `json:"id"`
			Code        string `json:"code"`
			GitHubLogin string `json:"github_login"`
		}
		if err := adminCall("POST", "/admin/invites", body, &out); err != nil {
			return err
		}
		if out.Code != "" {
			fmt.Printf("invite #%d created — share this code (shown only once):\n%s\n", out.ID, out.Code)
		} else {
			fmt.Printf("invite #%d created — GitHub user %q can now sign in\n", out.ID, out.GitHubLogin)
		}
		return nil
	case "invites":
		var invites []struct {
			ID              int        `json:"id"`
			GitHubLogin     string     `json:"github_login"`
			CreatedAt       time.Time  `json:"created_at"`
			UsedAt          *time.Time `json:"used_at"`
			UsedByNamespace string     `json:"used_by_namespace"`
			HasCode         bool       `json:"has_code"`
		}
		if err := adminCall("GET", "/admin/invites", nil, &invites); err != nil {
			return err
		}
		if len(invites) == 0 {
			fmt.Println("no invites")
			return nil
		}
		for _, in := range invites {
			kind := "code"
			if in.GitHubLogin != "" {
				kind = "github:" + in.GitHubLogin
			}
			state := "unused"
			if in.UsedAt != nil {
				state = "used by " + in.UsedByNamespace + " at " + in.UsedAt.Format(time.RFC3339)
			}
			fmt.Printf("#%-4d %-30s %s (created %s)\n", in.ID, kind, state, in.CreatedAt.Format(time.RFC3339))
		}
		return nil
	case "invite-revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: wanctl admin invite-revoke ID")
		}
		id, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invite id must be a number, got %q", args[1])
		}
		if err := adminCall("POST", "/admin/invites/revoke", map[string]int{"id": id}, nil); err != nil {
			return err
		}
		fmt.Printf("invite #%d revoked\n", id)
		return nil
	default:
		return fmt.Errorf("unknown admin subcommand %q (want invite | invites | invite-revoke)", args[0])
	}
}

func adminCall(method, path string, body, out any) error {
	relay, err := config.Relay()
	if err != nil {
		return err
	}
	secret := os.Getenv("WANCTL_ADMIN_SECRET")
	if secret == "" {
		return fmt.Errorf("WANCTL_ADMIN_SECRET is not set (the relay's admin secret)")
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(relay, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Admin-Secret", secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("relay admin %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
