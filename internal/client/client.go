// Package client is the controller. It dials the relay's /dial endpoint for a
// target device, completes the client-side mutual-TLS handshake (TOFU pinning),
// and runs exec/file requests. Designed to be driven from a terminal or an
// agent's shell tool: streams output, propagates the remote exit code.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"wanctl/internal/httpconn"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// Client is the controller node.
type Client struct {
	id        *transport.Identity
	known     *transport.Store
	relayURL  string
	token     string
	transport string // "ws" (default) or "http"
}

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
	relayURL := os.Getenv("WANCTL_RELAY")
	token := os.Getenv("WANCTL_TOKEN")
	if relayURL == "" || token == "" {
		return nil, fmt.Errorf("set WANCTL_RELAY and WANCTL_TOKEN")
	}
	tr := os.Getenv("WANCTL_TRANSPORT")
	return NewWith(id, known, relayURL, token, tr), nil
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
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: helloKind, Role: "client", Name: host, Version: "1"}); err != nil {
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
		return nil, fmt.Errorf("device rejected this controller: %s\n  -> approve it on the device (GUI/console pairing prompt)", reply.Reason)
	}
	if reply.Kind != protocol.KindOK {
		dr.Conn.Close()
		return nil, fmt.Errorf("unexpected device reply: %s", reply.Kind)
	}
	return dr.Conn, nil
}

// OpenConsole dials target and opens a control-plane (console) session,
// returning the authenticated TLS conn. The caller drives the console RPC /
// notification protocol over it (see internal/portal/deviceconn.go).
func (c *Client) OpenConsole(ctx context.Context, target string) (*tls.Conn, error) {
	return c.connectKind(ctx, target, protocol.KindConsoleHello)
}

// Logs pulls matching event-log lines from the target device and streams them to
// stdout (JSON lines). Filters: type, grep, since (RFC3339), limit (0 = all).
func (c *Client) Logs(ctx context.Context, target, logType, grep, since string, limit int) error {
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
			os.Stdout.Write(payload)
		case protocol.FrameJSON:
			m, _ := protocol.DecodeMessage(payload)
			switch m.Kind {
			case protocol.KindExit:
				return nil
			case protocol.KindError:
				return fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return fmt.Errorf("%s", m.Reason)
			}
		}
	}
}

// Exec runs a command on target, streaming output, returning the remote code.
// cwd (optional) sets the working directory and is the policy scope on the device.
func (c *Client) Exec(ctx context.Context, target, command string, oneShot bool, cwd string) (int, error) {
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
			os.Stdout.Write(payload)
		case protocol.FrameStderr:
			os.Stderr.Write(payload)
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
				return -1, fmt.Errorf("%s", m.Reason)
			}
		}
	}
}
