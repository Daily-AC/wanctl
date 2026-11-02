package adb

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"wanctl/internal/adb/adbtest"
)

func TestStreamRoundTripEOFExitAndSeparateOutput(t *testing.T) {
	payload := strings.Repeat("stdin\x00输入", 2000)
	server := adbtest.Start(t, func(_ context.Context, cmd string, in io.Reader, out, stderr io.Writer) int {
		if cmd != "engine" {
			t.Errorf("command %q", cmd)
		}
		// A diagnostic precedes stdin consumption and its acknowledgement.
		io.WriteString(stderr, "first diagnostic\n")
		b, _ := io.ReadAll(in)
		if string(b) != payload {
			t.Errorf("stdin got %d bytes", len(b))
		}
		out.Write(b)
		return 42
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, server.Addr, testKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Stream(ctx, "engine")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = io.WriteString(s.Stdin(), payload); err != nil {
		t.Fatal(err)
	}
	if err = s.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(s.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(s.Stderr())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte(payload)) || string(stderr) != "first diagnostic\n" {
		t.Fatalf("outputs %d / %q", len(out), stderr)
	}
	if code, err := s.Wait(); err != nil || code != 42 {
		t.Fatalf("exit %d %v", code, err)
	}
	if _, err = s.Stdin().Write([]byte("late")); err == nil {
		t.Fatal("write after EOF")
	}
}
func TestStreamDropAndCancellationAreErrors(t *testing.T) {
	for _, drop := range []bool{true, false} {
		t.Run(map[bool]string{true: "drop", false: "cancel"}[drop], func(t *testing.T) {
			started := make(chan struct{})
			server := adbtest.Start(t, func(ctx context.Context, _ string, in io.Reader, out, stderr io.Writer) int {
				close(started)
				io.Copy(io.Discard, in)
				return 0
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, err := Dial(ctx, server.Addr, testKey(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			s, err := c.Stream(ctx, "engine")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			<-started
			if drop {
				server.Drop()
			} else {
				cancel()
			}
			if _, err = io.ReadAll(s.Stdout()); err == nil {
				t.Fatal("dropped stream reported EOF success")
			}
			if code, err := s.Wait(); err == nil || code != -1 {
				t.Fatalf("exit %d %v", code, err)
			}
		})
	}
}
func TestStreamRequiresShellV2(t *testing.T) {
	addr := newFakeAdbd(t, &fakeAdbd{features: "cmd"})
	c, err := Dial(context.Background(), addr, testKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Stream(context.Background(), "engine"); !ErrNoExitCode(err) {
		t.Fatal(err)
	}
}

func TestDialCancellationInterruptsSilentHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		close(accepted)
		io.Copy(io.Discard, c)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey(t)
	done := make(chan error, 1)
	go func() { _, err := Dial(ctx, ln.Addr().String(), key, nil); done <- err }()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake ignored cancellation")
	}
	<-exited
}

func TestStreamRejectsOversizedPacketBeforeIntConversion(t *testing.T) {
	client, remote := net.Pipe()
	defer remote.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remote.Close()
		m, err := readMessage(remote, maxPayload, false)
		if err != nil {
			return
		}
		message{Command: cmdOkay, Arg0: 7, Arg1: m.Arg0}.write(remote)
		message{Command: cmdWrte, Arg0: 7, Arg1: m.Arg0, Data: []byte{shellStdout, 255, 255, 255, 255}}.write(remote)
		readMessage(remote, maxPayload, false)
	}()
	conn := &Conn{c: client, features: map[string]bool{"shell_v2": true}, limit: maxPayload, nextID: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := conn.Stream(ctx, "engine")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Wait(); err == nil || !strings.Contains(err.Error(), "packet exceeds limit") {
		t.Fatal(err)
	}
	<-done
}
