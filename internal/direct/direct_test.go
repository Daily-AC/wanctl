package direct

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestSTUNBindingRoundTrip(t *testing.T) {
	for _, ip := range []netip.Addr{netip.MustParseAddr("203.0.113.4"), netip.MustParseAddr("2001:db8::4")} {
		t.Run(ip.String(), func(t *testing.T) {
			req, tid, err := bindingRequest()
			if err != nil {
				t.Fatal(err)
			}
			if len(req) != 20 || binary.BigEndian.Uint16(req[:2]) != 1 {
				t.Fatalf("request = %x", req)
			}
			answer := bindingResponse(tid, netip.AddrPortFrom(ip, 4321))
			got, responseID, ok := parseBindingResponse(answer)
			if !ok || responseID != tid || got != netip.AddrPortFrom(ip, 4321) {
				t.Fatalf("response = %v, %x, %v", got, responseID, ok)
			}
		})
	}
}

func bindingResponse(tid [12]byte, addr netip.AddrPort) []byte {
	ip := addr.Addr().AsSlice()
	family := byte(1)
	if addr.Addr().Is6() {
		family = 2
	}
	attr := make([]byte, 4+4+len(ip))
	binary.BigEndian.PutUint16(attr[0:2], 0x0020)
	binary.BigEndian.PutUint16(attr[2:4], uint16(4+len(ip)))
	attr[5] = family
	binary.BigEndian.PutUint16(attr[6:8], addr.Port()^uint16(stunCookie>>16))
	key := make([]byte, 16)
	binary.BigEndian.PutUint32(key, stunCookie)
	copy(key[4:], tid[:])
	for i, b := range ip {
		attr[8+i] = b ^ key[i]
	}
	out := make([]byte, 20+len(attr))
	binary.BigEndian.PutUint16(out[0:2], 0x0101)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(attr)))
	binary.BigEndian.PutUint32(out[4:8], stunCookie)
	copy(out[8:20], tid[:])
	copy(out[20:], attr)
	return out
}

func TestSTUNReplyMustMatchTransactionAndSource(t *testing.T) {
	server := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 3478}
	other := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 3478}
	tid := [12]byte{1, 2, 3}
	want := map[[12]byte]netip.AddrPort{tid: netip.MustParseAddrPort("192.0.2.1:3478")}
	answer := bindingResponse(tid, netip.MustParseAddrPort("203.0.113.4:4321"))
	if _, ok := acceptBinding(answer, other, want); ok {
		t.Fatal("accepted wrong source")
	}
	bad := bytes.Clone(answer)
	bad[8]++
	if _, ok := acceptBinding(bad, server, want); ok {
		t.Fatal("accepted unknown transaction")
	}
	if got, ok := acceptBinding(answer, server, want); !ok || got.String() != "203.0.113.4:4321" {
		t.Fatalf("valid reply = %v, %v", got, ok)
	}
}

func TestPeerCandidatesValidation(t *testing.T) {
	valid := []string{"192.168.1.2:80", "[::ffff:192.168.1.3]:81", "[2001:db8::1]:443"}
	got := validateCandidates(valid, false)
	if len(got) != 3 || got[1].String() != "192.168.1.3:81" {
		t.Fatalf("valid = %v", got)
	}
	bad := []string{"127.0.0.1:2", "0.0.0.0:2", "224.0.0.1:2", "255.255.255.255:2", "169.254.1.1:2", "[fe80::1]:2", "192.0.2.1:0", "192.0.2.1:65536", "host:4"}
	for _, candidate := range bad {
		if got := validateCandidates([]string{candidate}, false); len(got) != 0 {
			t.Errorf("accepted %q: %v", candidate, got)
		}
	}
	if got := validateCandidates(append(valid, bad[0]), false); len(got) != 0 {
		t.Fatalf("partially accepted invalid list: %v", got)
	}
	tooMany := make([]string, 9)
	for i := range tooMany {
		tooMany[i] = "192.0.2.1:4"
	}
	if got := validateCandidates(tooMany, false); len(got) != 0 {
		t.Fatal("accepted nine candidates")
	}
	if got := validateCandidates([]string{"[2001:db8::123456789012345678901234567890123456789012345678901234567890]:4"}, false); len(got) != 0 {
		t.Fatal("accepted overlong candidate")
	}
}

