package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sync"

	"wanctl/internal/protocol"
)

// WorkspaceLink belongs to one conversation. It serializes framed requests
// on one authenticated connection; it never retries a possibly delivered
// mutation. Reconnects keep the explicit remote workspace reference.
type WorkspaceLink struct {
	gate       chan struct{}
	mu         sync.Mutex
	conn       *tls.Conn
	key        [32]byte
	closed     bool
	generation uint64
}

func NewWorkspaceLink() *WorkspaceLink                 { return &WorkspaceLink{gate: make(chan struct{}, 1)} }
func (c *Client) UseWorkspaceLink(link *WorkspaceLink) { c.workspaceLink = link }

func (l *WorkspaceLink) Drop() {
	l.mu.Lock()
	l.generation++
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (l *WorkspaceLink) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.Drop()
}

type workspaceExchangeError struct{ cause error }

func (e *workspaceExchangeError) Error() string { return e.cause.Error() }
func (e *workspaceExchangeError) Unwrap() error { return e.cause }

func (l *WorkspaceLink) exchange(ctx context.Context, c *Client, ref WorkspaceRef, req protocol.Message) (protocol.Message, error) {
	select {
	case l.gate <- struct{}{}:
	case <-ctx.Done():
		return protocol.Message{}, ctx.Err()
	}
	defer func() { <-l.gate }()
	if err := ctx.Err(); err != nil {
		return protocol.Message{}, err
	}
	conn, err := l.connFor(ctx, c, ref)
	if err != nil {
		return protocol.Message{}, err
	}
	// Join a firing cancellation before another request can reuse this conn.
	// Merely stopping the callback can let request A close request B's socket.
	done := make(chan struct{})
	// The transport, not the TLS conn: closing that first waits up to five
	// seconds to send close_notify to a peer that may be the thing stalled.
	stop := context.AfterFunc(ctx, func() { conn.NetConn().Close(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	failed := func(err error) (protocol.Message, error) {
		l.mu.Lock()
		if l.conn == conn {
			l.conn = nil
		}
		l.mu.Unlock()
		conn.Close()
		return protocol.Message{}, &workspaceExchangeError{err}
	}
	if err := protocol.WriteMessage(conn, req); err != nil {
		return failed(err)
	}
	res, err := protocol.ReadMessage(conn)
	if err != nil {
		return failed(err)
	}
	if res.Kind == protocol.KindReject || ctx.Err() != nil {
		l.mu.Lock()
		if l.conn == conn {
			l.conn = nil
		}
		l.mu.Unlock()
		conn.Close()
	}
	return res, nil
}

// connFor returns the link's connection for ref, dialing one when there is
// none or the one there was opened for a different scope. The caller holds
// the gate.
func (l *WorkspaceLink) connFor(ctx context.Context, c *Client, ref WorkspaceRef) (*tls.Conn, error) {
	pin, _ := c.known.GetByName(ref.Target)
	b, _ := json.Marshal([]string{c.relayURL, c.transport, c.token, c.id.Fingerprint, c.label, pin.Fingerprint, ref.String()})
	key := sha256.Sum256(b)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, fmt.Errorf("workspace connection is closed")
	}
	conn := l.conn
	generation := l.generation
	if conn != nil && l.key != key {
		l.conn = nil
		l.mu.Unlock()
		conn.Close()
		conn = nil
		l.mu.Lock()
	}
	if l.closed || generation != l.generation {
		l.mu.Unlock()
		return nil, fmt.Errorf("workspace connection scope changed")
	}
	l.mu.Unlock()
	if conn == nil {
		var err error
		conn, err = c.connectKind(ctx, ref.Target, protocol.KindWorkspaceHello)
		if err != nil {
			return nil, fmt.Errorf("reusable workspace connection: %w", err)
		}
		l.mu.Lock()
		if l.closed || generation != l.generation {
			l.mu.Unlock()
			conn.Close()
			return nil, fmt.Errorf("workspace connection is closed")
		}
		l.conn, l.key = conn, key
		l.mu.Unlock()
	}
	return conn, nil
}

// Warm dials the link's connection for ref so the handshake can overlap other
// work before the first request. Only the hello goes out, and the device does
// no more with it than check this controller's trust, as for any connection;
// nothing reaches its policy until a request does.
func (l *WorkspaceLink) Warm(ctx context.Context, c *Client, ref WorkspaceRef) error {
	select {
	case l.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.gate }()
	_, err := l.connFor(ctx, c, ref)
	return err
}
