package adb

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAdbd is enough of adbd to exercise the client against something that
// enforces the protocol: it checks magic and checksums, demands authentication,
// verifies the signature with the public key it was given, and speaks the
// shell_v2 packet framing.
type fakeAdbd struct {
	t        *testing.T
	ln       net.Listener
	features string
	// requireAuth makes the device send AUTH before accepting CNXN.
	requireAuth bool
	// knownKey, when set, is the only key whose signature is accepted.
	knownKey *rsa.PublicKey
	// neverAccept models a device whose user has not tapped Allow: it keeps
	// issuing tokens after being handed a public key.
	neverAccept bool
	// useTLS models Android 11+ wireless debugging: CNXN is answered with STLS
	// rather than a banner, and the transport's online/offline state follows
	// adbd's rules (see serve).
	useTLS    bool
	tlsConfig *tls.Config
	// lateCnxn records a CNXN that arrived after the TLS handshake — the
	// mistake this fake exists to catch.
	lateCnxn atomic.Bool

	script func(command string) (stdout string, exit byte)
}

func newFakeAdbd(t *testing.T, f *fakeAdbd) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.t, f.ln = t, ln
	if f.features == "" {
		f.features = "shell_v2,cmd"
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return ln.Addr().String()
}

func (f *fakeAdbd) serve(c net.Conn) {
	defer c.Close()
	authed := !f.requireAuth
	gotKey := false
	// online mirrors adbd's transport state, which A_OPEN is gated on. A
	// plaintext transport is online from the start; a TLS one only after its
	// handshake.
	online, upgraded := !f.useTLS, false
	for {
		m, err := readMessage(c, maxPayload, false)
		if err != nil {
			return
		}
		switch m.Command {
		case cmdCnxn:
			if f.useTLS {
				// adb.cpp handle_new_connection(): every CNXN starts with
				// handle_offline(), and a use_tls transport is answered with
				// another STLS request rather than a banner. A client that
				// sends CNXN again after the handshake therefore knocks the
				// transport it just established back offline.
				if upgraded {
					f.lateCnxn.Store(true)
				}
				online = false
				_ = message{Command: cmdStls, Arg0: stlsVersion}.write(c)
				continue
			}
			if !authed {
				_ = message{Command: cmdAuth, Arg0: authToken, Data: make([]byte, 20)}.write(c)
				continue
			}
			f.sendConnected(c)
		case cmdStls:
			tc := tls.Server(c, f.tlsConfig)
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			upgraded, online = true, true
			// adbd_wifi_secure_connect(): handle_online() then send_connect().
			// The banner is sent by the device, unprompted.
			f.sendConnected(c)
		case cmdAuth:
			switch m.Arg0 {
			case authSignature:
				if f.knownKey != nil && !f.neverAccept && verifyToken(f.knownKey, m.Data) {
					authed = true
					f.sendConnected(c)
					continue
				}
				// Unknown signature: ask again, which is what makes the client
				// offer its public key.
				_ = message{Command: cmdAuth, Arg0: authToken, Data: make([]byte, 20)}.write(c)
			case authPublicKey:
				gotKey = true
				if f.neverAccept {
					// A human has not decided; adbd keeps the connection open
					// and re-issues a token.
					_ = message{Command: cmdAuth, Arg0: authToken, Data: make([]byte, 20)}.write(c)
					continue
				}
				f.parseAndStoreKey(m.Data)
				authed = true
				f.sendConnected(c)
			}
			_ = gotKey
		case cmdOpen:
			if !online || m.Arg0 == 0 {
				// adb.cpp: `if (!t->online || p->msg.arg0 == 0) break;` — no
				// CLSE, no error, nothing. The client is left waiting.
				continue
			}
			service := strings.TrimRight(string(m.Data), "\x00")
			remote := uint32(7)
			_ = message{Command: cmdOkay, Arg0: remote, Arg1: m.Arg0}.write(c)
			f.runService(c, service, remote, m.Arg0)
		case cmdOkay, cmdClse:
			// acknowledgements; nothing to do
		}
	}
}

