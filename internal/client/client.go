// Package client is the controller. It dials the relay's /dial endpoint for a
// target device, completes the client-side mutual-TLS handshake (TOFU pinning),
// and runs exec/file requests. Designed to be driven from a terminal or an
// agent's shell tool: streams output, propagates the remote exit code.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"wanctl/internal/config"
	"wanctl/internal/httpconn"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// ErrNoToken is returned by New when no token can be found in env or stored
// credentials. Callers (MCP server etc.) detect this via errors.Is to redirect
// the user to a login flow rather than printing a raw error.
var ErrNoToken = errors.New("no token: run wanctl_login (MCP) or `wanctl login` (CLI), or set WANCTL_TOKEN")

// RejectError is returned when a device rejects a controller's connection,
// typically because the controller fingerprint hasn't been paired yet. Callers
// (CLI / MCP / portal) inspect PairingURL to surface a one-click trust link.
type RejectError struct {
	Reason     string // device-supplied reason
	PairingURL string // empty unless this rejection is fixable by trusting
}

// Error renders the same text the CLI prints to stderr. Both fields surface so
// a human (or AI) sees both why and where to fix it.
func (e *RejectError) Error() string {
	if e.PairingURL != "" {
		return fmt.Sprintf(
			"device 未信任此控制端。请把下面这条链接发给设备主人，让 ta 在浏览器里点一次「信任」，然后重试本命令：\n\n  %s\n\n(链接 5 分钟内有效；reason: %s)",
			e.PairingURL, e.Reason,
		)
	}
	return fmt.Sprintf("device rejected this controller: %s", e.Reason)
}

// Client is the controller node.
type Client struct {
	id        *transport.Identity
	known     *transport.Store
	relayURL  string
	token     string
	transport string // "ws" (default) or "http"
	label     string // self-description sent at pairing (WANCTL_LABEL)
}

// SetLabel overrides the controller's self-description (who/why), shown to the
// device owner at pairing time and in audit.
func (c *Client) SetLabel(l string) { c.label = l }

// New loads identity + config from env (WANCTL_RELAY, WANCTL_TOKEN).
func New() (*Client, error) {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return nil, err
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		return nil, err
	}
	relayURL := config.EnvOr("WANCTL_RELAY", config.DefaultRelay)
	token := config.EnvOr("WANCTL_TOKEN", config.StoredToken())
	if token == "" {
		return nil, ErrNoToken
	}
	tr := config.EnvOr("WANCTL_TRANSPORT", config.DefaultTransport)
	c := NewWith(id, known, relayURL, token, tr)
	c.label = os.Getenv("WANCTL_LABEL")
	return c, nil
}

// NewWith builds a client from explicit config (used by the portal, which has
// its own identity/token and does not read controller env vars).
func NewWith(id *transport.Identity, known *transport.Store, relayURL, token, tr string) *Client {
	if tr == "" {
		tr = "ws"
	}
	return &Client{id: id, known: known, relayURL: strings.TrimRight(relayURL, "/"), token: token, transport: tr}
}

// Identity exposes this controller's fingerprint.
func (c *Client) Identity() *transport.Identity { return c.id }

// Peers lists online devices the token can see.
func (c *Client) Peers(ctx context.Context) ([]string, error) {
	path := "/peers"
	if c.transport == "http" {
		path = "/h/peers"
	}
	httpURL := strings.Replace(c.relayURL, "ws", "http", 1) + path + "?token=" + c.token
	req, _ := http.NewRequestWithContext(ctx, "GET", httpURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("peers: relay returned %d", resp.StatusCode)
	}
	var out struct{ Devices []string }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Devices, nil
}

// pinName is the logical name used for TOFU; use the device part of target.
func pinName(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

func (c *Client) resolve(ctx context.Context, target string) (string, error) {
	if target != "" {
		return target, nil
	}
	devs, err := c.Peers(ctx)
	if err != nil {
		return "", err
	}
	if len(devs) == 1 {
		return devs[0], nil
	}
	if len(devs) == 0 {
		return "", fmt.Errorf("no devices online for this token")
	}
	return "", fmt.Errorf("multiple devices online; pass --target: %s", strings.Join(devs, ", "))
}

func (c *Client) connect(ctx context.Context, target string) (*tls.Conn, error) {
	return c.connectKind(ctx, target, protocol.KindHello)
}

func (c *Client) connectKind(ctx context.Context, target, helloKind string) (*tls.Conn, error) {
	target, err := c.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	var nc net.Conn
	if c.transport == "http" {
		nc, err = c.dialHTTP(ctx, target)
	} else {
		nc, err = c.dialWS(ctx, target)
	}
	if err != nil {
		return nil, err
	}
	return c.finishHandshake(ctx, nc, target, helloKind)
}

func (c *Client) dialWS(ctx context.Context, target string) (net.Conn, error) {
	url := c.relayURL + "/dial?token=" + c.token + "&target=" + target
	nc, resp, err := wsconn.Dial(ctx, url, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial relay (%d): is %q online?", resp.StatusCode, target)
		}
		return nil, err
	}
	return nc, nil
}

