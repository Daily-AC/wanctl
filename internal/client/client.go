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
	"unicode"
	"unicode/utf8"

	"wanctl/internal/admission"
	"wanctl/internal/config"
	"wanctl/internal/httpconn"
	"wanctl/internal/protocol"
	"wanctl/internal/relayhttp"
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
	id            *transport.Identity
	known         *transport.Store
	relayURL      string
	token         string
	transport     string       // "ws" (default) or "http"
	label         string       // self-description sent at pairing (WANCTL_LABEL)
	httpc         *http.Client // relay HTTP client
	workspaceLink *WorkspaceLink
}

// SetLabel overrides the controller's self-description (who/why), shown to the
// device owner at pairing time and in audit.
func (c *Client) SetLabel(l string) { c.label = l }

// New loads identity + config from env (WANCTL_RELAY, WANCTL_TOKEN), falling
// back to the persisted relay setting.
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
	if relayURL == "" {
		relayURL, err = config.Relay()
		if err != nil {
			return nil, err
		}
	}
	if tr == "" {
		tr = config.Transport()
	}
	c := NewWith(id, known, relayURL, token, tr)
	c.label = config.EnvOr("WANCTL_LABEL", config.StoredLabel())
	return c, nil
}

// RelayURL exposes the relay this client resolved to (for status output).
func (c *Client) RelayURL() string { return c.relayURL }

