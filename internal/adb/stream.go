package adb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// Stream owns a dedicated Conn. Never open it on the shared Shell connection.
// Its reader handles acknowledgements independently of consumers of stdout and
// stderr: an early diagnostic must not deadlock a pending stdin write.
type Stream struct {
	conn            *Conn
	local, remote   uint32
	sendMu, inputMu sync.Mutex
	inputClosed     bool
	ack             chan struct{}
	done            chan struct{}
	out, errOut     *streamOutput
	exit            int
	err             error
}

func (c *Conn) Stream(ctx context.Context, command string) (*Stream, error) {
	if !c.HasFeature("shell_v2") {
		c.Close()
		return nil, errNoExitCode
	}
	c.mu.Lock()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	fail := func(err error) (*Stream, error) { stop(); c.Close(); c.mu.Unlock(); return nil, err }
	local := c.nextID
	c.nextID++
	if err := c.send(message{Command: cmdOpen, Arg0: local, Data: []byte("shell,v2,raw:" + command + "\x00")}); err != nil {
		return fail(err)
	}
	m, err := c.recv()
	if err != nil {
		return fail(err)
	}
	if m.Command != cmdOkay || m.Arg1 != local || m.Arg0 == 0 {
		return fail(errors.New("adb: shell_v2 stream refused"))
	}
	s := &Stream{conn: c, local: local, remote: m.Arg0, ack: make(chan struct{}, 1), done: make(chan struct{}), out: newStreamOutput(), errOut: newStreamOutput(), exit: -1}
	go func() {
		s.exit, s.err = s.read()
		stop()
		c.Close()
		s.out.finish(s.err)
		s.errOut.finish(s.err)
		c.mu.Unlock()
		close(s.done)
	}()
	return s, nil
}
func (s *Stream) Stdin() io.WriteCloser { return streamInput{s} }
func (s *Stream) Stdout() io.ReadCloser { return s.out }
func (s *Stream) Stderr() io.ReadCloser { return s.errOut }
func (s *Stream) Wait() (int, error)    { <-s.done; return s.exit, s.err }

// Close aborts immediately, including blocked reads/writes. Stdin.Close is the
// graceful half-close; callers can wait for the exit packet after that.
func (s *Stream) Close() error { err := s.conn.Close(); <-s.done; return err }
func (s *Stream) send(m message) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.send(m)
}

type streamInput struct{ s *Stream }

func (w streamInput) Write(p []byte) (int, error) {
	s := w.s
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.inputClosed {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) > 0 {
		size := min(len(p), int(s.conn.limit)-5)
		if size <= 0 {
			return n, errors.New("adb: payload limit too small")
		}
		if err := s.inputPacket(0, p[:size]); err != nil {
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}
func (w streamInput) Close() error {
	s := w.s
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.inputClosed {
		return nil
	}
	s.inputClosed = true
	return s.inputPacket(4, nil) // shell-v2 close-stdin
}
func (s *Stream) inputPacket(id byte, p []byte) error {
	select {
	case <-s.done:
		return io.ErrClosedPipe
	default:
	}
	b := make([]byte, 5+len(p))
	b[0] = id
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(p)))
	copy(b[5:], p)
	if err := s.send(message{Command: cmdWrte, Arg0: s.local, Arg1: s.remote, Data: b}); err != nil {
		s.conn.Close()
		return err
	}
	select {
	case <-s.ack:
		return nil
	case <-s.done:
		return io.ErrClosedPipe
	}
}
func (s *Stream) read() (int, error) {
	var pending []byte
	exit := -1
	for {
		m, err := s.conn.recv()
		if err != nil {
			if errors.Is(err, io.EOF) && exit >= 0 && len(pending) == 0 {
				return exit, nil
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return -1, err
		}
		if m.Arg1 != s.local || m.Arg0 != s.remote {
			return -1, errors.New("adb: unexpected stream identity")
		}
		switch m.Command {
		case cmdOkay:
			select {
			case s.ack <- struct{}{}:
			default:
				return -1, errors.New("adb: unexpected stdin acknowledgement")
			}
		case cmdWrte:
			if err := s.send(message{Command: cmdOkay, Arg0: s.local, Arg1: s.remote}); err != nil {
				return -1, err
			}
			pending = append(pending, m.Data...)
			for len(pending) >= 5 {
				id := pending[0]
				length := binary.LittleEndian.Uint32(pending[1:5])
				if length > maxPayload {
					return -1, errors.New("adb: shell packet exceeds limit")
				}
				size := int(length) // convert only after bounding, including 32-bit Android
				if len(pending) < 5+size {
					break
				}
				p := pending[5 : 5+size]
				switch id {
				case shellStdout:
					err = s.out.append(p)
				case shellStderr:
					err = s.errOut.append(p)
				case shellExit:
					if size != 1 || exit >= 0 {
						return -1, errors.New("adb: invalid exit packet")
					}
					exit = int(p[0])
				default:
					return -1, errors.New("adb: unexpected shell packet")
				}
				if err != nil {
					return -1, err
				}
				pending = pending[5+size:]
			}
		case cmdClse:
			// The socket belongs to this stream; closing it also acknowledges teardown.
			if exit < 0 || len(pending) != 0 {
				return -1, errors.New("adb: stream closed without a complete exit packet")
			}
			return exit, nil
		default:
			return -1, errors.New("adb: unexpected stream packet")
		}
	}
}

// A bounded queue keeps wire acknowledgements moving while a consumer is
// writing stdin. Overflow fails the stream instead of silently dropping bytes.
const maxStreamOutput = 64 << 20

type streamOutput struct {
	mu    sync.Mutex
	ready *sync.Cond
	buf   bytes.Buffer
	err   error
}

func newStreamOutput() *streamOutput { p := &streamOutput{}; p.ready = sync.NewCond(&p.mu); return p }
func (p *streamOutput) append(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if len(b) > maxStreamOutput-p.buf.Len() {
		return errors.New("adb: unread stream output exceeds 64 MiB")
	}
	_, _ = p.buf.Write(b)
	p.ready.Signal()
	return nil
}
func (p *streamOutput) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for p.buf.Len() == 0 && p.err == nil {
		p.ready.Wait()
	}
	if p.buf.Len() > 0 {
		return p.buf.Read(b)
	}
	return 0, p.err
}
func (p *streamOutput) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		if err == nil {
			err = io.EOF
		}
		p.err = err
	}
	p.ready.Broadcast()
}
func (p *streamOutput) Close() error {
	p.mu.Lock()
	p.buf.Reset()
	p.mu.Unlock()
	p.finish(io.ErrClosedPipe)
	return nil
}
