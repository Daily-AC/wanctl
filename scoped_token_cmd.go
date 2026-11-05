package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"wanctl/internal/relay"
)

// Scoped command tokens: a credential that can run one command on one device.
// See docs/adr/0021-scoped-command-token.md.

const scopedTokenHelp = `wanctl scoped-token subcommands:
  scoped-token create --device DEV --command CMD [--days N] [--label TEXT]
  scoped-token list
  scoped-token revoke ID`

func cmdScopedToken(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Println(scopedTokenHelp)
		return nil
	}
	switch args[0] {
	case "create":
		return scopedTokenCreate(ctx, args[1:])
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: wanctl scoped-token list")
		}
		return scopedTokenList(ctx)
	case "revoke":
		return scopedTokenRevoke(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Println(scopedTokenHelp)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], scopedTokenHelp)
	}
}

func scopedTokenCreate(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("scoped-token create", flag.ContinueOnError))
	device := fs.String("device", "", "device of yours the token may use")
	command := fs.String("command", "", "the one command it may run, exactly as the shell will see it")
	days := fs.Int("days", relay.ScopedDefaultDays, fmt.Sprintf("lifetime in days (1-%d)", relay.ScopedMaxDays))
	label := fs.String("label", "", "a note for yourself about who holds it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device == "" || *command == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: wanctl scoped-token create --device DEV --command CMD [--days N] [--label TEXT]")
	}
	var result struct {
		Token string            `json:"token"`
		Info  relay.ScopedToken `json:"info"`
	}
	err := userRequest(ctx, http.MethodPost, "/u/scoped-tokens", map[string]any{
		"device": *device, "command": *command, "label": *label, "days": *days,
	}, &result)
	if err != nil {
		return err
	}
	// Only the token goes to stdout, so `T=$(wanctl scoped-token create ...)` works.
	fmt.Println(result.Token)
	fmt.Fprintf(os.Stderr, "Scoped token #%d for device %s, valid until %s. Shown once; it cannot be recovered.\n"+
		"It can run exactly this and nothing else:\n  %s\n"+
		"Use it:  WANCTL_TOKEN=<token> wanctl exec --target %s --oneshot -- %s\n"+
		"The device's own rules and pairing still apply to it. Revoke:  wanctl scoped-token revoke %d\n",
		result.Info.ID, result.Info.Device, result.Info.ExpiresAt.Local().Format("2006-01-02"), result.Info.Command,
		result.Info.Device, strconv.Quote(result.Info.Command), result.Info.ID)
	return nil
}

func scopedTokenList(ctx context.Context) error {
	var result struct {
		Tokens []relay.ScopedToken `json:"tokens"`
	}
	if err := userRequest(ctx, http.MethodGet, "/u/scoped-tokens", nil, &result); err != nil {
		return err
	}
	if len(result.Tokens) == 0 {
		fmt.Println("(no scoped tokens)")
		return nil
	}
	for _, t := range result.Tokens {
		state := "valid until " + t.ExpiresAt.Local().Format("2006-01-02")
		switch {
		case t.RevokedAt != nil:
			state = "revoked"
		case time.Now().After(t.ExpiresAt):
			state = "expired"
		}
		label := ""
		if t.Label != "" {
			label = "  [" + t.Label + "]"
		}
		fmt.Printf("#%-4d %-18s %s  device %s%s\n", t.ID, state, strconv.Quote(t.Command), t.Device, label)
	}
	return nil
}

func scopedTokenRevoke(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wanctl scoped-token revoke ID")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil || id <= 0 {
		return fmt.Errorf("usage: wanctl scoped-token revoke ID")
	}
	err = userRequest(ctx, http.MethodPost, "/u/scoped-tokens/revoke", map[string]int{"id": id}, nil)
	if isRelayError(err, http.StatusNotFound, "not-found") {
		return fmt.Errorf("no live scoped token #%d of yours (see `wanctl scoped-token list`)", id)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Revoked scoped token #%d. Its next request is refused.\n", id)
	return nil
}