// NewWith builds a client from explicit config (used by the portal, which has
// its own identity/token and does not read controller env vars).
func NewWith(id *transport.Identity, known *transport.Store, relayURL, token, tr string) *Client {
	if tr == "" {
		tr = "ws"
	}
	return &Client{id: id, known: known, relayURL: strings.TrimRight(relayURL, "/"), token: token, transport: tr, httpc: &http.Client{Transport: relayhttp.Shared()}}
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

// PeersWithAliases lists canonical device IDs and their display labels. Legacy
// devices retain their name as the route until upgraded.
func (c *Client) PeersWithAliases(ctx context.Context) ([]string, map[string]string, error) {
	info, err := c.peerInfo(ctx)
	if err != nil {
		return nil, nil, err
	}
	if info.Aliases == nil {
		info.Aliases = map[string]string{}
	}
	return info.Devices, info.Aliases, nil
}

// Peers is everything the relay will say about the devices one token can
// reach. Namespace is part of it because a bare device ID is only half a
// target: pins, shares and logs are all keyed by the canonical
// "namespace/device", and the caller cannot build that on its own.
type Peers struct {
	Namespace string
	Devices   []string
	Aliases   map[string]string
	Shared    []SharedDevice
}

// PeersAndShared is PeersWithAliases plus the namespace and the devices other
// namespaces have shared with this token. Older relays omit the shared list
// entirely.
func (c *Client) PeersAndShared(ctx context.Context) (Peers, error) {
	info, err := c.peerInfo(ctx)
	if err != nil {
		return Peers{}, err
	}
	if info.Aliases == nil {
		info.Aliases = map[string]string{}
	}
	return Peers{Namespace: info.Namespace, Devices: info.Devices, Aliases: info.Aliases, Shared: info.Shared}, nil
}

// PeerAliases lists IDs and unambiguous display labels for target completion,
// both short and namespace-qualified.
func (c *Client) PeerAliases(ctx context.Context) ([]string, error) {
	info, err := c.peerInfo(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, label := range info.Aliases {
		counts[strings.ToLower(label)]++
	}
	aliases := make([]string, 0, len(info.Devices)*4)
	for _, device := range info.Devices {
		aliases = append(aliases, device, info.Namespace+"/"+device)
		if alias := info.Aliases[device]; alias != "" && counts[strings.ToLower(alias)] == 1 {
			aliases = append(aliases, alias, info.Namespace+"/"+alias)
		}
	}
	for _, shared := range info.Shared {
		aliases = append(aliases, shared.Target)
		if shared.Label != "" && !strings.ContainsAny(shared.Label, " /") {
			aliases = append(aliases, shared.Owner+"/"+shared.Label)
		}
	}
	return aliases, nil
}

// SharedDevice is a device another namespace granted this token access to. Its
// Target is namespace-qualified and can be passed to --target verbatim.
type SharedDevice struct {
	Owner  string `json:"owner"`
	Device string `json:"device"`
	Label  string `json:"label"`
	Target string `json:"target"`
	Online bool   `json:"online"`
}

type peerInfo struct {
	Namespace string            `json:"namespace"`
	Devices   []string          `json:"devices"`
	Aliases   map[string]string `json:"aliases"`
	Shared    []SharedDevice    `json:"shared"`
}

func (c *Client) peerInfo(ctx context.Context) (peerInfo, error) {
	path := "/peers"
	if c.transport == "http" {
		path = "/h/peers"
	}
	base, err := config.RelayHTTPOrigin(c.relayURL)
	if err != nil {
		return peerInfo{}, err
	}
	httpURL := base + path
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

// pinName is the canonical owner namespace plus device ID.
func pinName(target string) string { return strings.TrimSpace(target) }

func (c *Client) resolveLegacy(ctx context.Context, target string) (string, error) {
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
		return info.Namespace + "/" + canonicalDevice(info, target), nil
	}
	if len(info.Devices) == 1 {
		return info.Namespace + "/" + info.Devices[0], nil
	}
	if len(info.Devices) == 0 {
		return "", fmt.Errorf("no devices online for this token")
	}
	return "", fmt.Errorf("multiple devices online; pass --target: %s", strings.Join(info.Devices, ", "))
}

// resolve canonicalizes both qualified and unqualified names before pinning.
// Only a 404 (older relay) permits the old peers-based resolution path.
func (c *Client) resolve(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		var err error
		target, err = c.resolveLegacy(ctx, target)
		if err != nil {
			return "", err
		}
	}
	if strings.Contains(target, "/") {
		parts := strings.Split(target, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("invalid target %q: expected owner/device", target)
		}
	}
	base, err := config.RelayHTTPOrigin(c.relayURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/resolve?"+url.Values{"target": {target}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	admission.SetBearer(req, c.token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.resolveLegacy(ctx, target)
	}
	if resp.StatusCode != 200 {
		if detail := relayExplanation(resp); detail != "" {
			return "", fmt.Errorf("%s", detail)
		}
		return "", fmt.Errorf("cannot resolve device (%d); for duplicate names use a device ID", resp.StatusCode)
	}
	var out struct {
		Target       string `json:"target"`
		LegacyTarget string `json:"legacy_target"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	parts := strings.Split(out.Target, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("relay returned invalid device target")
	}
	if out.LegacyTarget != "" && c.known != nil {
		legacyParts := strings.Split(out.LegacyTarget, "/")
		if len(legacyParts) != 2 || legacyParts[0] != parts[0] || legacyParts[1] == "" {
			return "", fmt.Errorf("relay returned invalid legacy device target")
		}
		if _, exists := c.known.GetByName(out.Target); !exists {
			if old, exists := c.known.GetByName(out.LegacyTarget); exists {
				// Copy the stored pin, never the relay's offered fingerprint. A mismatch
				// still fails PinServer or the subsequent TLS handshake.
				if err := c.known.Pin(out.Target, old.Fingerprint, false); err != nil {
					return "", err
				}
			}
		}
	}
	return out.Target, nil
}

// canonicalDevice maps an owner-assigned alias back to the device's real name,
// the way the relay resolves it: a real name always wins over an alias.
//
// Without this the pin store, which is keyed by the target string as typed,
// holds "ns/客厅" and "ns/bench-02" as two separate identities — so the first
// dial by alias asks the operator to confirm a fingerprint they already
// confirmed for the same machine. Aliases only exist inside the caller's own
// namespace, which is exactly what /peers reports, so this needs no extra call.
func canonicalDevice(info peerInfo, target string) string {
	for _, device := range info.Devices {
		if device == target {
			return target
		}
	}
	for device, alias := range info.Aliases {
		if strings.EqualFold(alias, target) {
			return device
		}
	}
	return target
}

// Pinned reports the identity this controller has already pinned for a
// canonical "namespace/device" name. Purely local: it reads the known-servers
// store and never dials the relay or the device.
func (c *Client) Pinned(name string) (transport.Peer, bool) {
	if c.known == nil {
		return transport.Peer{}, false
	}
	return c.known.GetByName(name)
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
	nc, target, err := c.dialTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	// TLS handshaking observes ctx itself, but the following application hello
	// reads from a connection that intentionally outlives its dial context.
	defer wsconn.CloseOnCancel(ctx, nc)()
	return c.finishHandshake(ctx, nc, target, helloKind)
}

// dialTarget opens a relayed connection to target and returns it with the
// canonical name whose pin the handshake must match.
func (c *Client) dialTarget(ctx context.Context, target string) (net.Conn, string, error) {
	var (
		nc  net.Conn
		err error
	)
	if strings.TrimSpace(target) == "" {
		// Only /resolve can pick the single online device, so it has to
		// answer before there is anything to dial.
		if target, err = c.resolve(ctx, target); err != nil {
			return nil, "", err
		}
		nc, err = c.dial(ctx, target)
	} else {
		// The relay resolves a dial target the same way /resolve does, so the
		// two requests run side by side and the command saves a round trip.
		// The resolved name only selects the pinned fingerprint: if the two
		// ever disagreed, the device that answers would not match the pin and
		// the handshake below would refuse it.
		type resolved struct {
			target string
			err    error
		}
		rc := make(chan resolved, 1)
		go func() {
			t, err := c.resolve(ctx, target)
			rc <- resolved{t, err}
		}()
		raw := strings.TrimSpace(target)
		nc, err = c.dial(ctx, raw)
		r := <-rc
		if r.err != nil {
			if nc != nil {
				nc.Close()
			}
			return nil, "", r.err
		}
		target = r.target
		if err != nil && target != raw {
			// A relay from before dial-side resolution only knows the
			// canonical name, so the saved round trip is given back.
			nc, err = c.dial(ctx, target)
		}
	}
	if err != nil {
		return nil, "", err
	}
	return nc, target, nil
}

func (c *Client) dial(ctx context.Context, target string) (net.Conn, error) {
	if c.transport == "http" {
		return c.dialHTTP(ctx, target)
	}
	return c.dialWS(ctx, target)
}

func (c *Client) dialWS(ctx context.Context, target string) (net.Conn, error) {
	dialURL := c.relayURL + "/dial?" + url.Values{"target": {target}}.Encode()
	nc, resp, err := wsconn.Dial(ctx, dialURL, admission.Header(c.token))
	if err != nil {
		if resp != nil {
			if detail := relayExplanation(resp); detail != "" {
				return nil, fmt.Errorf("dial relay (%d): %s", resp.StatusCode, detail)
			}
			return nil, fmt.Errorf("dial relay (%d): is %q online?", resp.StatusCode, target)
		}
		return nil, err
	}
	return nc, nil
}

func (c *Client) dialHTTP(ctx context.Context, target string) (net.Conn, error) {
	base, err := config.RelayHTTPOrigin(c.relayURL)
	if err != nil {
		return nil, err
	}
	dialURL := base + "/h/dial?" + url.Values{"target": {target}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, "GET", dialURL, nil)
	admission.SetBearer(req, c.token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if detail := relayExplanation(resp); detail != "" {
			return nil, fmt.Errorf("dial relay (%d): %s", resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("dial relay (%d): is %q online?", resp.StatusCode, target)
	}
	var out struct{ Session string }
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Session == "" {
		return nil, fmt.Errorf("relay did not assign a session")
	}
	nc, err := httpconn.Dial(ctx, base, out.Session, "client", c.token)
	if err == nil && resp.Header.Get(httpconn.UpSeqCapabilityHeader) == "1" {
		httpconn.MarkOrdered(nc)
	}
	if err == nil && resp.Header.Get(httpconn.DownWindowCapabilityHeader) == "4" {
		httpconn.MarkWindow(nc)
	}
	return nc, err
}

func (c *Client) finishHandshake(ctx context.Context, nc net.Conn, target, helloKind string) (*tls.Conn, error) {
	conn, err := c.sendHello(ctx, nc, target, helloKind)
	if err != nil {
		return nil, err
	}
	if err := checkHelloReply(conn, helloKind); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// deviceHandshakeTimeout bounds the TLS handshake with the device. A relay
// can hand a session to a device whose agent has just exited — an update or
// restart, before the relay notices — and nothing then ever answers: the
// controller re-polled an empty session for as long as anyone let it run.
// A shaped link needs a few seconds for the handshake's round trips, so this
// only has to be far past that.
var deviceHandshakeTimeout = 60 * time.Second

// sendHello completes the TLS handshake and writes the hello, without waiting
// for the device to answer it.
func (c *Client) sendHello(ctx context.Context, nc net.Conn, target, helloKind string) (*tls.Conn, error) {
	hctx, cancel := context.WithTimeout(ctx, deviceHandshakeTimeout)
	defer cancel()
	dr, err := transport.ClientHandshake(hctx, nc, pinName(target), c.id, c.known)
	if err != nil {
		if ctx.Err() == nil && hctx.Err() != nil {
			return nil, fmt.Errorf("device %s did not answer the handshake within %s; it may be restarting, try again", target, deviceHandshakeTimeout)
		}
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
	return dr.Conn, nil
}

// checkHelloReply reads the device's answer to a hello.
func checkHelloReply(conn io.Reader, helloKind string) error {
	reply, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if reply.Kind == protocol.KindReject {
		return rejectError(reply)
	}
	if reply.Kind != protocol.KindOK {
		return fmt.Errorf("unexpected device reply: %s", reply.Kind)
	}
	if helloKind == protocol.KindWorkspaceHello && !reply.WorkspaceReuse {
		return fmt.Errorf("device did not negotiate reusable workspace authorization")
	}
	return nil
}

// helloConn is a session whose hello is on the wire but unanswered. The
// request that follows goes out behind the hello instead of a round trip
// later, and the first read collects the device's answer to the hello before
// anything else. A device reads its connection in order, so an agent of any
// version sees hello then request exactly as if the client had waited; one
// that refuses the hello answers with a reject and hangs up, and that reject
// is what the first read returns, unwrapped, so errors.As still finds it.
type helloConn struct {
	*tls.Conn
	checked bool
	err     error
}

func (h *helloConn) Read(p []byte) (int, error) {
	if !h.checked {
		h.checked = true
		h.err = checkHelloReply(h.Conn, protocol.KindHello)
	}
	if h.err != nil {
		return 0, h.err
	}
	return h.Conn.Read(p)
}

// connectPipelined is connect for an operation whose caller returns the first
// read error as it is. Callers that interpret a failed read — fileOp turns
// one into "result unknown" — must use connect, or a refused hello would be
// reported as a lost result.
func (c *Client) connectPipelined(ctx context.Context, target string) (net.Conn, error) {
	nc, target, err := c.dialTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer wsconn.CloseOnCancel(ctx, nc)()
	conn, err := c.sendHello(ctx, nc, target, protocol.KindHello)
	if err != nil {
		return nil, err
	}
	return &helloConn{Conn: conn}, nil
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
	conn, err := c.connectPipelined(ctx, target)
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

// cancelGrace is how long a cancelled Exec waits for the device to answer its
// cancel frame before closing the connection anyway. The device normally
// replies at once (the command is killed and an error frame comes back); the
// close is only there so a device that cannot answer never hangs the caller.
const cancelGrace = 2 * time.Second

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
	// ElevateOptional says the command is one the device may legitimately run
	// without an elevation channel, so a reply that names none is not the
	// silent failure elevationHonoured otherwise catches. Exactly one command
	// is like this: a screenshot, which needs root on Android and needs nothing
	// on a laptop. It is asked for elevated either way so that both get the
	// same policy class, and only the device knows which it is.
	ElevateOptional bool
	// SpillAfter asks the device to keep the whole output in a file of its own
	// once it grows past this many bytes, and to name that file on exit. A
	// caller that will have to truncate what it shows sets it to the size it
	// can show; zero asks for nothing and leaves no file.
	SpillAfter int64
}

// ExecOutcome is everything one exec reports beyond its output bytes.
type ExecOutcome struct {
	Code int
	// SpillPath is where the device kept the whole output, empty when nothing
	// was spilled — either because the output was small, because the caller
	// asked for no spill, or because the device is running an agent from before
	// spilling existed.
	SpillPath string
	// SpillBytes is the output's true length in bytes, which is what the tail a
	// caller shows is a tail of. It is reported even when SpillPath is empty,
	// and that is how a device that tried and could not keep the output is told
	// apart from an agent too old to have been asked: the first counted, the
	// second reports nothing at all.
	SpillBytes int64
	// SpillKept is how much of the output the device's copy actually holds. Less
	// than SpillBytes means the file is a prefix and the middle is gone.
	SpillKept int64
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
	if !req.Elevate || req.ElevateOptional || m.ElevatedVia != "" {
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
	out, err := c.ExecOut(ctx, req, stdout, stderr)
	return out.Code, err
}

// ExecOut is ExecTo with the rest of what the device reports: where it kept the
// whole output when the caller asked for a spill, and how long that output was.
func (c *Client) ExecOut(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (ExecOutcome, error) {
	failed := ExecOutcome{Code: -1}
	conn, err := c.connectPipelined(ctx, req.Target)
	if err != nil {
		return failed, err
	}
	defer conn.Close()
	msg := protocol.Message{
		Kind: protocol.KindExec, Command: req.Command, OneShot: req.OneShot, Cwd: req.Cwd,
		Elevate: req.Elevate, Via: req.Via, SpillAfter: req.SpillAfter,
	}
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return failed, err
	}
	// A cancelled context (Ctrl-C at the terminal, an MCP host abandoning the
	// call) has to reach the device, or the command runs to completion there
	// with nobody left to read it (#37). Say so on the wire first, then drop
	// the connection so the device also notices if the frame never lands.
	//
	// Neither reaches an agent from before this fix, and nothing here can: such
	// an agent reads the connection only between commands, so both the frame
	// and the close are seen after the command has already finished. It then
	// answers the frame with "unknown request" and ends the session — harmless,
	// but the command ran to completion. Cancellation needs a device running
	// this version.
	stopCancel := context.AfterFunc(ctx, func() {
		_ = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindCancel})
		time.AfterFunc(cancelGrace, func() { conn.Close() })
	})
	defer stopCancel()
	return execOver(ctx, conn, req, stdout, stderr)
}

// execOver reads one command's frames to their end on an already-open session.
// It is separate from the dial for the same reason fileOpOver is: the wire
// behaviour — including what a session that ends mid-command looks like — can
// then be tested against a stand-in device.
//
// Note what it does NOT do with a connection that ends after the request was
// sent: claim the device could not run the command. That inference is only
// sound when the device says so, and a command that was already running when
// the link dropped is the commoner case.
func execOver(ctx context.Context, rw io.ReadWriter, req ExecRequest, stdout, stderr io.Writer) (ExecOutcome, error) {
	failed := ExecOutcome{Code: -1}
	for {
		ft, payload, err := protocol.ReadFrame(rw)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failed, ctxErr
			}
			return failed, err
		}
		switch ft {
		case protocol.FrameStdout:
			stdout.Write(payload)
		case protocol.FrameStderr:
			stderr.Write(payload)
		case protocol.FrameJSON:
			m, perr := protocol.DecodeMessage(payload)
			if perr != nil {
				return failed, perr
			}
			switch m.Kind {
			case protocol.KindExit:
				return ExecOutcome{
					Code: m.Code, SpillPath: m.Path, SpillBytes: m.Size, SpillKept: m.SpillKept,
				}, elevationHonoured(req, m)
			case protocol.KindError:
				// The spill rides on the error frame too, and is carried out
				// with it: a command that failed after emitting megabytes is
				// exactly when the caller most needs to know where the rest of
				// the output is, and dropping the path here would have thrown
				// that away at the last step.
				lost := ExecOutcome{Code: -1, SpillPath: m.Path, SpillBytes: m.Size, SpillKept: m.SpillKept}
				// A device that honoured our cancel reports the killed command
				// as an error. The caller asked for that, so it reads as
				// cancellation rather than as a device-side failure.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return lost, ctxErr
				}
				return lost, fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return failed, rejectError(m)
			}
		}
	}
}

// relayExplanation returns the relay's own account of a rejected request. The
// relay answers a refused dial with a short plain-text line ("no device %q in
// namespace ..."), and printing only the status code threw that away: users saw
// a bare 403 and had no way to tell a missing grant from a mistyped target.
//
// Anything that does not look like that line - an HTML error page from a proxy
// in front of the relay, a long body, control characters - is discarded and the
// caller falls back to its own wording.
func relayExplanation(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	text := strings.TrimSpace(string(raw))
	if text == "" || len(text) > 400 || strings.HasPrefix(text, "<") || !utf8.ValidString(text) {
		return ""
	}
	for _, r := range text {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return ""
		}
	}
	return strings.Join(strings.Fields(text), " ")
}
