// Package client is the controller. It dials the relay's /dial endpoint for a
// target device, completes the client-side mutual-TLS handshake (explicit pinning),
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
	"net/url"
	"os"
	"strings"
	"time"

	"wanctl/internal/admission"
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

// TrustRequiredError requires the caller to confirm an unknown device identity
// out of band before any application data is sent.
type TrustRequiredError struct {
	Target      string
	Fingerprint string
}

func (e *TrustRequiredError) Error() string {
	return fmt.Sprintf("DEVICE IDENTITY CONFIRMATION REQUIRED for %q\n  fingerprint: %s\nVerify it with the device owner, then run:\n  wanctl trust server --target %q --fingerprint %q",
		e.Target, e.Fingerprint, e.Target, e.Fingerprint)
}

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
	transport string       // "ws" (default) or "http"
	label     string       // self-description sent at pairing (WANCTL_LABEL)
	httpc     *http.Client // relay HTTP client (no-proxy variant for the intranet relay)
	lan       bool         // true when this client resolved to the intranet relay
}

// SetLabel overrides the controller's self-description (who/why), shown to the
// device owner at pairing time and in audit.
func (c *Client) SetLabel(l string) { c.label = l }

// New loads identity + config from env (WANCTL_RELAY, WANCTL_TOKEN) and the
// persisted network mode (`wanctl net wan|lan|auto`). An explicit WANCTL_RELAY
// always wins; otherwise "lan" targets the intranet fast-path relay over WS
// (bypassing any HTTP proxy env), and "auto" probes it first, falling back to
// the public relay.
func New() (*Client, error) {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return nil, err
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		return nil, err
	}
	token := config.EnvOr("WANCTL_TOKEN", config.StoredToken())
	if token == "" {
		return nil, ErrNoToken
	}
	relayURL := os.Getenv("WANCTL_RELAY")
	tr := os.Getenv("WANCTL_TRANSPORT")
	lan := false
	if relayURL == "" {
		switch config.StoredNetMode() {
		case "lan":
			if config.LanRelay() == "" {
				return nil, fmt.Errorf("no LAN relay configured (set WANCTL_LAN_RELAY)")
			}
			lan = true
		case "auto":
			lan = LanReachable(600 * time.Millisecond)
		}
		if lan {
			relayURL, tr = config.LanRelay(), "ws"
		} else {
			relayURL, err = config.Relay()
			if err != nil {
				return nil, err
			}
			if tr == "" {
				tr = config.Transport()
			}
		}
	} else if tr == "" {
		tr = config.Transport()
	}
	c := NewWith(id, known, relayURL, token, tr)
	c.label = config.EnvOr("WANCTL_LABEL", config.StoredLabel())
	if lan {
		c.lan = true
		c.httpc = wsconn.NoProxyClient
	}
	return c, nil
}

// LanReachable probes the intranet relay /healthz, bypassing proxy env vars.
func LanReachable(timeout time.Duration) bool {
	lanRelay := config.LanRelay()
	if lanRelay == "" {
		return false
	}
	base := strings.Replace(strings.TrimRight(lanRelay, "/"), "ws", "http", 1)
	hc := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := hc.Get(base + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Lan reports whether this client resolved to the intranet relay.
func (c *Client) Lan() bool { return c.lan }

// RelayURL exposes the relay this client resolved to (for status output).
func (c *Client) RelayURL() string { return c.relayURL }

// NewWith builds a client from explicit config (used by the portal, which has
// its own identity/token and does not read controller env vars).
func NewWith(id *transport.Identity, known *transport.Store, relayURL, token, tr string) *Client {
	if tr == "" {
		tr = "ws"
	}
	return &Client{id: id, known: known, relayURL: strings.TrimRight(relayURL, "/"), token: token, transport: tr, httpc: http.DefaultClient}
}

// Identity exposes this controller's fingerprint.
func (c *Client) Identity() *transport.Identity { return c.id }

// Peers lists online devices the token can see.
func (c *Client) Peers(ctx context.Context) ([]string, error) {
	info, err := c.peerInfo(ctx)
	if err != nil {
		return nil, err
	}
	return info.Devices, nil
}

// PeerAliases lists every exact spelling accepted for an online target: the
// short device name and its namespace-qualified form.
func (c *Client) PeerAliases(ctx context.Context) ([]string, error) {
	info, err := c.peerInfo(ctx)
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(info.Devices)*2)
	for _, device := range info.Devices {
		aliases = append(aliases, device, info.Namespace+"/"+device)
	}
	return aliases, nil
}

type peerInfo struct {
	Namespace string   `json:"namespace"`
	Devices   []string `json:"devices"`
}

func (c *Client) peerInfo(ctx context.Context) (peerInfo, error) {
	path := "/peers"
	if c.transport == "http" {
		path = "/h/peers"
	}
	httpURL := strings.Replace(c.relayURL, "ws", "http", 1) + path
	req, _ := http.NewRequestWithContext(ctx, "GET", httpURL, nil)
	admission.SetBearer(req, c.token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return peerInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return peerInfo{}, fmt.Errorf("peers: relay returned %d", resp.StatusCode)
	}
	var out peerInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return peerInfo{}, err
	}
	if out.Namespace == "" {
		return peerInfo{}, fmt.Errorf("peers: relay omitted token namespace")
	}
	return out, nil
}

