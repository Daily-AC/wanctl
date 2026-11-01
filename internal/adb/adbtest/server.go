// Package adbtest provides a local, shell-v2-only fake adbd. It deliberately
// negotiates a small payload and enforces WRTE acknowledgements in both directions.
package adbtest

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

type Handler func(context.Context, string, io.Reader, io.Writer, io.Writer) int
type Server struct {
	Addr  string
	mu    sync.Mutex
	conns map[net.Conn]bool
	wg    sync.WaitGroup
}

func Start(t testing.TB, handler Handler) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), conns: map[net.Conn]bool{}}
	acceptDone := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer close(acceptDone)
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns[c] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c, handler); s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-acceptDone; s.Drop(); s.wg.Wait() })
	return s
}
func (s *Server) Drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}
func (s *Server) serve(c net.Conn, h Handler) {
	defer c.Close()
	m, err := read(c)
	if err != nil || m.cmd != "CNXN" {
		return
	}
	if write(c, packet{"CNXN", 0x01000001, 4096, []byte("device::features=shell_v2\x00")}) != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	packets := make(chan packet)
	go func() {
		defer close(packets)
		for {
			p, err := read(c)
			if err != nil {
				return
			}
			select {
			case packets <- p:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		var ok bool
		m, ok = <-packets
		if !ok {
			return
		}
		if m.cmd == "CLSE" || m.cmd == "OKAY" {
			continue
		}
		if m.cmd != "OPEN" || !strings.HasPrefix(string(m.data), "shell,v2,raw:") {
			return
		}
		local := m.a
		if write(c, packet{"OKAY", 7, local, nil}) != nil {
			return
		}
		if !serveShell(c, packets, local, strings.TrimSuffix(strings.TrimPrefix(string(m.data), "shell,v2,raw:"), "\x00"), h) {
			return
		}
	}
}
func serveShell(c net.Conn, packets <-chan packet, local uint32, command string, h Handler) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, w := io.Pipe()
	defer in.Close()
	defer w.Close()
	input := make(chan []byte, 256)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		defer w.Close()
		for {
			select {
			case b := <-input:
				if b == nil {
					return
				}
				if _, err := w.Write(b); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); in.Close(); <-inputDone }()
	var send sync.Mutex
	sendPacket := func(p packet) error { send.Lock(); defer send.Unlock(); return write(c, p) }
	ack := make(chan struct{}, 1)
	done := make(chan struct{})
	out := &output{send: sendPacket, ack: ack, ctx: ctx, local: local, id: 1}
	stderr := &output{send: sendPacket, ack: ack, ctx: ctx, local: local, id: 2, serial: &out.mu}
	go func() {
		defer close(done)
		code := h(ctx, command, in, out, stderr)
		_ = out.packet(3, []byte{byte(code)})
		_ = sendPacket(packet{"CLSE", 7, local, nil})
	}()
	var pending []byte
	for {
		var m packet
		var ok bool
		select {
		case <-done:
			return true
		case m, ok = <-packets:
		}
		if !ok {
			cancel()
			in.Close()
			<-done
			return false
		}
		if m.b != 7 || m.a != local {
			cancel()
			in.Close()
			<-done
			return false
		}
		switch m.cmd {
		case "OKAY":
			select {
			case ack <- struct{}{}:
			case <-ctx.Done():
			}
		case "WRTE":
			pending = append(pending, m.data...)
			if sendPacket(packet{"OKAY", 7, local, nil}) != nil {
				cancel()
				in.Close()
				<-done
				return false
			}
			for len(pending) >= 5 {
				n := int(binary.LittleEndian.Uint32(pending[1:5]))
				if n > 1<<20 {
					cancel()
					in.Close()
					<-done
					return false
				}
				if len(pending) < n+5 {
					break
				}
				switch pending[0] {
				case 0:
					select {
					case input <- append([]byte{}, pending[5:5+n]...):
					case <-ctx.Done():
					}
				case 4:
					select {
					case input <- nil:
					case <-ctx.Done():
					}
				default:
					cancel()
				}
				pending = pending[5+n:]
			}
		case "CLSE":
			cancel()
			in.Close()
			w.Close()
			<-done
			return true
		default:
			cancel()
			in.Close()
			<-done
			return false
		}
		// Shell() reuses the connection; stream clients close their own socket.
		select {
		case <-done:
			return true
		default:
		}
	}
}

type output struct {
	mu     sync.Mutex
	serial *sync.Mutex
	send   func(packet) error
	ack    <-chan struct{}
	ctx    context.Context
	local  uint32
	id     byte
}

func (o *output) Write(b []byte) (int, error) {
	mu := &o.mu
	if o.serial != nil {
		mu = o.serial
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for len(b) > 0 {
		size := min(len(b), 4000)
		if err := o.packet(o.id, b[:size]); err != nil {
			return n, err
		}
		n += size
		b = b[size:]
	}
	return n, nil
}
func (o *output) packet(id byte, b []byte) error {
	p := make([]byte, len(b)+5)
	p[0] = id
	binary.LittleEndian.PutUint32(p[1:5], uint32(len(b)))
	copy(p[5:], b)
	// Fragment even the five-byte shell header across ADB packets.
	for _, part := range [][]byte{p[:3], p[3:]} {
		if err := o.send(packet{"WRTE", 7, o.local, part}); err != nil {
			return err
		}
		select {
		case <-o.ack:
		case <-o.ctx.Done():
			return o.ctx.Err()
		}
	}
	return nil
}

type packet struct {
	cmd  string
	a, b uint32
	data []byte
}

func read(r io.Reader) (packet, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(r, b); err != nil {
		return packet{}, err
	}
	n := binary.LittleEndian.Uint32(b[12:16])
	if n > 1<<20 {
		return packet{}, fmt.Errorf("oversized ADB packet")
	}
	if binary.LittleEndian.Uint32(b[:4])^binary.LittleEndian.Uint32(b[20:24]) != 0xffffffff {
		return packet{}, fmt.Errorf("bad ADB magic")
	}
	p := packet{string(b[:4]), binary.LittleEndian.Uint32(b[4:8]), binary.LittleEndian.Uint32(b[8:12]), make([]byte, n)}
	_, err := io.ReadFull(r, p.data)
	return p, err
}
func write(w io.Writer, p packet) error {
	b := make([]byte, 24+len(p.data))
	copy(b, p.cmd)
	binary.LittleEndian.PutUint32(b[4:8], p.a)
	binary.LittleEndian.PutUint32(b[8:12], p.b)
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(p.data)))
	binary.LittleEndian.PutUint32(b[20:24], binary.LittleEndian.Uint32(b[:4])^0xffffffff)
	copy(b[24:], p.data)
	_, err := w.Write(b)
	return err
}
