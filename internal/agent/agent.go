// Package agent is the controlled node. It dials the relay outbound, registers a
// device name, and for each session the relay opens it completes the server-side
// mutual-TLS handshake, applies TOFU authorization, and serves exec/file
// requests using the forked server handlers.
package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"wanctl/internal/gui"
	"wanctl/internal/httpconn"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/server"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"

	"golang.org/x/term"
)

// Options configures an agent run.
type Options struct {
	RelayURL  string // ws(s)://host[:port], no path
	Token     string
	Name      string
	Shell     string
	AutoYes   bool
	Transport string      // "ws" (default) or "http" (proxy-agnostic)
	Mode      policy.Mode // "normal" (default) or "bypass"
	GUIPort   int         // >0 enables the local web GUI (approvals/monitor) on 127.0.0.1:GUIPort
}

// Agent is a running controlled node.
type Agent struct {
	id     *transport.Identity
	known  *transport.Store
	opts   Options
	engine *policy.Engine
	gui    *gui.Server
	apprMu sync.Mutex
	appr   policy.Approver

	sessMu   sync.Mutex
	sessions map[string]*server.ShellSession
	stdin    *bufio.Reader
}

// New constructs an Agent with loaded identity, controller allow-list, and
// policy engine. The approver is the device console when attended, deny when
// running headless (so unattended agents are safe unless in bypass mode).
func New(opts Options) (*Agent, error) {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return nil, err
	}
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		return nil, err
	}
	if opts.Shell == "" {
		opts.Shell = server.DefaultShell()
	}
	if opts.Mode == "" {
		opts.Mode = policy.ModeNormal
	}
	if opts.Name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "wanctl-agent"
		}
		opts.Name = h
	}
	engine, err := policy.Open("rules.json", opts.Mode)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		id: id, known: known, opts: opts, engine: engine,
		sessions: map[string]*server.ShellSession{}, stdin: bufio.NewReader(os.Stdin),
	}
	switch {
	case opts.Mode == policy.ModeBypass:
		a.appr = policy.AllowApprover{}
	case opts.GUIPort > 0:
		a.gui = gui.New(engine, gui.Info{Device: opts.Name, Fingerprint: id.Fingerprint, Relay: opts.RelayURL})
		a.appr = a.gui // humans approve in the browser
	case term.IsTerminal(int(os.Stdin.Fd())):
		a.appr = policy.NewConsoleApprover(os.Stdin, os.Stdout)
	default:
		a.appr = policy.DenyApprover{} // headless + miss = deny (pre-load rules to allow)
	}
	return a, nil
}

// setApprover overrides the approver (used by tests).
func (a *Agent) setApprover(ap policy.Approver) {
	a.apprMu.Lock()
	a.appr = ap
	a.apprMu.Unlock()
}

// gate authorizes a request: bypass/pre-approved pass; otherwise ask the
// approver and optionally remember a rule. Returns true if the op may proceed.
func (a *Agent) gate(req policy.Request) bool {
	if a.engine.Mode() == policy.ModeBypass {
		return true
	}
	if a.engine.Allowed(req) {
		return true
	}
	a.apprMu.Lock()
	appr := a.appr
	a.apprMu.Unlock()
	d := appr.Ask(req)
	if !d.Allow {
		return false
	}
	if d.Remember {
		a.engine.Add(policy.RuleFor(req, d.Scope))
	}
	return true
}

// Run connects the control channel and serves sessions until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.gui != nil {
		addr := fmt.Sprintf("127.0.0.1:%d", a.opts.GUIPort)
		go func() {
			if err := a.gui.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "wanctl: GUI server stopped: %v\n", err)
			}
		}()
	}
	if a.opts.Transport == "http" {
		return a.runHTTP(ctx)
	}
	ctrlURL := strings.TrimRight(a.opts.RelayURL, "/") + "/agent?token=" + a.opts.Token
	nc, resp, err := wsconn.Dial(ctx, ctrlURL, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == 401 {
			return fmt.Errorf("relay rejected token (401)")
		}
		return fmt.Errorf("connect relay: %w", err)
	}
	defer nc.Close()
	enc := json.NewEncoder(nc)
	if err := enc.Encode(map[string]string{"op": "register", "device": a.opts.Name}); err != nil {
		return err
	}
	fmt.Printf("wanctl agent %q online via %s\n  fingerprint: %s\n", a.opts.Name, a.opts.RelayURL, a.id.Fingerprint)

	dec := json.NewDecoder(bufio.NewReader(nc))
	for {
		var msg struct{ Op, Session, URL string }
		if err := dec.Decode(&msg); err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("control channel closed: %w", err)
			}
		}
		if msg.Op == "open" {
			go a.serveSession(ctx, msg.URL)
		}
	}
}

func (a *Agent) serveSession(ctx context.Context, relPath string) {
	url := strings.TrimRight(a.opts.RelayURL, "/") + relPath
	nc, _, err := wsconn.Dial(ctx, url, nil)
	if err != nil {
		return
	}
	a.handleSession(ctx, nc)
}