func (c *Client) dialHTTP(ctx context.Context, target string) (net.Conn, error) {
	base := strings.Replace(c.relayURL, "ws", "http", 1)
	dialURL := base + "/h/dial?token=" + c.token + "&target=" + target
	req, _ := http.NewRequestWithContext(ctx, "GET", dialURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("dial relay (%d): is %q online?", resp.StatusCode, target)
	}
	var out struct{ Session string }
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Session == "" {
		return nil, fmt.Errorf("relay did not assign a session")
	}
	return httpconn.Dial(ctx, base, out.Session, "client", c.token)
}

func (c *Client) finishHandshake(ctx context.Context, nc net.Conn, target, helloKind string) (*tls.Conn, error) {
	dr, err := transport.ClientHandshake(ctx, nc, pinName(target), c.id, c.known)
	if err != nil {
		return nil, err
	}
	if dr.FirstSeen {
		fmt.Fprintf(os.Stderr, "wanctl: pinned new device %q identity %s\n", target, transport.ShortFingerprint(dr.PeerFP))
	}
	host, _ := os.Hostname()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: helloKind, Role: "client", Name: host, Label: c.label, Version: "1"}); err != nil {
		dr.Conn.Close()
		return nil, err
	}
	reply, err := protocol.ReadMessage(dr.Conn)
	if err != nil {
		dr.Conn.Close()
		return nil, err
	}
	if reply.Kind == protocol.KindReject {
		dr.Conn.Close()
		return nil, rejectError(reply)
	}
	if reply.Kind != protocol.KindOK {
		dr.Conn.Close()
		return nil, fmt.Errorf("unexpected device reply: %s", reply.Kind)
	}
	return dr.Conn, nil
}

// rejectError wraps a device-side reject message as a typed *RejectError so
// callers (MCP, portal, tests) can extract PairingURL programmatically with
// errors.As, while the CLI still prints the same friendly text via Error().
func rejectError(m protocol.Message) error {
	return &RejectError{Reason: m.Reason, PairingURL: m.PairingURL}
}

// OpenConsole dials target and opens a control-plane (console) session,
// returning the authenticated TLS conn. The caller drives the console RPC /
// notification protocol over it (see internal/portal/deviceconn.go).
func (c *Client) OpenConsole(ctx context.Context, target string) (*tls.Conn, error) {
	return c.connectKind(ctx, target, protocol.KindConsoleHello)
}

// Pair performs a control-plane handshake against target with no follow-on data
// operation, purely to surface whether this controller is already trusted by
// the device. On success (trusted=true) the connection is closed immediately.
// When the device rejects the controller because it has not paired this
// fingerprint yet, Pair swallows the *RejectError and returns the device-side
// pairing URL via pairingURL with err=nil — callers (CLI / MCP / portal) can
// show that URL to the user without parsing an error.
//
// Any other dial / handshake failure (target offline, token bad, relay error,
// reject with no PairingURL) propagates as err.
func (c *Client) Pair(ctx context.Context, target string) (trusted bool, pairingURL string, err error) {
	conn, err := c.connect(ctx, target)
	if err != nil {
		var rej *RejectError
		if errors.As(err, &rej) && rej.PairingURL != "" {
			return false, rej.PairingURL, nil
		}
		return false, "", err
	}
	conn.Close()
	return true, "", nil
}

// Logs streams matching event-log JSON lines to stdout. Filters: type, grep,
// since (RFC3339), limit (0 = all).
func (c *Client) Logs(ctx context.Context, target, logType, grep, since string, limit int) error {
	return c.LogsTo(ctx, target, logType, grep, since, limit, os.Stdout)
}

// LogsTo is the same as Logs but writes the event lines to out instead of
// os.Stdout — used by the MCP server to capture them.
func (c *Client) LogsTo(ctx context.Context, target, logType, grep, since string, limit int, out io.Writer) error {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindLogs, LogType: logType, Grep: grep, Since: since, Limit: limit,
	}); err != nil {
		return err
	}
	for {
		ft, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return err
		}
		switch ft {
		case protocol.FrameStdout:
			out.Write(payload)
		case protocol.FrameJSON:
			m, _ := protocol.DecodeMessage(payload)
			switch m.Kind {
			case protocol.KindExit:
				return nil
			case protocol.KindError:
				return fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return rejectError(m)
			}
		}
	}
}

// Exec runs a command on target, streaming output to os.Stdout/Stderr and
// returning the remote exit code. cwd (optional) sets the working directory
// (and is the policy scope) on the device. Convenience wrapper around ExecTo.
func (c *Client) Exec(ctx context.Context, target, command string, oneShot bool, cwd string) (int, error) {
	return c.ExecTo(ctx, target, command, oneShot, cwd, os.Stdout, os.Stderr)
}

// ExecTo is the same as Exec but lets callers (the MCP server) supply their own
// writers to capture stdout/stderr into buffers.
func (c *Client) ExecTo(ctx context.Context, target, command string, oneShot bool, cwd string, stdout, stderr io.Writer) (int, error) {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExec, Command: command, OneShot: oneShot, Cwd: cwd}); err != nil {
		return -1, err
	}
	for {
		ft, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return -1, err
		}
		switch ft {
		case protocol.FrameStdout:
			stdout.Write(payload)
		case protocol.FrameStderr:
			stderr.Write(payload)
		case protocol.FrameJSON:
			m, perr := protocol.DecodeMessage(payload)
			if perr != nil {
				return -1, perr
			}
			switch m.Kind {
			case protocol.KindExit:
				return m.Code, nil
			case protocol.KindError:
				return -1, fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return -1, rejectError(m)
			}
		}
	}
}
