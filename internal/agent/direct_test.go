package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/direct"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
)

func TestDirectStalledAttachReleasesSlot(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	settings := direct.Settings{MinBytes: 1, STUNServers: []string{}, AllowLoopback: true, Hold: time.Second, NoProgress: 150 * time.Millisecond}.Effective()
	a.directSettings = settings
	dr := connectController(t, base)
	defer dr.Conn.Close()
	dir := t.TempDir()
	remote := filepath.Join(dir, "stalled.bin")
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 1 << 20, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(dr.Conn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("offer = %+v %v", ack, err)
	}
	ep, err := direct.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindDirectOffer, Direct: ep.Info()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := ep.Dial(ctx, ack.Direct)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(stream, protocol.Message{Kind: protocol.KindDirectAttach}); err != nil {
		t.Fatal(err)
	}
	confirmed, err := protocol.ReadMessage(stream)
	if err != nil || confirmed.Kind != protocol.KindOK {
		t.Fatalf("attach = %+v %v", confirmed, err)
	}
	if got := a.directActive.Load(); got != 1 {
		t.Fatalf("active after attach = %d", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.directActive.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stalled transfer retained %d slots", a.directActive.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(remote); !os.IsNotExist(err) {
		t.Fatalf("stalled upload committed: %v", err)
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".wanctl-upload-*")); len(temps) != 0 {
		t.Fatalf("stalled upload temps = %v", temps)
	}
}

func TestDelegatedSessionNeverOffersDirect(t *testing.T) {
	f := startDelegationFixture(t, "ws", "ws", policy.ModeBypass, true, time.Minute)
	var opened atomic.Int32
	f.a.directSettings = direct.Settings{MinBytes: 1, STUNServers: []string{}, AllowLoopback: true,
		OpenSocket: func(network string, addr *net.UDPAddr) (*net.UDPConn, error) {
			opened.Add(1)
			return net.ListenUDP(network, addr)
		}}.Effective()
	payload := bytes.Repeat([]byte("delegated"), (8<<20)/9+1)
	remote := filepath.Join(t.TempDir(), "shared.bin")
	if err := f.c.PushBytes(f.ctx, f.target, remote, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(t.TempDir(), "back.bin")
	if err := f.c.Pull(f.ctx, f.target, remote, back); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(back); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("delegated bytes = %d %v", len(got), err)
	}
	if opened.Load() != 0 {
		t.Fatalf("delegated session opened %d direct sockets", opened.Load())
	}
}

func TestDirectCompletedDataFramesKeepTransferAlive(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	settings := direct.Settings{MinBytes: 1, STUNServers: []string{}, AllowLoopback: true, Hold: time.Second, NoProgress: 350 * time.Millisecond}.Effective()
	a.directSettings = settings
	dr := connectController(t, base)
	defer dr.Conn.Close()
	remote := filepath.Join(t.TempDir(), "slow.bin")
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 100, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(dr.Conn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("offer = %+v %v", ack, err)
	}
	ep, err := direct.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindDirectOffer, Direct: ep.Info()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := ep.Dial(ctx, ack.Direct)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(stream, protocol.Message{Kind: protocol.KindDirectAttach}); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := protocol.ReadMessage(stream); err != nil || confirmed.Kind != protocol.KindOK {
		t.Fatalf("attach = %+v %v", confirmed, err)
	}
	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond)
		if err := protocol.WriteFrame(stream, protocol.FrameData, bytes.Repeat([]byte{'x'}, 25)); err != nil {
			t.Fatalf("slow file bytes %d: %v", i, err)
		}
	}
	if err := protocol.WriteMessage(stream, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	result, err := protocol.ReadMessage(stream)
	if err != nil || result.Kind != protocol.KindOK {
		t.Fatalf("slow transfer = %+v %v", result, err)
	}
	if got, err := os.ReadFile(remote); err != nil || len(got) != 100 {
		t.Fatalf("slow file = %d %v", len(got), err)
	}
}

func TestDirectTrickledControlBytesDoNotResetNoProgress(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	settings := direct.Settings{MinBytes: 1, STUNServers: []string{}, AllowLoopback: true, Hold: time.Second, NoProgress: 250 * time.Millisecond}.Effective()
	a.directSettings = settings
	dr := connectController(t, base)
	defer dr.Conn.Close()
	remote := filepath.Join(t.TempDir(), "trickled-control.bin")
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 1 << 20, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(dr.Conn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("offer = %+v %v", ack, err)
	}
	ep, err := direct.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindDirectOffer, Direct: ep.Info()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := ep.Dial(ctx, ack.Direct)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(stream, protocol.Message{Kind: protocol.KindDirectAttach}); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := protocol.ReadMessage(stream); err != nil || confirmed.Kind != protocol.KindOK {
		t.Fatalf("attach = %+v %v", confirmed, err)
	}
	var header [5]byte
	header[0] = byte(protocol.FrameJSON)
	binary.BigEndian.PutUint32(header[1:], 10000)
	if _, err := stream.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(75 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := stream.Write([]byte{'x'}); err != nil {
					return
				}
			}
		}
	}()
	select {
	case <-conn.Context().Done():
		if elapsed := time.Since(started); elapsed > 600*time.Millisecond {
			t.Fatalf("control trickle delayed no-progress timeout to %v", elapsed)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("control bytes kept transfer slot alive")
	}
	deadline := time.Now().Add(time.Second)
	for a.directActive.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("slot not released: %d", a.directActive.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
