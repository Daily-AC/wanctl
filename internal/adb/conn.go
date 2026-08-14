package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Conn is an authenticated connection to an adbd.
type Conn struct {
	c        net.Conn
	features map[string]bool
	banner   string
	limit    uint32
	// verifyChecksum stays true only until the device tells us its protocol
	// version. Anything from 0x01000001 on sends a zero checksum by design.
	verifyChecksum bool

	mu     sync.Mutex // one command at a time; streams are not multiplexed here
	nextID uint32
}

// hostBanner is what this side reports in CNXN. The feature list is the part
// adbd acts on: shell_v2 is what makes an exit code available at all, and
// without it a failed command is indistinguishable from a successful one.
const hostBanner = "host::features=shell_v2,cmd,stat_v2"

// ErrPublicKeyPending means adbd has been handed this agent's public key and is
// waiting for a human to accept it on the device. It is not a failure: the same
// call succeeds once someone taps Allow, and callers should say so rather than
// reporting a generic auth error.
var ErrPublicKeyPending = errors.New("adb: the device is showing an \"Allow USB debugging?\" dialog for wanctl's key; accept it on the device screen")

// publicKeyGrace is how long to wait for someone to answer that dialog before
// reporting it. Short on purpose: the caller (elevate.ADB.Probe) is on a five
// second budget, and "there is a dialog waiting" is a more useful answer than a
// connection that hangs.
const publicKeyGrace = 3 * time.Second

// isTimeout reports whether err is a network read timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Dial connects to an adbd and completes CNXN/AUTH.
//
// tlsConfig, when non-nil, is used if the device demands STLS (Android 11+
// wireless debugging always does). A plain `adb tcpip` port does not, and
// passing nil there is correct.
func Dial(ctx context.Context, addr string, key *Key, tlsConfig TLSConfigFunc) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Start permissive: the very first packet a modern adbd sends already
	// carries a zero checksum, so there is no window in which insisting on it
	// would be correct.
	c := &Conn{c: nc, limit: maxPayload, features: map[string]bool{}, nextID: 1}
	if deadline, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(deadline)
	}
	if err := c.handshake(key, tlsConfig); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return c, nil
}

