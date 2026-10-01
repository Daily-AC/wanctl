package elevate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wanctl/internal/adb"
)

// PortEnv names the adbd port to connect to, bypassing discovery. Set by hand
// for an `adb tcpip 5555` device, and by the app for a wireless-debugging port
// it discovered over mDNS.
const PortEnv = "WANCTL_ADB_PORT"

// StateEnv points at the JSON file the Android app writes. The adb channel
// reads the wireless-debugging port from it, because discovering that port
// means receiving mDNS, and receiving mDNS on Android means holding a
// MulticastLock — a framework call the Go child cannot make. Same mechanism
// v0.1.12 built for battery state.
const StateEnv = "WANCTL_DEVICE_STATE_FILE"

// tcpipPort is where `adb tcpip 5555` puts adbd. Tried last: it is plaintext
// and only present when somebody deliberately enabled it.
const tcpipPort = 5555

// offWhen is why wireless debugging is off on a phone whose owner turned it on.
// It is not a setting that stays put: AOSP's AdbDebuggingManager clears it on
// boot, when Wi-Fi is switched off or disconnects, and when the BSSID changes —
// which includes roaming between two nodes of one mesh network. Each of those
// needs a human to turn it back on, and an owner who is not told why will
// reasonably insist it is on: they remember turning it on.
const offWhen = "Android switches it off on every reboot, when Wi-Fi disconnects and when the phone joins a different access point"

// The adb channel's link states. The app's 提权通道 row and the portal's ADB
// card turn each into one sentence for the owner, so they name what the owner
// would do next rather than what failed.
const (
	// LinkConnected: adbd ran `id` as shell.
	LinkConnected = "connected"
	// LinkNoPort: nothing answered on any candidate port. Wireless debugging
	// is off, or on but its port not discovered yet; turning it back on
	// recovers without pairing again.
	LinkNoPort = "no_port"
	// LinkUnpaired: adbd answered and refused wanctl's key — never paired, or
	// the pairing lapsed. Pairing again is the fix.
	LinkUnpaired = "unpaired"
	// LinkError: anything else (a pending "Allow USB debugging?" dialog, an
	// adbd that runs as an app uid); Reason says what.
	LinkError = "error"
)

// linkError is a connect failure that knows its Link state.
type linkError struct {
	link, msg string
}

func (e *linkError) Error() string { return e.msg }

func linkOf(err error) string {
	var le *linkError
	if errors.As(err, &le) {
		return le.link
	}
	return LinkError
}

// adbProbeTimeout bounds a connection attempt. Loopback either answers
// immediately or is not listening.
const adbProbeTimeout = 5 * time.Second

// ADB runs commands through the device's own adbd, reached over loopback.
//
// This is the channel for a phone that is not rooted:
// its owner turns on Developer options → Wireless debugging, and the agent
// connects to the adbd already running on the same device. What it gets is
// uid 2000 in the shell domain — the same identity `adb shell` has, which is
// what the rest of the adb surface requires.
//
// It does not survive a reboot, a Wi-Fi drop or a move to another access point:
// Android turns wireless debugging off for each of them (offWhen). Say so rather
// than letting someone discover it after a power cut (docs/android.md).
type ADB struct {
	// dial is injected in tests; nil means the real adb client.
	dial func(ctx context.Context, addr string, key *adb.Key) (shellConn, error)
	// ports, when non-nil, overrides discovery in tests.
	ports func() ([]int, string)

	configDir string
	name      string

	mu   sync.Mutex
	conn shellConn
	port int

	// running counts commands in Run, so a background probe can stay off a
	// connection that is busy (adb.Conn runs one command at a time).
	running atomic.Int32
}

// Busy reports whether a command is running on this channel right now.
func (a *ADB) Busy() bool { return a.running.Load() > 0 }

// shellConn is the part of *adb.Conn this channel uses.
type shellConn interface {
	Shell(ctx context.Context, command string, out io.Writer) (int, error)
	Close() error
}

