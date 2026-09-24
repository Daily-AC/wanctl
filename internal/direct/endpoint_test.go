package direct

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"wanctl/internal/protocol"
)

func TestLoopbackEndpointDialsAndAccepts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opened := 0
	settings := Settings{AllowLoopback: true, STUNServers: []string{}, OpenSocket: func(network string, addr *net.UDPAddr) (*net.UDPConn, error) {
		opened++
		if network == "udp4" && !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
			t.Fatalf("non-loopback bind: %v", addr)
		}
		if network == "udp6" && !addr.IP.Equal(net.IPv6loopback) {
			t.Fatalf("non-loopback bind: %v", addr)
		}
		return net.ListenUDP(network, addr)
	}}
	device, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if opened < 2 || len(device.Info().Candidates) == 0 || len(controller.Info().Candidates) == 0 {
		t.Fatalf("opened=%d device=%+v controller=%+v", opened, device.Info(), controller.Info())
	}
	accepts, err := device.Listen(ctx, controller.Info())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := controller.Dial(ctx, device.Info())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	select {
	case result := <-accepts:
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if result.Conn == nil {
			t.Fatal("no accepted connection")
		}
		defer result.Conn.CloseWithError(0, "")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSocketFailureStopsGathering(t *testing.T) {
	opened := 0
	_, err := Open(context.Background(), Settings{AllowLoopback: true, STUNServers: []string{}, OpenSocket: func(string, *net.UDPAddr) (*net.UDPConn, error) { opened++; return nil, net.ErrClosed }})
	if err == nil || opened != 1 {
		t.Fatalf("open failure = %v, sockets=%d", err, opened)
	}
}

func TestSTUNGatherUsesSharedLoopbackSocket(t *testing.T) {
	for _, tc := range []struct {
		network  string
		listenIP net.IP
		mapped   netip.AddrPort
	}{
		{"udp4", net.IPv4(127, 0, 0, 1), netip.MustParseAddrPort("203.0.113.4:4321")},
		{"udp6", net.IPv6loopback, netip.MustParseAddrPort("[2001:db8::4]:4321")},
	} {
		t.Run(tc.network, func(t *testing.T) {
			server, err := net.ListenUDP(tc.network, &net.UDPAddr{IP: tc.listenIP})
			if err != nil {
				t.Skipf("loopback family unavailable: %v", err)
			}
			defer server.Close()
			received := make(chan bool, 1)
			go func() {
				var request [64]byte
				n, from, err := server.ReadFromUDP(request[:])
				received <- err == nil && n == 20
				if err != nil || n != 20 {
					return
				}
				var tid [12]byte
				copy(tid[:], request[8:20])
				server.WriteToUDP(bindingResponse(tid, tc.mapped), from)
			}()
			ep, err := Open(context.Background(), Settings{AllowLoopback: true, STUNServers: []string{server.LocalAddr().String()}})
			if err != nil {
				t.Fatal(err)
			}
			defer ep.Close()
			select {
			case ok := <-received:
				if !ok {
					t.Fatal("invalid STUN request")
				}
			default:
				t.Fatal("STUN request not sent")
			}
			if !bytes.Contains([]byte(strings.Join(ep.Info().Candidates, ",")), []byte(tc.mapped.String())) {
				t.Fatalf("STUN mapping missing: %+v", ep.Info())
			}
		})
	}
}

func TestSameFamilyCandidateDialsCanReachSelection(t *testing.T) {
	settings := Settings{AllowLoopback: true, STUNServers: []string{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	device, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	accepts, err := device.Listen(ctx, controller.Info())
	if err != nil {
		t.Fatal(err)
	}
	peer := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(device.Info().Candidates[0]))
	var accepted []*quic.Conn
	for i := 0; i < 2; i++ {
		conn, err := controller.sockets[0].tr.Dial(ctx, peer, verifyTLSConfig(controller.cert, device.Info().CertSHA256, false), quicConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		select {
		case result := <-accepts:
			if result.Err != nil || result.Conn == nil {
				t.Fatalf("accept %d = %+v", i, result)
			}
			defer result.Conn.CloseWithError(0, "")
			accepted = append(accepted, result.Conn)
		case <-ctx.Done():
			t.Fatalf("candidate dial %d could not be selected: %v", i, ctx.Err())
		}
	}
	device.CloseListenersExcept(accepted[1])
	select {
	case <-accepted[0].Context().Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("losing connection remained open")
	}
	select {
	case <-accepted[1].Context().Done():
		t.Fatal("selected connection was closed")
	default:
	}
}

// The relay offer can arrive after the controller has already sent Initials.
// This loopback shim behaves like a closed inbound NAT mapping until the device
// sends its first punch packet to the controller's apparent address. It also
// loses the first two post-open Initials so local quic-go's short PTO cannot
// hide the real-link failure.
func TestProbeTriggeredDialBeatsInitialPTO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	settings := Settings{AllowLoopback: true, STUNServers: []string{}}
	device, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	shim, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	deviceAddr := device.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	controllerAddr := controller.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	gate := make(chan time.Duration, 1)
	started := time.Now()
	go func() {
		var buf [2048]byte
		open := false
		postOpenInitialDrops := 2
		for {
			n, from, err := shim.ReadFromUDP(buf[:])
			if err != nil {
				return
			}
			switch from.Port {
			case deviceAddr.Port:
				if !open {
					open = true
					gate <- time.Since(started)
				}
				_, _ = shim.WriteToUDP(buf[:n], controllerAddr)
			case controllerAddr.Port:
				if !open {
					continue
				}
				// A freshly opened mapping can still lose Initials.
				if postOpenInitialDrops > 0 && n > 0 && buf[0]&0xc0 == 0xc0 {
					postOpenInitialDrops--
					continue
				}
				_, _ = shim.WriteToUDP(buf[:n], deviceAddr)
			}
		}
	}()
	shimAddr := shim.LocalAddr().String()
	deviceOffer := &protocol.DirectInfo{Candidates: []string{shimAddr}, CertSHA256: device.Info().CertSHA256}
	controllerOffer := &protocol.DirectInfo{Candidates: []string{shimAddr}, CertSHA256: controller.Info().CertSHA256}
	type listenResult struct {
		accepts <-chan AcceptResult
		err     error
	}
	listenResults := make(chan listenResult, 1)
	go func() {
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		accepts, err := device.Listen(ctx, controllerOffer)
		listenResults <- listenResult{accepts, err}
	}()
	conn, err := controller.Dial(ctx, deviceOffer)
	if err != nil {
		t.Fatalf("dial missed 2.5 s budget (NAT opened at %v): %v", receiveGate(gate), err)
	}
	defer conn.CloseWithError(0, "")
	elapsed := time.Since(started)
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("dial took %v, beyond budget", elapsed)
	}
	result := <-listenResults
	if result.err != nil {
		t.Fatal(result.err)
	}
	select {
	case accepted := <-result.accepts:
		if accepted.Conn == nil || accepted.Err != nil {
			t.Fatalf("listener result: %+v", accepted)
		}
		defer accepted.Conn.CloseWithError(0, "")
	case <-ctx.Done():
		t.Fatalf("listener never accepted successful dial: %v", ctx.Err())
	}
	t.Logf("NAT opened at %v; handshake at %v", receiveGate(gate), elapsed)
}

func receiveGate(ch <-chan time.Duration) time.Duration {
	select {
	case d := <-ch:
		return d
	default:
		return 0
	}
}

func TestProbeFromPeerReflexiveAddressStartsDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	settings := Settings{AllowLoopback: true, STUNServers: []string{}}
	device, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	initial, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close() // initial candidate blackholes all traffic
	mapped, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	deviceAddr := device.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	controllerAddr := controller.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	go func() {
		var buf [2048]byte
		for {
			n, from, err := mapped.ReadFromUDP(buf[:])
			if err != nil {
				return
			}
			if from.Port == deviceAddr.Port {
				_, _ = mapped.WriteToUDP(buf[:n], controllerAddr)
			}
			if from.Port == controllerAddr.Port {
				_, _ = mapped.WriteToUDP(buf[:n], deviceAddr)
			}
		}
	}()
	deviceOffer := &protocol.DirectInfo{Candidates: []string{initial.LocalAddr().String()}, CertSHA256: device.Info().CertSHA256}
	controllerOffer := &protocol.DirectInfo{Candidates: []string{mapped.LocalAddr().String()}, CertSHA256: controller.Info().CertSHA256}
	accepts, err := device.Listen(ctx, controllerOffer)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := controller.Dial(ctx, deviceOffer)
	if err != nil {
		t.Fatalf("peer-reflexive probe did not start a dial: %v", err)
	}
	defer conn.CloseWithError(0, "")
	select {
	case result := <-accepts:
		if result.Conn == nil || result.Err != nil {
			t.Fatalf("listener = %+v", result)
		}
		defer result.Conn.CloseWithError(0, "")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDialIgnoresSTUNPackets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	settings := Settings{AllowLoopback: true, STUNServers: []string{}}
	device, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	listener, err := device.sockets[0].tr.Listen(verifyTLSConfig(device.cert, controller.Info().CertSHA256, true), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	var fake [64]byte
	binary.BigEndian.PutUint32(fake[4:8], stunCookie)
	controllerAddr := controller.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = device.sockets[0].tr.WriteTo(fake[:], controllerAddr)
	}()
	offer := &protocol.DirectInfo{Candidates: []string{blackhole.LocalAddr().String()}, CertSHA256: device.Info().CertSHA256}
	if conn, err := controller.Dial(ctx, offer); err == nil {
		conn.CloseWithError(0, "")
		t.Fatal("STUN packet started a QUIC dial")
	}
}

func TestProbeTriggeredDialsAreCappedAtEight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	controller, err := Open(ctx, Settings{AllowLoopback: true, STUNServers: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	var sources []*net.UDPConn
	defer func() {
		for _, source := range sources {
			source.Close()
		}
	}()
	for i := 0; i < 9; i++ {
		source, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	done := make(chan error, 1)
	go func() {
		_, err := controller.Dial(ctx, &protocol.DirectInfo{Candidates: []string{blackhole.LocalAddr().String()}, CertSHA256: "unused"})
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	to := controller.sockets[0].udp.LocalAddr().(*net.UDPAddr)
	var probe [64]byte
	for _, source := range sources {
		if _, err := source.WriteToUDP(probe[:], to); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err == nil {
		t.Fatal("unexpected handshake")
	}
	started := 0
	for _, source := range sources {
		_ = source.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		var packet [1500]byte
		if n, _, err := source.ReadFromUDP(packet[:]); err == nil && n >= 1200 {
			started++
		}
	}
	if started != 8 {
		t.Fatalf("extra dials = %d, want 8", started)
	}
}
