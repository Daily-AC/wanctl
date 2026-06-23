// Command wanctl is a cross-internet remote-control CLI. The same binary runs as
// the relay (`wanctl relay`, on thunderbox), the controlled device
// (`wanctl agent`), and the controller (`wanctl exec/push/pull`). Endpoints meet
// through the relay's WebSocket broker and speak end-to-end mutual TLS, so the
// relay only sees ciphertext.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"wanctl/internal/agent"
	"wanctl/internal/client"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

const usage = `wanctl — control a device across the internet over an encrypted, relayed channel

USAGE
  wanctl relay [--addr :8080]                 run the relay (thunderbox); tokens from WANCTL_TOKENS
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

func cmdRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	spec := os.Getenv("WANCTL_TOKENS")
	if spec == "" {
		return fmt.Errorf("set WANCTL_TOKENS=\"token:namespace,...\"")
	}
	r := relay.New(relay.EnvTokenStore(spec))
	fmt.Printf("wanctl relay listening on %s\n", *addr)
	return http.ListenAndServe(*addr, r.Handler())
}

func cmdAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	name := fs.String("name", "", "device name (default hostname)")
	relayURL := fs.String("relay", os.Getenv("WANCTL_RELAY"), "relay ws(s) URL")
	token := fs.String("token", os.Getenv("WANCTL_TOKEN"), "access/registration token")
	shell := fs.String("shell", "", "shell (default powershell on Windows, /bin/sh elsewhere)")
	yes := fs.Bool("yes", false, "auto-trust new controllers (unattended)")
	fs.Parse(args)
	if *relayURL == "" || *token == "" {
		return fmt.Errorf("provide --relay and --token (or WANCTL_RELAY/WANCTL_TOKEN)")
	}
	ag, err := agent.New(agent.Options{RelayURL: *relayURL, Token: *token, Name: *name, Shell: *shell, AutoYes: *yes})
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
	fs.Parse(args)
	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if command == "" {
		return fmt.Errorf("no command given")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	code, err := c.Exec(ctx, *target, command, *oneShot)
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