func TestOwnCandidateOrderingAndCap(t *testing.T) {
	defaults := []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("fd00::2")}
	reflexive := []netip.AddrPort{netip.MustParseAddrPort("203.0.113.1:5"), netip.MustParseAddrPort("203.0.113.2:5"), netip.MustParseAddrPort("203.0.113.3:5"), netip.MustParseAddrPort("[2001:db8::1]:5"), netip.MustParseAddrPort("[2001:db8::2]:5")}
	hosts := []hostCandidate{{addr: netip.MustParseAddr("172.16.0.2")}, {addr: netip.MustParseAddr("198.51.100.2")}, {addr: netip.MustParseAddr("192.168.2.2"), pointToPoint: true}, {addr: netip.MustParseAddr("10.0.0.3")}, {addr: netip.MustParseAddr("10.0.0.4")}, {addr: netip.MustParseAddr("10.0.0.5")}}
	got := orderCandidates(4123, defaults, reflexive, hosts, false)
	want := []string{"10.0.0.2:4123", "[fd00::2]:4123", "203.0.113.1:5", "203.0.113.2:5", "[2001:db8::1]:5", "172.16.0.2:4123", "10.0.0.3:4123", "10.0.0.4:4123"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestPinnedQUICHandshake(t *testing.T) {
	device, devicePin, err := newCertificate()
	if err != nil {
		t.Fatal(err)
	}
	controller, controllerPin, err := newCertificate()
	if err != nil {
		t.Fatal(err)
	}
	wrong, wrongPin, err := newCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyTLSConfig(device, wrongPin, true).VerifyConnection(tls.ConnectionState{PeerCertificates: nil, NegotiatedProtocol: directALPN}); err == nil {
		t.Fatal("accepted missing client certificate")
	}
	if err := verifyPeer(tls.ConnectionState{PeerCertificates: []*x509.Certificate{controller.Leaf}, NegotiatedProtocol: "wrong"}, controllerPin); err == nil {
		t.Fatal("accepted wrong ALPN")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: udp}
	defer tr.Close()
	defer udp.Close()
	ln, err := tr.Listen(verifyTLSConfig(device, controllerPin, true), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientTR := &quic.Transport{Conn: clientUDP}
	defer clientTR.Close()
	defer clientUDP.Close()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp.LocalAddr().(*net.UDPAddr).Port}
	if conn, err := clientTR.Dial(ctx, addr, verifyTLSConfig(wrong, devicePin, false), quicConfig()); err == nil {
		defer conn.CloseWithError(0, "")
	}
	noAcceptCtx, stopNoAccept := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stopNoAccept()
	if conn, err := ln.Accept(noAcceptCtx); err == nil {
		conn.CloseWithError(0, "")
		t.Fatal("listener accepted wrong client certificate")
	}
	if conn, err := clientTR.Dial(ctx, addr, verifyTLSConfig(controller, wrongPin, false), quicConfig()); err == nil {
		conn.CloseWithError(0, "")
		t.Fatal("accepted wrong server certificate")
	}
	wrongALPN := verifyTLSConfig(controller, devicePin, false)
	wrongALPN.NextProtos = []string{"wrong"}
	if conn, err := clientTR.Dial(ctx, addr, wrongALPN, quicConfig()); err == nil {
		conn.CloseWithError(0, "")
		t.Fatal("accepted wrong ALPN")
	}
	conn, err := clientTR.Dial(ctx, addr, verifyTLSConfig(controller, devicePin, false), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	accepted, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.CloseWithError(0, "")
	first, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := accepted.AcceptStream(ctx); err != nil {
		t.Fatal(err)
	}
	streamCtx, stopStreams := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopStreams()
	if stream, err := conn.OpenStreamSync(streamCtx); err == nil {
		stream.CancelWrite(0)
		t.Fatal("second bidirectional stream opened")
	}
	if stream, err := conn.OpenUniStreamSync(streamCtx); err == nil {
		stream.CancelWrite(0)
		t.Fatal("unidirectional stream opened")
	}
	if quicConfig().Allow0RTT || quicConfig().MaxIncomingUniStreams != -1 || quicConfig().MaxIncomingStreams != 1 {
		t.Fatal("stream limits or 0-RTT changed")
	}
}