// pinName is the canonical owner namespace plus device name.
func pinName(target string) string { return strings.TrimSpace(target) }

func (c *Client) resolve(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if strings.Contains(target, "/") {
		parts := strings.Split(target, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("invalid target %q: expected owner/device", target)
		}
		return target, nil
	}
	info, err := c.peerInfo(ctx)
	if err != nil {
		return "", err
	}
	if target != "" {
		return info.Namespace + "/" + target, nil
	}
	if len(info.Devices) == 1 {
		return info.Namespace + "/" + info.Devices[0], nil
	}
	if len(info.Devices) == 0 {
		return "", fmt.Errorf("no devices online for this token")
	}
	return "", fmt.Errorf("multiple devices online; pass --target: %s", strings.Join(info.Devices, ", "))
}

// PinServer records an explicitly verified fingerprint for a canonical target.
func (c *Client) PinServer(ctx context.Context, target, fingerprint string, replace bool) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("target is required")
	}
	canonical, err := c.resolve(ctx, target)
	if err != nil {
		return "", err
	}
	if err := c.known.Pin(canonical, fingerprint, replace); err != nil {
		return "", err
	}
	return canonical, nil
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
	dialURL := c.relayURL + "/dial?" + url.Values{"target": {target}}.Encode()
	var hc *http.Client
	if c.lan {
		hc = wsconn.NoProxyClient
	}
	nc, resp, err := wsconn.DialWith(ctx, dialURL, admission.Header(c.token), hc)
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
	dialURL := base + "/h/dial?" + url.Values{"target": {target}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, "GET", dialURL, nil)
	admission.SetBearer(req, c.token)
	resp, err := c.httpc.Do(req)
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
		dr.Conn.Close()
		return nil, &TrustRequiredError{Target: target, Fingerprint: dr.PeerFP}
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

// OpenConsole dials target and requests a control-plane (console) session,
// returning the authenticated TLS conn when this client's fingerprint matches
// the device's enrolled console administrator. The caller drives the console
// RPC / notification protocol over it (see internal/portal/deviceconn.go).
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

// ExecRequest is one command to run on a device. It replaced a five-argument
// signature when elevation added two more; the fields are named at every call
// site, which is worth more here than brevity.
type ExecRequest struct {
	Target  string
	Command string
	OneShot bool
	Cwd     string
	// Elevate asks the device to run this through an elevation channel
	// (Android: root, or its own adbd). Via optionally pins one.
	Elevate bool
	Via     string
}

// elevationHonoured checks that a device which reported an exit actually ran
// the command elevated, when that is what was asked.
//
// The hazard is specific and silent. An agent built before elevation existed
// decodes the request's `elevate` field into nothing — encoding/json discards
// unknown fields — runs the command with the app sandbox's own privileges, and
// answers with an ordinary exit. Every visible signal says success. The only
// evidence available is negative: such an agent cannot echo back the channel
// that ran, because it has no concept of one.
//
// The command has already run by the time this is detectable, so the message
// says so. Reporting "elevation not supported" alone would suggest nothing
// happened, and a caller who then re-ran the command elsewhere would be acting
// on a false belief about what this device just did.
func elevationHonoured(req ExecRequest, m protocol.Message) error {
	if !req.Elevate || m.ElevatedVia != "" {
		return nil
	}
	target := req.Target
	if target == "" {
		target = "<device>"
	}
	return fmt.Errorf(
		"device did not elevate this command: it ran with the agent's own privileges (exit %d).\n"+
			"That agent predates elevation support — update the device, then retry.\n"+
			"  wanctl status --target %s", m.Code, target)
}

// Exec runs a command on target, streaming output to os.Stdout/Stderr and
// returning the remote exit code. Convenience wrapper around ExecTo.
func (c *Client) Exec(ctx context.Context, req ExecRequest) (int, error) {
	return c.ExecTo(ctx, req, os.Stdout, os.Stderr)
}

// ExecTo is the same as Exec but lets callers (the MCP server) supply their own
// writers to capture stdout/stderr into buffers.
func (c *Client) ExecTo(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (int, error) {
	conn, err := c.connect(ctx, req.Target)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	msg := protocol.Message{
		Kind: protocol.KindExec, Command: req.Command, OneShot: req.OneShot, Cwd: req.Cwd,
		Elevate: req.Elevate, Via: req.Via,
	}
	if err := protocol.WriteMessage(conn, msg); err != nil {
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
				return m.Code, elevationHonoured(req, m)
			case protocol.KindError:
				return -1, fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return -1, rejectError(m)
			}
		}
	}
}