func (c *Conn) handshake(key *Key, tlsConfig TLSConfigFunc) error {
	if err := c.send(message{Command: cmdCnxn, Arg0: protocolVersion, Arg1: maxPayload,
		Data: []byte(hostBanner + "\x00")}); err != nil {
		return err
	}
	sentPublicKey := false
	for {
		m, err := c.recv()
		if err != nil {
			return err
		}
		switch m.Command {
		case cmdCnxn:
			c.acceptBanner(m)
			return nil
		case cmdStls:
			if tlsConfig == nil {
				return errors.New("adb: the device requires TLS (Android 11+ wireless debugging), " +
					"which needs a paired key; pair the device first")
			}
			if err := c.startTLS(tlsConfig); err != nil {
				return err
			}
			// Nothing is sent here, and that silence is load-bearing. adbd
			// calls handle_online() and send_connect() itself the instant the
			// handshake succeeds (daemon/adb_wifi.cpp,
			// adbd_wifi_secure_connect), so its CNXN is already on the way and
			// the loop only has to keep reading.
			//
			// A second CNXN from this side reads as a brand-new connection:
			// handle_new_connection() opens with handle_offline(), and for a
			// use_tls transport it answers with another STLS request instead of
			// coming back online. Everything still looks perfect — the banner
			// parses, the features negotiate — and then every stream is dropped
			// without a reply, because adb.cpp's A_OPEN case begins
			// `if (!t->online || p->msg.arg0 == 0) break;`, a bare break with
			// no CLSE and no log line.
			//
			// Measured on an OPPO PGBM10 (Android 14) on 2026-08-14: the extra
			// CNXN put "host-32: offline" in logcat one millisecond after
			// "adbd_wifi_secure_connect: connected host-32", and shell opens
			// timed out from there on.
		case cmdAuth:
			if m.Arg0 != authToken {
				return fmt.Errorf("adb: unexpected AUTH subtype %d", m.Arg0)
			}
			if sentPublicKey {
				// adbd re-issued a token after being given the key, which means
				// the human has not decided yet. Reporting a signature failure
				// here would send someone hunting for a bug that is a dialog.
				return ErrPublicKeyPending
			}
			sig, err := key.Sign(m.Data)
			if err != nil {
				return err
			}
			if err := c.send(message{Command: cmdAuth, Arg0: authSignature, Data: sig}); err != nil {
				return err
			}
			// If the signature is not recognized adbd sends another token; the
			// next iteration answers it with the public key instead.
			next, err := c.recv()
			if err != nil {
				return err
			}
			if next.Command == cmdCnxn {
				c.acceptBanner(next)
				return nil
			}
			if next.Command != cmdAuth {
				return fmt.Errorf("adb: unexpected %s during authentication", next.Command)
			}
			if err := c.send(message{Command: cmdAuth, Arg0: authPublicKey, Data: key.PublicKey()}); err != nil {
				return err
			}
			sentPublicKey = true
			// From here adbd sends NOTHING until a human answers the "Allow USB
			// debugging?" dialog it just raised — it does not re-issue a token
			// and does not close the connection. Measured on a Xiaomi Mi 10
			// Ultra (Android 11) on 2026-08-14: the dialog appears with this
			// key's fingerprint and the socket simply goes quiet.
			//
			// Waiting for the caller's own deadline would surface that as a
			// bare "i/o timeout", which says nothing about the dialog now
			// waiting on the device's screen. Give it a short grace period —
			// enough for someone already holding the phone — and otherwise
			// report what is actually happening.
			if err := c.c.SetReadDeadline(time.Now().Add(publicKeyGrace)); err != nil {
				return err
			}
			m, err := c.recv()
			if err != nil {
				if isTimeout(err) {
					return ErrPublicKeyPending
				}
				return err
			}
			if err := c.c.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
			// After the key is offered, CNXN is the only signal that means
			// accepted. Anything else — typically yet another token — means it
			// was not, and looping back to wait for more would hang until the
			// caller's deadline for no new information.
			if m.Command != cmdCnxn {
				return ErrPublicKeyPending
			}
			c.acceptBanner(m)
			return nil
		default:
			return fmt.Errorf("adb: unexpected %s during connect", m.Command)
		}
	}
}

// acceptBanner records what the device says it is and can do. The smaller of
// the two payload ceilings governs.
func (c *Conn) acceptBanner(m message) {
	c.banner = strings.TrimRight(string(m.Data), "\x00")
	if m.Arg1 > 0 && m.Arg1 < c.limit {
		c.limit = m.Arg1
	}
	// arg0 is the device's protocol version.
	c.verifyChecksum = m.Arg0 < versionSkipChecksum
	// The banner is "<type>::<k>=<v>;<k>=<v>;…", e.g.
	// "device::ro.product.name=x;ro.product.model=y;features=shell_v2,cmd".
	// features is one of the semicolon-separated properties, not one of the
	// "::"-separated sections.
	props := c.banner
	if i := strings.Index(props, "::"); i >= 0 {
		props = props[i+2:]
	}
	for _, prop := range strings.Split(props, ";") {
		if !strings.HasPrefix(prop, "features=") {
			continue
		}
		for _, f := range strings.Split(strings.TrimPrefix(prop, "features="), ",") {
			if f != "" {
				c.features[f] = true
			}
		}
	}
}

// Banner is the device's self-description from CNXN (e.g.
// "device::ro.product.name=…;…"). Useful in a status line and in tests.
func (c *Conn) Banner() string { return c.banner }

// HasFeature reports whether the device advertised a protocol feature.
func (c *Conn) HasFeature(name string) bool { return c.features[name] }

func (c *Conn) send(m message) error { return m.write(c.c) }

func (c *Conn) recv() (message, error) { return readMessage(c.c, c.limit, c.verifyChecksum) }

