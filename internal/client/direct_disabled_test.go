package client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/direct"
	"wanctl/internal/protocol"
)

func TestDisabledControllerRejectsUnsolicitedDirectInfo(t *testing.T) {
	for _, operation := range []string{"push", "pull"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			t.Setenv("WANCTL_DIRECT", "0")
			var opened atomic.Int32
			controller, device := net.Pipe()
			defer controller.Close()
			defer device.Close()
			c := NewWith(nil, nil, "", "", "ws")
			if c.directEnabled {
				t.Fatal("WANCTL_DIRECT=0 was ignored")
			}
			c.directSettings = direct.Settings{MinBytes: 1, AllowLoopback: true, STUNServers: []string{}, Budget: 100 * time.Millisecond,
				OpenSocket: func(network string, addr *net.UDPAddr) (*net.UDPConn, error) {
					opened.Add(1)
					return net.ListenUDP(network, addr)
				}}
			c.fileConnect = func(context.Context, string) (net.Conn, error) { return controller, nil }
			peerDone := make(chan error, 1)
			payload := bytes.Repeat([]byte("relay-after-unsolicited-offer"), 3000)
			go func() {
				defer device.Close()
				request, err := protocol.ReadMessage(device)
				if err != nil {
					peerDone <- err
					return
				}
				if request.Direct != nil {
					peerDone <- fmt.Errorf("disabled controller advertised direct: %+v", request)
					return
				}
				offer := &protocol.DirectInfo{Candidates: []string{"127.0.0.1:40001"}, CertSHA256: "wrong-pin"}
				if operation == "push" {
					if err := protocol.WriteMessage(device, protocol.Message{Kind: protocol.KindOK, Direct: offer}); err != nil {
						peerDone <- err
						return
					}
				} else {
					if err := protocol.WriteMessage(device, protocol.Message{Kind: protocol.KindFileMeta, Size: int64(len(payload)), Direct: offer}); err != nil {
						peerDone <- err
						return
					}
				}
				signal, err := protocol.ReadMessage(device)
				if err != nil || signal.Kind != protocol.KindDirectFallback {
					peerDone <- fmt.Errorf("selection = %+v, %v; want direct_fallback", signal, err)
					return
				}
				if operation == "push" {
					var got []byte
					for {
						frame, part, err := protocol.ReadFrame(device)
						if err != nil {
							peerDone <- err
							return
						}
						if frame == protocol.FrameData {
							got = append(got, part...)
							continue
						}
						end, err := protocol.DecodeMessage(part)
						if frame != protocol.FrameJSON || err != nil || end.Kind != protocol.KindEOF || !bytes.Equal(got, payload) {
							peerDone <- fmt.Errorf("push data = %d bytes, end = %+v, %v", len(got), end, err)
							return
						}
						break
					}
					peerDone <- protocol.WriteMessage(device, protocol.Message{Kind: protocol.KindOK, Size: int64(len(payload))})
				} else {
					if err := protocol.WriteFrame(device, protocol.FrameData, payload); err != nil {
						peerDone <- err
						return
					}
					peerDone <- protocol.WriteMessage(device, protocol.Message{Kind: protocol.KindEOF})
				}
			}()
			var err error
			if operation == "push" {
				err = c.PushBytes(context.Background(), "device", "remote", payload, 0o600)
			} else {
				local := filepath.Join(t.TempDir(), "local")
				err = c.Pull(context.Background(), "device", "remote", local)
				if err == nil {
					got, readErr := os.ReadFile(local)
					if readErr != nil || !bytes.Equal(got, payload) {
						t.Fatalf("pull bytes = %d %v", len(got), readErr)
					}
				}
			}
			if err != nil {
				t.Fatalf("%s: %v", operation, err)
			}
			if err := <-peerDone; err != nil {
				t.Fatal(err)
			}
			if opened.Load() != 0 {
				t.Fatalf("disabled controller opened %d UDP sockets", opened.Load())
			}
		})
	}
}
