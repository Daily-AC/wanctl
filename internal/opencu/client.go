// Package opencu speaks the public OpenCU MCP stdio host interface. Tool names
// and schemas belong to the peer, never to wanctl.
package opencu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const InstallCommand = "irm https://github.com/Daily-AC/opencu/releases/latest/download/install.ps1 | iex"
const NotInstalledMessage = "OPENCU_NOT_INSTALLED: Ask the person to run `" + InstallCommand + "` in PowerShell on this device (no admin needed), then try again. The actions mode remains available."
const MaxResult = 64 << 20

var ErrNotInstalled = errors.New(NotInstalledMessage)
var ErrUnknown = errors.New("OpenCU outcome unknown: the process or stream failed during the call; observe again before acting; never replay this call")

type Manifest struct {
	MCP struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcp"`
}

// ReadManifest is called only inside the selected user's helper.
func ReadManifest() (Manifest, error) {
	var m Manifest
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return m, ErrNotInstalled
	}
	b, err := os.ReadFile(filepath.Join(dir, "OpenCU", "opencu.json"))
	if err != nil {
		return m, ErrNotInstalled
	}
	if json.Unmarshal(b, &m) != nil || !filepath.IsAbs(m.MCP.Command) {
		return m, errors.New("invalid OpenCU manifest: mcp.command must be absolute")
	}
	for _, arg := range m.MCP.Args {
		if !filepath.IsAbs(arg) {
			return m, errors.New("invalid OpenCU manifest: mcp.args must be absolute paths")
		}
	}
	return m, nil
}

type Client struct {
	cmd        *exec.Cmd
	in         io.WriteCloser
	out        io.ReadCloser
	scan       *bufio.Scanner
	done       chan struct{}
	once       sync.Once
	id         int
	initialize json.RawMessage
}

// Start receives a prepared, hidden, non-elevated command from the helper.
// stderr is drained but never reflected into audit logs or controller output.
func Start(ctx context.Context, cmd *exec.Cmd) (*Client, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, writer, err := os.Pipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	cmd.Stdout = writer
	cmd.Stderr = io.Discard
	// Bound a descendant that inherited stderr after the MCP process exits.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		writer.Close()
		return nil, errors.New("cannot start OpenCU MCP server")
	}
	writer.Close()
	c := &Client{cmd: cmd, in: in, out: out, scan: bufio.NewScanner(out), done: make(chan struct{})}
	c.scan.Buffer(make([]byte, 64<<10), MaxResult)
	go func() { _ = cmd.Wait(); close(c.done) }()
	raw, err := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "wanctl", "version": "1"}})
	if err == nil {
		var init struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(raw, &init) != nil || (init.ProtocolVersion != "2024-11-05" && init.ProtocolVersion != "2025-03-26" && init.ProtocolVersion != "2025-06-18" && init.ProtocolVersion != "2025-11-25") {
			err = errors.New("unsupported OpenCU MCP protocol version")
		}
	}
	if err == nil {
		err = json.NewEncoder(c.in).Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	}
	if err != nil {
		c.Close()
		return nil, errors.New("OpenCU MCP initialization failed")
	}
	c.initialize = raw
	return c, nil
}

func (c *Client) Close() {
	c.once.Do(func() {
		c.in.Close()
		defer c.out.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
}

// rpc is serialized by the host's device lock. It never retries a request.
func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { c.Close(); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	c.id++
	if err := json.NewEncoder(c.in).Encode(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params}); err != nil {
		return nil, ErrUnknown
	}
	for c.scan.Scan() {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(c.scan.Bytes(), &msg) != nil || msg.JSONRPC != "2.0" {
			return nil, ErrUnknown
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 { // No client capabilities were advertised.
				if json.NewEncoder(c.in).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "host method not supported"}}) != nil {
					return nil, ErrUnknown
				}
			}
			continue
		}
		if string(msg.ID) != fmt.Sprint(c.id) {
			return nil, ErrUnknown
		}
		if len(msg.Error) > 0 {
			return nil, &RPCError{Raw: msg.Error}
		}
		if len(msg.Result) == 0 {
			return nil, ErrUnknown
		}
		return msg.Result, nil
	}
	return nil, ErrUnknown
}

type RPCError struct{ Raw json.RawMessage }

func (e *RPCError) Error() string { return "OpenCU MCP returned a JSON-RPC error" }

func (c *Client) Discover(ctx context.Context) (json.RawMessage, error) {
	var init map[string]json.RawMessage
	if json.Unmarshal(c.initialize, &init) != nil {
		return nil, ErrUnknown
	}
	all := []json.RawMessage{}
	cursor := ""
	seen := map[string]bool{}
	bytes := len(c.initialize)
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.rpc(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []json.RawMessage `json:"tools"`
			Next  string            `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Tools == nil {
			return nil, ErrUnknown
		}
		bytes += len(raw)
		if bytes > MaxResult {
			return nil, errors.New("OpenCU discovery exceeds 64 MiB")
		}
		all = append(all, page.Tools...)
		cursor = page.Next
		if cursor == "" {
			break
		}
		if seen[cursor] || len(all) > 10000 {
			return nil, errors.New("invalid OpenCU tools pagination")
		}
		seen[cursor] = true
	}
	return json.Marshal(struct {
		ServerInfo   json.RawMessage   `json:"serverInfo"`
		Instructions json.RawMessage   `json:"instructions,omitempty"`
		Tools        []json.RawMessage `json:"tools"`
	}{init["serverInfo"], init["instructions"], all})
}
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return c.rpc(ctx, "tools/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, args})
}

// ErrorCode does not rewrite the peer result, including the person's stop text.
func ErrorCode(raw json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	var code string
	_ = json.Unmarshal(r.Meta["opencu/error"], &code)
	if code == "USER_STOPPED" || code == "STOPPED_BY_PERSON" {
		return code
	}
	for _, item := range r.Content {
		if item.Type == "text" {
			prefix, _, _ := strings.Cut(item.Text, ":")
			if prefix == "USER_STOPPED" || prefix == "STOPPED_BY_PERSON" || prefix == "PERSON_ACTIVE" {
				return prefix
			}
			break
		}
	}
	return code
}
