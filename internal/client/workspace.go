package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"wanctl/internal/protocol"
	"wanctl/internal/wsconn"
)

// WorkspaceRef locates a workspace; it never grants access by itself.
type WorkspaceRef struct{ Target, ID string }

func (r WorkspaceRef) String() string { return r.Target + "#" + r.ID }

func ParseWorkspace(ref string) (WorkspaceRef, error) {
	target, id, ok := strings.Cut(ref, "#")
	if !ok || len(strings.Split(target, "/")) != 2 || len(id) != 34 || !strings.HasPrefix(id, "w-") {
		return WorkspaceRef{}, fmt.Errorf("invalid workspace reference; use the exact workspace returned by wanctl_workspace")
	}
	if _, err := hex.DecodeString(id[2:]); err != nil {
		return WorkspaceRef{}, fmt.Errorf("invalid workspace ID")
	}
	parts := strings.Split(target, "/")
	if parts[0] == "" || parts[1] == "" {
		return WorkspaceRef{}, fmt.Errorf("invalid workspace target")
	}
	return WorkspaceRef{target, id}, nil
}

func NewRequestID() string { return "r-" + rand.Text() }

// Resolve aliases once and choose an ID before opening, so a lost response
// can be recovered without creating a second workspace.
func (c *Client) PrepareWorkspace(ctx context.Context, target string) (WorkspaceRef, error) {
	canonical, err := c.resolve(ctx, target)
	if err != nil {
		return WorkspaceRef{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return WorkspaceRef{}, err
	}
	return WorkspaceRef{canonical, "w-" + hex.EncodeToString(id[:])}, nil
}

func (c *Client) Workspace(ctx context.Context, ref WorkspaceRef, action string, req protocol.Message) (*protocol.WorkspaceResult, error) {
	conn, err := c.connect(ctx, ref.Target)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()
	req.Kind, req.Action, req.WorkspaceID = protocol.KindWorkspace, action, ref.ID
	if err := protocol.WriteMessage(conn, req); err != nil {
		return nil, err
	}
	res, err := protocol.ReadMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("workspace result unknown: %w; workspace=%s request_id=%s; query this reference before resubmitting", err, ref.String(), req.RequestID)
	}
	if res.Kind == protocol.KindError && strings.Contains(res.Reason, "unknown request") {
		return nil, &UnsupportedError{Target: ref.Target, Kind: protocol.KindWorkspace}
	}
	if res.Kind == protocol.KindReject || res.Kind == protocol.KindError {
		return nil, fmt.Errorf("%s", res.Reason)
	}
	if res.Kind != protocol.KindWorkspace || res.Workspace == nil || res.Workspace.ID != ref.ID {
		return nil, fmt.Errorf("invalid workspace response; nothing may fall back to legacy exec")
	}
	return res.Workspace, nil
}