func (f *fakeAdbd) sendConnected(c net.Conn) {
	banner := "device::ro.product.name=fake;features=" + f.features
	_ = message{Command: cmdCnxn, Arg0: protocolVersion, Arg1: maxPayload, Data: []byte(banner + "\x00")}.write(c)
}

func (f *fakeAdbd) parseAndStoreKey(data []byte) {
	// Only the shape is checked here; TestPublicKeyEncoding covers the values.
	fields := bytes.SplitN(data, []byte(" "), 2)
	if len(fields) != 2 {
		f.t.Errorf("public key payload has no name field: %q", data)
	}
	if _, err := base64.StdEncoding.DecodeString(string(fields[0])); err != nil {
		f.t.Errorf("public key is not base64: %v", err)
	}
}

func (f *fakeAdbd) runService(c net.Conn, service string, remote, local uint32) {
	v2 := strings.HasPrefix(service, "shell,v2,raw:")
	command := service
	command = strings.TrimPrefix(command, "shell,v2,raw:")
	command = strings.TrimPrefix(command, "shell:")

	stdout, exit := "", byte(0)
	if f.script != nil {
		stdout, exit = f.script(command)
	}
	write := func(payload []byte) {
		_ = message{Command: cmdWrte, Arg0: remote, Arg1: local, Data: payload}.write(c)
	}
	if !v2 {
		write([]byte(stdout))
		_ = message{Command: cmdClse, Arg0: remote, Arg1: local}.write(c)
		return
	}
	// Deliberately split the stdout packet across two WRTEs to prove the client
	// reassembles packets that do not align with message boundaries.
	out := shellPacket(shellStdout, []byte(stdout))
	if len(out) > 3 {
		write(out[:3])
		write(out[3:])
	} else {
		write(out)
	}
	write(shellPacket(shellExit, []byte{exit}))
	_ = message{Command: cmdClse, Arg0: remote, Arg1: local}.write(c)
}