// Close tears down the connection.
func (c *Conn) Close() error { return c.c.Close() }

// shell protocol (shell_v2) packet ids.
const (
	shellStdout = 1
	shellStderr = 2
	shellExit   = 3
)

// Shell runs one command and streams its merged output to out, returning the
// command's exit code.
//
// Merging stdout and stderr matches what wanctl's own exec does everywhere else
// — the controller receives a single ordered stream — so the distinction is
// dropped here deliberately rather than lost.
func (c *Conn) Shell(ctx context.Context, command string, out io.Writer) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if deadline, ok := ctx.Deadline(); ok {
		_ = c.c.SetDeadline(deadline)
		defer c.c.SetDeadline(time.Time{})
	}
	v2 := c.features["shell_v2"]
	service := "shell,v2,raw:" + command
	if !v2 {
		// Pre-shell_v2 devices (before Android 7) cannot report an exit code at
		// all. Say so rather than inventing a zero.
		service = "shell:" + command
	}

	local := c.nextID
	c.nextID++
	if err := c.send(message{Command: cmdOpen, Arg0: local, Data: []byte(service + "\x00")}); err != nil {
		return -1, err
	}

	var remote uint32
	var pending []byte
	exit := -1
	sawExit := false
	for {
		m, err := c.recv()
		if err != nil {
			if errors.Is(err, io.EOF) && sawExit {
				return exit, nil
			}
			return -1, err
		}
		// Ignore traffic for other streams; nothing else opens one on this
		// connection, but a stale CLSE can arrive after a previous command.
		if m.Arg1 != 0 && m.Arg1 != local {
			continue
		}
		switch m.Command {
		case cmdOkay:
			remote = m.Arg0
		case cmdWrte:
			remote = m.Arg0
			// Every WRTE must be acknowledged or adbd stops sending.
			if err := c.send(message{Command: cmdOkay, Arg0: local, Arg1: remote}); err != nil {
				return -1, err
			}
			if !v2 {
				if _, err := out.Write(m.Data); err != nil {
					return -1, err
				}
				continue
			}
			pending = append(pending, m.Data...)
			var perr error
			if pending, exit, sawExit, perr = drainShellPackets(pending, out, exit, sawExit); perr != nil {
				return -1, perr
			}
		case cmdClse:
			_ = c.send(message{Command: cmdClse, Arg0: local, Arg1: remote})
			if !v2 {
				// No exit code exists on this path. Zero would be a claim the
				// protocol did not make.
				return 0, errNoExitCode
			}
			if !sawExit {
				return -1, errors.New("adb: the device closed the stream without an exit code")
			}
			return exit, nil
		}
	}
}

// errNoExitCode marks the pre-shell_v2 path, where the exit status is simply
// not carried. Callers decide whether that is acceptable; nothing pretends.
var errNoExitCode = errors.New("adb: this device's adbd predates shell_v2, so no exit code is available")

// ErrNoExitCode reports whether err is the missing-exit-code condition.
func ErrNoExitCode(err error) bool { return errors.Is(err, errNoExitCode) }

// drainShellPackets consumes whole shell-protocol packets from buf, writing
// stdout/stderr to out and recording an exit status. Partial packets stay in
// the returned buffer: a WRTE boundary is not a packet boundary.
func drainShellPackets(buf []byte, out io.Writer, exit int, sawExit bool) ([]byte, int, bool, error) {
	for len(buf) >= 5 {
		id := buf[0]
		length := binary.LittleEndian.Uint32(buf[1:5])
		if uint32(len(buf)-5) < length {
			break // incomplete; wait for more
		}
		payload := buf[5 : 5+length]
		switch id {
		case shellStdout, shellStderr:
			if _, err := out.Write(payload); err != nil {
				return nil, exit, sawExit, err
			}
		case shellExit:
			if length >= 1 {
				exit, sawExit = int(payload[0]), true
			}
		}
		buf = buf[5+length:]
	}
	return buf, exit, sawExit, nil
}
