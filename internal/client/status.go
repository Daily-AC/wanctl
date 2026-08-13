package client

import (
	"context"
	"fmt"
	"strings"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// AgentStatus is the read-only state a remote agent exposes to a paired
// controller. Detailed is false when the connection reached an older agent
// which predates the status verb.
type AgentStatus struct {
	Name     string
	Mode     policy.Mode
	Version  string
	Detailed bool
}

// Status connects to target, proving that it is online, then requests the
// details supported by current agents. Older agents reject the unknown verb;
// that response is a successful online check rather than a command failure.
func (c *Client) Status(ctx context.Context, target string) (AgentStatus, error) {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return AgentStatus{}, err
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindStatus}); err != nil {
		return AgentStatus{}, err
	}
	reply, err := protocol.ReadMessage(conn)
	if err != nil {
		return AgentStatus{}, err
	}
	return decodeStatus(reply)
}

func decodeStatus(reply protocol.Message) (AgentStatus, error) {
	switch reply.Kind {
	case protocol.KindStatus:
		mode := policy.Mode(reply.ConsoleMode)
		if mode != policy.ModeNormal && mode != policy.ModeBypass {
			return AgentStatus{}, fmt.Errorf("remote status: invalid policy mode %q", mode)
		}
		return AgentStatus{Name: reply.Name, Mode: mode, Version: reply.Version, Detailed: true}, nil
	case protocol.KindError:
		if strings.HasPrefix(reply.Reason, "unknown request") || strings.Contains(string(reply.Data), "unknown RPC kind") {
			return AgentStatus{}, nil
		}
		return AgentStatus{}, fmt.Errorf("remote status: %s", reply.Reason)
	case protocol.KindReject:
		return AgentStatus{}, rejectError(reply)
	default:
		return AgentStatus{}, fmt.Errorf("remote status: unexpected device reply %q", reply.Kind)
	}
}
