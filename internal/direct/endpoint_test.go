package direct

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
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
