package adb

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

// fakePairingServer plays adbd's side of the pairing exchange, using this
// package's own primitives in the Bob role. It proves the framing, the
// TLS-bound password and the AES layer all agree with themselves; only a real
// device can prove they agree with AOSP.
type fakePairingServer struct {
	code string
	// gotPeerInfo receives the decrypted PeerInfo the client sent.
	gotPeerInfo chan []byte
	// err receives whatever went wrong on the server side.
	err chan error
}

func startFakePairingServer(t *testing.T, code string) (addr string, srv *fakePairingServer) {
	t.Helper()
	cert, err := selfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv = &fakePairingServer{code: code, gotPeerInfo: make(chan []byte, 1), err: make(chan error, 1)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := srv.serve(conn.(*tls.Conn)); err != nil {
			srv.err <- err
		}
	}()
	return ln.Addr().String(), srv
}

func (s *fakePairingServer) serve(conn *tls.Conn) error {
	if err := conn.Handshake(); err != nil {
		return err
	}
	state := conn.ConnectionState()
	material, err := state.ExportKeyingMaterial(exportLabel, nil, exportedKeySize)
	if err != nil {
		return err
	}
	password := append([]byte(s.code), material...)

	sp := newSPAKE2(spake2Bob, "adb pair server", "adb pair client")
	myMsg, err := sp.generateMsg(password)
	if err != nil {
		return err
	}
	typ, theirMsg, err := readPairingPacket(conn)
	if err != nil {
		return err
	}
	if typ != pairingSPAKE2Msg {
		return errType(typ)
	}
	if err := writePairingPacket(conn, pairingSPAKE2Msg, myMsg); err != nil {
		return err
	}
	keyMaterial, err := sp.processMsg(theirMsg)
	if err != nil {
		return err
	}
	cipher, err := newPairingCipher(keyMaterial)
	if err != nil {
		return err
	}

	typ, sealed, err := readPairingPacket(conn)
	if err != nil {
		return err
	}
	if typ != pairingPeerInfo {
		return errType(typ)
	}
	peerInfo, err := cipher.open(sealed)
	if err != nil {
		return err
	}
	s.gotPeerInfo <- peerInfo

	mine, err := cipher.seal(make([]byte, peerInfoSize))
	if err != nil {
		return err
	}
	return writePairingPacket(conn, pairingPeerInfo, mine)
}

type errType byte

func (e errType) Error() string { return "unexpected packet type" }

