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
	"os"
	"strings"
	"sync"

	"wanctl/internal/protocol"
	"wanctl/internal/server"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// Options configures an agent run.
type Options struct {
	RelayURL string // ws(s)://host[:port], no path
	Token    string
	Name     string
	Shell    string
	AutoYes  bool
}

// Agent is a running controlled node.
type Agent struct {
	id    *transport.Identity
	known *transport.Store
	opts  Options

	sessMu   sync.Mutex
	sessions map[string]*server.ShellSession
	stdin    *bufio.Reader
}

// New constructs an Agent with loaded identity and controller allow-list.
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
	if opts.Name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "wanctl-agent"
		}
		opts.Name = h
	}
	return &Agent{id: id, known: known, opts: opts, sessions: map[string]*server.ShellSession{}, stdin: bufio.NewReader(os.Stdin)}, nil
}

// Run connects the control channel and serves sessions until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
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
			server.HandleFilePut(conn, m)
		case protocol.KindFileGet:
			server.HandleFileGet(conn, m)
		default:
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + m.Kind})
			return
		}
	}
}

func (a *Agent) doExec(conn *tls.Conn, fp string, m protocol.Message) {
	out := server.FrameWriter(conn, protocol.FrameStdout)
	var code int
	var err error
	if m.OneShot {
		code, err = server.RunOneShot(a.opts.Shell, m.Command, out)
	} else {
		sess, serr := a.session(fp)
		if serr != nil {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: serr.Error()})
			return
		}
		code, err = sess.Exec(m.Command, out)
	}
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExit, Code: code})
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