// NewADB builds the adb self-connect channel. configDir is where the agent's
// adb key is kept; name is what the device shows in its "Allow debugging?"
// prompt and records in adb_keys afterwards.
func NewADB(configDir, name string) *ADB {
	return &ADB{configDir: configDir, name: name}
}

func (a *ADB) Kind() Kind { return KindADB }

// candidatePorts lists where adbd might be listening, most-specific first, and
// explains what it would mean if none of them answered.
func (a *ADB) candidatePorts() ([]int, string) {
	if a.ports != nil {
		return a.ports()
	}
	var ports []int
	// Comma-separated, because Android assigns the wireless-debugging port at
	// random and there is no way to ask for it from the app sandbox without
	// mDNS. Listing candidates is how a device gets configured before that
	// discovery exists; each is dialed in turn and a wrong one simply fails to
	// speak the protocol.
	for _, field := range strings.Split(os.Getenv(PortEnv), ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(field)); err == nil && p > 0 && p < 65536 {
			ports = append(ports, p)
		}
	}
	statePath := os.Getenv(StateEnv)
	statePort := portFromState(statePath)
	if statePort > 0 {
		ports = append(ports, statePort)
	}
	ports = append(ports, tcpipPort)
	if statePath != "" && statePort == 0 {
		// The app is watching mDNS for this device's adbd and has nothing
		// current to show for it. Turning wireless debugging off and on
		// makes adbd advertise a fresh port, which also covers the rarer case
		// of it being on while the app missed or outlived the announcement.
		return ports, "the wanctl app has no current wireless-debugging port for this device: " +
			"turn on Developer options → Wireless debugging (" + offWhen + "), " +
			"or, if it is already on, turn it off and on again so the app sees its new port"
	}
	return ports, "turn on Developer options → Wireless debugging on the device (" + offWhen + ")"
}

func (a *ADB) Probe(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, adbProbeTimeout)
	defer cancel()

	st := a.probe(ctx)
	if st.Available {
		st.Link = LinkConnected
	} else if st.Link == "" {
		st.Link = LinkError
	}
	return st
}

func (a *ADB) probe(ctx context.Context) Status {
	conn, port, reused, err := a.connect(ctx)
	if err != nil {
		return Status{Available: false, Reason: err.Error(), Link: linkOf(err)}
	}
	var sb strings.Builder
	code, err := conn.Shell(ctx, "id", &sb)
	if err != nil && !adb.ErrNoExitCode(err) && reused {
		// The connection an earlier command left open can die while nobody
		// is using it — some ROMs drop the socket when the screen locks — and
		// adbd is still right there. Run redials once for this; a probe that
		// did not would call the channel unavailable, and the Manager would
		// believe it for a whole probe TTL. A connection dialed just now gets
		// no second attempt: it cannot have gone stale, and its failure, with
		// the banner describeConn adds, is the diagnosis.
		a.reset()
		if conn, port, _, err = a.connect(ctx); err != nil {
			return Status{Available: false, Reason: err.Error(), Link: linkOf(err)}
		}
		sb.Reset()
		code, err = conn.Shell(ctx, "id", &sb)
	}
	out := firstLine(strings.TrimSpace(sb.String()))
	switch {
	case err != nil && !adb.ErrNoExitCode(err):
		detail := describeConn(conn)
		a.reset()
		return Status{Available: false, Reason: fmt.Sprintf(
			"connected to adbd on port %d but `id` failed: %v%s", port, err, detail)}
	case code != 0 && !adb.ErrNoExitCode(err):
		a.reset()
		return Status{Available: false, Reason: fmt.Sprintf("adbd ran `id` with exit %d: %s", code, out)}
	case !strings.Contains(out, "uid="):
		a.reset()
		return Status{Available: false, Reason: "adbd answered but did not report a uid: " + out}
	case strings.Contains(out, "uid=10"):
		// Connected to something running as an app, not as shell. That is not
		// an elevation, and using it would be worse than refusing.
		a.reset()
		return Status{Available: false, Reason: "the adbd on port " + strconv.Itoa(port) +
			" runs as an app uid, not shell: " + out}
	}
	return Status{Available: true, Detail: out}
}