// handleSession completes the server-side handshake and serves requests on an
// already-established transport conn (WebSocket or HTTP).
func (a *Agent) handleSession(ctx context.Context, nc net.Conn) {
	conn, fp, err := transport.ServerHandshake(ctx, nc, a.id)
	if err != nil {
		return
	}
	defer conn.Close()

	hello, err := protocol.ReadMessage(conn)
	if err != nil || hello.Kind != protocol.KindHello {
		return
	}
	if !a.authorize(fp, hello.Name) {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "not authorized by the device"})
		return
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, Name: a.opts.Name})
	a.serve(conn, fp)
}

func (a *Agent) authorize(fp, name string) bool {
	if a.known.Has(fp) {
		a.known.Touch(fp)
		return true
	}
	if a.opts.AutoYes {
		a.known.Add(fp, name)
		fmt.Printf("[auto-trust] new controller %q paired: %s\n", name, fp)
		return true
	}
	fmt.Printf("\n──────────────────────────────────────────────\n")
	fmt.Printf("  Pairing request from a new controller\n    name: %s\n    fingerprint: %s\n", name, fp)
	fmt.Printf("  Allow it to control this device? [y/N]: ")
	line, _ := a.stdin.ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans == "y" || ans == "yes" {
		a.known.Add(fp, name)
		fmt.Printf("  ✓ paired.\n")
		return true
	}
	fmt.Printf("  ✗ denied.\n")
	return false
}

func (a *Agent) serve(conn *tls.Conn, fp string) {
	for {
		m, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		switch m.Kind {
		case protocol.KindExec:
			a.doExec(conn, fp, m)
		case protocol.KindFilePut:
			if !a.gate(policy.Request{Kind: policy.KindWrite, Path: m.Path, Peer: fp}) {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "write denied by device policy: " + m.Path})
				continue
			}
			server.HandleFilePut(conn, m)
		case protocol.KindFileGet:
			if !a.gate(policy.Request{Kind: policy.KindRead, Path: m.Path, Peer: fp}) {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "read denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileGet(conn, m)
		default:
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + m.Kind})
			return
		}
	}
}

func (a *Agent) doExec(conn *tls.Conn, fp string, m protocol.Message) {
	if !a.gate(policy.Request{Kind: policy.KindExec, Cmd: m.Command, Cwd: m.Cwd, Peer: fp}) {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "command denied by device policy: " + m.Command})
		return
	}
	command := withCwd(m.Cwd, m.Command)
	out := server.FrameWriter(conn, protocol.FrameStdout)
	var code int
	var err error
	if m.OneShot {
		code, err = server.RunOneShot(a.opts.Shell, command, out)
	} else {
		sess, serr := a.session(fp)
		if serr != nil {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: serr.Error()})
			return
		}
		code, err = sess.Exec(command, out)
	}
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExit, Code: code})
}

// withCwd prefixes a directory change so the command runs in cwd. Uses ';' so it
// works in both POSIX sh and PowerShell.
func withCwd(cwd, cmd string) string {
	if cwd == "" {
		return cmd
	}
	return "cd \"" + cwd + "\"; " + cmd
}

// httpBase converts the relay URL to an HTTP(S) origin for the HTTP transport.
func httpBase(relayURL string) string {
	b := strings.TrimRight(relayURL, "/")
	if strings.HasPrefix(b, "wss:") {
		return "https:" + b[4:]
	}
	if strings.HasPrefix(b, "ws:") {
		return "http:" + b[3:]
	}
	return b
}

// runHTTP drives the proxy-agnostic HTTP transport: long-poll /h/poll for
// sessions to open, and serve each over an httpconn.
func (a *Agent) runHTTP(ctx context.Context) error {
	base := httpBase(a.opts.RelayURL)
	fmt.Printf("wanctl agent %q online via %s (http transport)\n  fingerprint: %s\n", a.opts.Name, base, a.id.Fingerprint)
	hc := &http.Client{Timeout: 35 * time.Second}
	q := url.Values{"token": {a.opts.Token}, "device": {a.opts.Name}}.Encode()
	pollURL := base + "/h/poll?" + q
	for {
		if ctx.Err() != nil {
			return nil
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if strings.Contains(err.Error(), "401") {
				return fmt.Errorf("relay rejected token")
			}
			time.Sleep(2 * time.Second) // backoff then re-poll
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return fmt.Errorf("relay rejected token (401)")
		}
		var msg struct{ Session string }
		json.NewDecoder(resp.Body).Decode(&msg)
		resp.Body.Close()
		if msg.Session != "" {
			go a.serveSessionHTTP(ctx, base, msg.Session)
		}
	}
}

func (a *Agent) serveSessionHTTP(ctx context.Context, base, session string) {
	nc, err := httpconn.Dial(ctx, base, session, "agent", a.opts.Token)
	if err != nil {
		return
	}
	a.handleSession(ctx, nc)
}

func (a *Agent) session(fp string) (*server.ShellSession, error) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if sess, ok := a.sessions[fp]; ok && !sess.Closed() {
		return sess, nil
	}
	sess, err := server.NewShellSession(a.opts.Shell)
	if err != nil {
		return nil, err
	}
	a.sessions[fp] = sess
	return sess, nil
}
