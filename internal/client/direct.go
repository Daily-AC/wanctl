package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"wanctl/internal/direct"
	"wanctl/internal/protocol"
)

type filePath struct {
	rw     io.ReadWriter
	direct bool
	close  func()
}

func relayPath(conn net.Conn) filePath { return filePath{rw: conn, close: func() {}} }

// selectFilePath is the sole sender of offer, fallback, and attach for an operation.
func (c *Client) selectFilePath(ctx context.Context, relay net.Conn, info *protocol.DirectInfo) (filePath, error) {
	settings := c.directSettings.Effective()
	fallback := func(reason string) (filePath, error) {
		fmt.Fprintf(os.Stderr, "direct lane fallback: %s\n", reason)
		if err := protocol.WriteMessage(relay, protocol.Message{Kind: protocol.KindDirectFallback}); err != nil {
			return filePath{}, err
		}
		return relayPath(relay), nil
	}
	if len(direct.ValidateCandidates(info.Candidates, settings.AllowLoopback)) == 0 {
		return fallback("no valid peer candidates")
	}
	ep, err := direct.Open(ctx, settings)
	if err != nil {
		return fallback("UDP socket unavailable")
	}
	if len(ep.Info().Candidates) == 0 {
		ep.Close()
		return fallback("no local candidates")
	}
	offerSent := time.Now()
	if err := protocol.WriteMessage(relay, protocol.Message{Kind: protocol.KindDirectOffer, Direct: ep.Info()}); err != nil {
		ep.Close()
		return filePath{}, err
	}
	deadline := offerSent.Add(settings.Budget)
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	conn, err := ep.Dial(dialCtx, info)
	cancel()
	if err != nil {
		ep.Close()
		return fallback("punching timed out")
	}
	if time.Now().After(deadline) {
		conn.CloseWithError(1, "direct budget expired")
		ep.Close()
		return fallback("punching timed out")
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		conn.CloseWithError(1, "stream unavailable")
		ep.Close()
		return fallback("stream unavailable")
	}
	release := func() { conn.CloseWithError(0, ""); ep.Close() }
	stop := context.AfterFunc(ctx, func() { conn.CloseWithError(1, "controller cancelled") })
	closePath := func() { stop(); release() }
	if err := protocol.WriteMessage(stream, protocol.Message{Kind: protocol.KindDirectAttach}); err != nil {
		closePath()
		return filePath{}, fmt.Errorf("direct attach failed: %w", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(settings.AckWait))
	ack, err := protocol.ReadMessage(stream)
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil {
		closePath()
		return filePath{}, fmt.Errorf("direct attach acknowledgement failed: %w", err)
	}
	if ack.Kind == protocol.KindError {
		closePath()
		return filePath{}, fmt.Errorf("direct attach refused: %s", ack.Reason)
	}
	if ack.Kind != protocol.KindOK {
		closePath()
		return filePath{}, fmt.Errorf("unexpected direct attach reply: %s", ack.Kind)
	}
	return filePath{rw: stream, direct: true, close: closePath}, nil
}