func (a *ADB) Run(ctx context.Context, command, cwd string, out io.Writer) (int, error) {
	a.running.Add(1)
	defer a.running.Add(-1)
	conn, _, _, err := a.connect(ctx)
	if err != nil {
		return -1, err
	}
	if cwd != "" {
		// adb has no working-directory concept, so it becomes a prefix. The
		// path is quoted; the command is already shell source and is
		// concatenated exactly once, so nothing here re-splits it.
		command = "cd " + quotePOSIX(cwd) + " && " + command
	}
	code, err := conn.Shell(ctx, command, out)
	if err != nil && !adb.ErrNoExitCode(err) {
		// A dead connection is worth one transparent retry: wireless debugging
		// drops the socket when the screen locks on some ROMs.
		a.reset()
		conn, _, _, derr := a.connect(ctx)
		if derr != nil {
			return -1, err
		}
		return conn.Shell(ctx, command, out)
	}
	return code, err
}

// connect returns a live connection, dialing on first use and after a reset.
// reused reports that the connection was already open, which is the only kind
// that can have died while nothing was using it.
func (a *ADB) connect(ctx context.Context) (conn shellConn, port int, reused bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		return a.conn, a.port, true, nil
	}
	key, err := adb.LoadOrCreateKey(a.configDir, a.name)
	if err != nil {
		return nil, 0, false, fmt.Errorf("adb key: %w", err)
	}
	dial := a.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string, key *adb.Key) (shellConn, error) {
			// The TLS config is built lazily: a plain `adb tcpip` port never
			// asks for it, and an Android 11+ wireless-debugging port always
			// does. It must carry the key pairing registered — see
			// Key.TLSCertificate.
			return adb.Dial(ctx, addr, key, key.TLSConfig)
		}
	}
	ports, hint := a.candidatePorts()
	var last error
	var failures []string
	rejected := false
	for _, p := range ports {
		conn, err := dial(ctx, fmt.Sprintf("127.0.0.1:%d", p), key)
		if err == nil {
			a.conn, a.port = conn, p
			return conn, p, false, nil
		}
		if errors.Is(err, adb.ErrPublicKeyPending) {
			// Distinct from "nothing is listening": someone has to tap Allow on
			// the device, and trying other ports would bury that.
			return nil, 0, false, &linkError{LinkError, fmt.Sprintf("adbd on port %d is waiting for someone to allow wanctl's key on the device screen", p)}
		}
		if errors.Is(err, adb.ErrKeyRejected) {
			// adbd answered on this port and refused the key. Its error
			// already says the pairing is the fix; ending with "turn on
			// wireless debugging" would send the owner to a switch that is
			// already on.
			rejected = true
		}
		// Every port's failure is kept. Reporting only the last one hid the
		// real diagnosis behind the fallback port's timeout — the useful error
		// came from the port that actually had adbd on it.
		failures = append(failures, fmt.Sprintf("%d: %v", p, err))
		last = err
	}
	if last == nil {
		return nil, 0, false, errors.New("no adbd port to try")
	}
	reason := fmt.Sprintf("could not reach adbd on this device (%s)", strings.Join(failures, "; "))
	if rejected {
		return nil, 0, false, &linkError{LinkUnpaired, reason}
	}
	return nil, 0, false, &linkError{LinkNoPort, reason + "; " + hint}
}

func (a *ADB) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
}

// Close drops the connection to adbd.
func (a *ADB) Close() error {
	a.reset()
	return nil
}

func quotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// describeConn adds what the device said about itself to a failure, so a
// connection that completes and then goes quiet is diagnosable without a
// rebuild. adbd's banner names its protocol features, and a missing shell_v2
// changes which service is opened.
func describeConn(c shellConn) string {
	type describer interface {
		Banner() string
		HasFeature(string) bool
	}
	d, ok := c.(describer)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" [device banner: %q; shell_v2=%v]", d.Banner(), d.HasFeature("shell_v2"))
}
