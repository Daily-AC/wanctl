package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcp-go decodes tool argument maps through float64. Capture only OpenCU's
// opaque arguments before that decoding and restore them immediately before
// dispatch; large integers and future schemas must survive a host round trip.
// Entries exist only between parsing hooks, never across tool execution.
func openCUArgumentHooks() *server.Hooks {
	type key struct{ session, id string }
	requestKey := func(ctx context.Context, id any) key {
		k := key{id: fmt.Sprintf("%T:%v", id, id)}
		if session := server.ClientSessionFromContext(ctx); session != nil {
			k.session = session.SessionID()
		}
		return k
	}
	var mu sync.Mutex
	pending := map[key]json.RawMessage{}
	hooks := &server.Hooks{}
	hooks.AddOnRequestInitialization(func(ctx context.Context, id any, message any) error {
		raw, ok := message.(json.RawMessage)
		if !ok {
			return nil
		}
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil || req.Method != "tools/call" || req.Params.Name != "wanctl_act" {
			return nil
		}
		args, ok := req.Params.Arguments["arguments"]
		if !ok {
			return nil
		}
		k := requestKey(ctx, id)
		mu.Lock()
		defer mu.Unlock()
		if _, exists := pending[k]; exists {
			return fmt.Errorf("duplicate MCP request ID during OpenCU argument parsing")
		}
		pending[k] = args
		return nil
	})
	hooks.AddBeforeCallTool(func(ctx context.Context, id any, req *mcpapi.CallToolRequest) {
		k := requestKey(ctx, id)
		mu.Lock()
		raw, ok := pending[k]
		delete(pending, k)
		mu.Unlock()
		if ok && req.Params.Name == "wanctl_act" && req.GetArguments() != nil {
			req.GetArguments()["arguments"] = raw
		}
	})
	hooks.AddOnError(func(ctx context.Context, id any, _ mcpapi.MCPMethod, _ any, _ error) {
		mu.Lock()
		delete(pending, requestKey(ctx, id))
		mu.Unlock()
	})
	return hooks
}