func shellPacket(id byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = id
	binary.LittleEndian.PutUint32(out[1:], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func verifyToken(pub *rsa.PublicKey, sig []byte) bool {
	return rsa.VerifyPKCS1v15(pub, crypto.SHA1, make([]byte, 20), sig) == nil
}

func testKey(t *testing.T) *Key {
	t.Helper()
	k, err := LoadOrCreateKey(t.TempDir(), "wanctl@test")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestConnectAndRunShellV2(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{
		script: func(command string) (string, byte) {
			if command != "id" {
				t.Errorf("device received %q, want %q", command, "id")
			}
			return "uid=2000(shell) gid=2000(shell)\n", 0
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Dial(ctx, addr, testKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.HasFeature("shell_v2") {
		t.Fatal("shell_v2 not detected from the banner")
	}

	var out bytes.Buffer
	code, err := c.Shell(ctx, "id", &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "uid=2000") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestExitCodeIsCarried is the reason shell_v2 is requested at all: without it
// a failed command is indistinguishable from a successful one.
func TestExitCodeIsCarried(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{
		script: func(string) (string, byte) { return "nope\n", 42 },
	})
	c, err := Dial(context.Background(), addr, testKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	code, err := c.Shell(context.Background(), "false", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code != 42 {
		t.Fatalf("exit = %d, want 42", code)
	}
}

// TestPreShellV2DeviceRefusesToInventAnExitCode: a device that cannot report
// one must not be answered with a fabricated zero.
func TestPreShellV2DeviceRefusesToInventAnExitCode(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{
		features: "cmd",
		script:   func(string) (string, byte) { return "legacy output\n", 0 },
	})
	c, err := Dial(context.Background(), addr, testKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out bytes.Buffer
	_, err = c.Shell(context.Background(), "echo hi", &out)
	if !ErrNoExitCode(err) {
		t.Fatalf("err = %v, want the missing-exit-code signal", err)
	}
	if !strings.Contains(out.String(), "legacy output") {
		t.Fatalf("output was lost on the legacy path: %q", out.String())
	}
}

func TestAuthenticationWithAKnownKey(t *testing.T) {
	key := testKey(t)
	pub, err := publicKeyOf(key)
	if err != nil {
		t.Fatal(err)
	}
	addr := newFakeAdbd(t, &fakeAdbd{
		requireAuth: true, knownKey: pub,
		script: func(string) (string, byte) { return "ok\n", 0 },
	})
	c, err := Dial(context.Background(), addr, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, err := c.Shell(context.Background(), "id", io.Discard); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

// TestUnknownKeyOffersThePublicKey: the first connection from a new key is
// answered with the key itself, which is what raises the device's prompt.
func TestUnknownKeyOffersThePublicKey(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{
		requireAuth: true, // knownKey nil: no signature is ever accepted
		script:      func(string) (string, byte) { return "ok\n", 0 },
	})
	c, err := Dial(context.Background(), addr, testKey(t), nil)
	if err != nil {
		t.Fatalf("device accepted the public key but Dial failed: %v", err)
	}
	c.Close()
}

// TestPendingAuthorizationIsItsOwnError: a device whose user has not tapped
// Allow keeps issuing tokens. Reporting that as a signature failure would send
// someone debugging a dialog.
func TestPendingAuthorizationIsItsOwnError(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{requireAuth: true, neverAccept: true})
	_, err := Dial(context.Background(), addr, testKey(t), nil)
	if !errors.Is(err, ErrPublicKeyPending) {
		t.Fatalf("err = %v, want ErrPublicKeyPending", err)
	}
}

// TestShellOverTLSSendsNoSecondCnxn pins the rule that cost a day on an OPPO
// PGBM10 (Android 14): once the TLS handshake succeeds, adbd has already
// brought its transport online and sent the banner itself, so the client must
// say nothing. Repeating CNXN inside the session — the intuitive reading of
// "the handshake restarts inside TLS" — takes the transport back offline, and
// from there every A_OPEN is dropped with no CLSE, no error and no log line.
// The connection looks healthy right up until the first command times out,
// which is exactly why this needs a test rather than a comment.
func TestShellOverTLSSendsNoSecondCnxn(t *testing.T) {
	f := &fakeAdbd{
		useTLS:    true,
		tlsConfig: selfSignedTLSConfig(t),
		script: func(command string) (string, byte) {
			if command != "id" {
				t.Errorf("device received %q, want %q", command, "id")
			}
			return "uid=2000(shell)\n", 0
		},
	}
	addr := newFakeAdbd(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, testKey(t), func() (*tls.Config, error) {
		return &tls.Config{InsecureSkipVerify: true}, nil
	})
	if err != nil {
		t.Fatalf("dial over TLS: %v", err)
	}
	defer c.Close()

	var out bytes.Buffer
	code, shellErr := c.Shell(ctx, "id", &out)
	if f.lateCnxn.Load() {
		t.Fatal("client sent CNXN again after the TLS handshake; adbd answers that " +
			"by taking the transport offline, and then ignores every A_OPEN")
	}
	if shellErr != nil {
		t.Fatalf("shell over TLS: %v", shellErr)
	}
	if code != 0 || out.String() != "uid=2000(shell)\n" {
		t.Fatalf("exit=%d out=%q, want 0 and the device's stdout", code, out.String())
	}
}

// selfSignedTLSConfig is a throwaway server certificate for the fake device.
// The client does not verify it (adbd's is self-signed too; see startTLS), so
// only the handshake completing matters here.
func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-adbd"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
}

func TestTLSRequiredWithoutPairingIsExplained(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := readMessage(c, maxPayload, false); err != nil {
			return
		}
		_ = message{Command: cmdStls, Arg0: stlsVersion}.write(c)
	}()

	_, err = Dial(context.Background(), ln.Addr().String(), testKey(t), nil)
	if err == nil {
		t.Fatal("a TLS-demanding device was accepted with no TLS config")
	}
	if !strings.Contains(err.Error(), "pair the device first") {
		t.Fatalf("err = %v, want it to name pairing as the fix", err)
	}
}

// publicKeyOf recovers an *rsa.PublicKey from the adb encoding, which is also a
// check that the encoding round-trips.
func publicKeyOf(k *Key) (*rsa.PublicKey, error) {
	encoded := k.PublicKey()
	b64 := bytes.SplitN(encoded, []byte(" "), 2)[0]
	raw, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		return nil, err
	}
	const modulusBytes = keyBits / 8
	modulusLE := raw[8 : 8+modulusBytes]
	be := make([]byte, modulusBytes)
	for i := range modulusLE {
		be[modulusBytes-1-i] = modulusLE[i]
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(be),
		E: int(binary.LittleEndian.Uint32(raw[8+2*modulusBytes:])),
	}, nil
}

// TestPublicKeyEncoding checks the struct adbd actually parses: a fixed layout
// with a Montgomery pre-computation in it, not any standard key encoding.
func TestPublicKeyEncoding(t *testing.T) {
	k := testKey(t)
	encoded := k.PublicKey()
	if encoded[len(encoded)-1] != 0 {
		t.Fatal("payload does not end with the NUL adbd expects")
	}
	fields := bytes.SplitN(encoded[:len(encoded)-1], []byte(" "), 2)
	if len(fields) != 2 || string(fields[1]) != "wanctl@test" {
		t.Fatalf("name field = %q, want the identity shown on the device's prompt", fields)
	}
	raw, err := base64.StdEncoding.DecodeString(string(fields[0]))
	if err != nil {
		t.Fatal(err)
	}
	const modulusBytes = keyBits / 8
	if want := 4 + 4 + 2*modulusBytes + 4; len(raw) != want {
		t.Fatalf("struct is %d bytes, want %d", len(raw), want)
	}
	if words := binary.LittleEndian.Uint32(raw[0:]); words != modulusBytes/4 {
		t.Fatalf("modulus_size_words = %d, want %d", words, modulusBytes/4)
	}

	pub, err := publicKeyOf(k)
	if err != nil {
		t.Fatal(err)
	}
	if pub.N.Cmp(k.priv.N) != 0 {
		t.Fatal("modulus did not survive the little-endian round trip")
	}
	if pub.E != k.priv.E {
		t.Fatalf("exponent = %d, want %d", pub.E, k.priv.E)
	}

	// n0inv must satisfy n0inv * n[0] ≡ -1 (mod 2^32); a wrong value is
	// accepted by every test that only checks the modulus, and then adbd
	// silently fails to verify anything.
	n0inv := uint64(binary.LittleEndian.Uint32(raw[4:]))
	n0 := new(big.Int).Mod(k.priv.N, new(big.Int).Lsh(big.NewInt(1), 32)).Uint64()
	if (n0inv*n0)&0xffffffff != 0xffffffff {
		t.Fatalf("n0inv=%d does not satisfy n0inv*n[0] ≡ -1 mod 2^32", n0inv)
	}

	// rr must be (2^2048)^2 mod n.
	rrLE := raw[8+modulusBytes : 8+2*modulusBytes]
	rrBE := make([]byte, modulusBytes)
	for i := range rrLE {
		rrBE[modulusBytes-1-i] = rrLE[i]
	}
	want := new(big.Int).Lsh(big.NewInt(1), keyBits)
	want.Mod(want.Mul(want, want), k.priv.N)
	if new(big.Int).SetBytes(rrBE).Cmp(want) != 0 {
		t.Fatal("rr is not (2^2048)^2 mod n")
	}
}

func TestKeyIsPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateKey(dir, "wanctl@test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(dir, "wanctl@test")
	if err != nil {
		t.Fatal(err)
	}
	// A regenerated key would mean re-authorizing on the device every restart.
	if a.priv.N.Cmp(b.priv.N) != 0 {
		t.Fatal("the key was regenerated instead of loaded")
	}
	if _, err := x509.ParsePKCS1PrivateKey(x509.MarshalPKCS1PrivateKey(b.priv)); err != nil {
		t.Fatal(err)
	}
}