func TestPairingRoundTrip(t *testing.T) {
	addr, srv := startFakePairingServer(t, "123456")
	key := testKey(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Pair(ctx, addr, key, "123456"); err != nil {
		t.Fatalf("pairing failed: %v", err)
	}

	select {
	case info := <-srv.gotPeerInfo:
		if len(info) != peerInfoSize {
			t.Fatalf("PeerInfo is %d bytes, want the fixed %d adbd reads", len(info), peerInfoSize)
		}
		if info[0] != adbRSAPublicKey {
			t.Fatalf("PeerInfo type = %d, want ADB_RSA_PUB_KEY (%d)", info[0], adbRSAPublicKey)
		}
		// The device must receive the very key the agent will authenticate
		// with later; anything else pairs successfully and then cannot connect.
		want := key.PublicKey()
		if !bytes.Equal(info[1:1+len(want)], want) {
			t.Fatal("the public key in PeerInfo is not the agent's adb key")
		}
	case err := <-srv.err:
		t.Fatalf("server: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never received PeerInfo")
	}
}

// The wrong code must fail, and must fail in a way that says so.
func TestPairingWithTheWrongCodeFails(t *testing.T) {
	addr, _ := startFakePairingServer(t, "123456")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Pair(ctx, addr, testKey(t), "999999")
	if err == nil {
		t.Fatal("pairing succeeded with the wrong code")
	}
	if !strings.Contains(err.Error(), "pairing code") {
		t.Fatalf("error = %q, want it to name the pairing code as the likely cause", err)
	}
}

func TestPairingPacketFraming(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello")
	if err := writePairingPacket(&buf, pairingSPAKE2Msg, payload); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if len(raw) != pairingHeaderSize+len(payload) {
		t.Fatalf("framed length = %d, want %d", len(raw), pairingHeaderSize+len(payload))
	}
	if raw[0] != pairingHeaderVersion || raw[1] != pairingSPAKE2Msg {
		t.Fatalf("header = %x", raw[:2])
	}
	// Big-endian length, unlike the SPAKE2 transcript's little-endian prefixes.
	if want := []byte{0, 0, 0, 5}; !bytes.Equal(raw[2:6], want) {
		t.Fatalf("length field = %x, want big-endian %x", raw[2:6], want)
	}

	typ, got, err := readPairingPacket(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if typ != pairingSPAKE2Msg || !bytes.Equal(got, payload) {
		t.Fatalf("round trip = (%d, %q)", typ, got)
	}
}

func TestPairingPacketRejectsAnOversizedPayload(t *testing.T) {
	hdr := []byte{pairingHeaderVersion, pairingSPAKE2Msg, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := readPairingPacket(bytes.NewReader(hdr)); err == nil {
		t.Fatal("a 4GB payload length was accepted")
	}
}

func TestPairingPacketRejectsAnUnknownVersion(t *testing.T) {
	hdr := []byte{99, pairingSPAKE2Msg, 0, 0, 0, 0}
	if _, _, err := readPairingPacket(bytes.NewReader(hdr)); err == nil {
		t.Fatal("an unknown header version was accepted")
	}
}

// The nonce is a per-direction counter starting at zero. Reusing one under the
// same key would be a critical AES-GCM failure, so the counters must advance
// independently.
func TestPairingCipherNoncesAdvancePerDirection(t *testing.T) {
	c, err := newPairingCipher(bytes.Repeat([]byte{7}, 64))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.nonce(0); !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatalf("first nonce = %x, want all zeroes", got)
	}
	a, _ := c.seal([]byte("one"))
	b, _ := c.seal([]byte("one"))
	if bytes.Equal(a, b) {
		t.Fatal("the same plaintext sealed identically twice; the nonce did not advance")
	}
	if c.encSeq != 2 {
		t.Fatalf("encSeq = %d, want 2", c.encSeq)
	}
	if c.decSeq != 0 {
		t.Fatalf("decSeq = %d, want 0 — the directions must count separately", c.decSeq)
	}
}

func TestPairingCipherRoundTrip(t *testing.T) {
	material := bytes.Repeat([]byte{3}, 64)
	enc, err := newPairingCipher(material)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := newPairingCipher(material)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("peer info")
	sealed, err := enc.seal(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dec.open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("round trip = %q, want %q", got, msg)
	}
	// A different SPAKE2 output must not decrypt it.
	other, _ := newPairingCipher(bytes.Repeat([]byte{4}, 64))
	if _, err := other.open(sealed); err == nil {
		t.Fatal("ciphertext decrypted under a different key")
	}
}

// The exporter label carries a trailing NUL because AOSP passes sizeof() as the
// length. Dropping it yields valid-looking material that never matches the
// device, so pin it here.
func TestExporterLabelKeepsItsTrailingNUL(t *testing.T) {
	if exportLabel != "adb-label\x00" {
		t.Fatalf("exporter label = %q, want \"adb-label\\x00\"", exportLabel)
	}
	if len(exportLabel) != 10 {
		t.Fatalf("exporter label is %d bytes, want 10 (sizeof(\"adb-label\"))", len(exportLabel))
	}
}

func TestPairingDialFailureIsReported(t *testing.T) {
	// A closed port, to be sure the error names the address rather than
	// surfacing as something cryptographic.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = Pair(ctx, addr, testKey(t), "123456")
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err = %v, want a dial failure naming the address", err)
	}
}
