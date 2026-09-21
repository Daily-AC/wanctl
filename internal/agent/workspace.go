package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"wanctl/internal/eventlog"
	"wanctl/internal/limits"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/server"
	"wanctl/internal/sessionauth"
)

const maxWorkspaces = 16
const maxWorkspaceRequests = 128
const maxWorkspaceOutput = 8 << 20
const maxWorkspaceInput = 1 << 20
const workspaceOutputPage = 48 << 10

// The device owns these shells and jobs. No context or writer belonging to a
// controller connection is retained by a running command.
type workspace struct {
	mu              sync.Mutex
	id, owner, root string
	shell           *server.ShellSession
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	closed          bool
	stopping        bool
	active          string
	jobs            map[string]*workspaceJob
	outputBytes     int
	inputBytes      int
}

type workspaceJob struct {
	command, cwd string
	state        string
	output       []byte
	total        int64
	code         int
	err          string
	done         bool
}

func validWorkspaceID(id string) bool {
	if len(id) != 34 || !strings.HasPrefix(id, "w-") {
		return false
	}
	for _, c := range id[2:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func validWorkspaceRequest(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (a *Agent) getWorkspace(fp, id string) (*workspace, error) {
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	w := a.workspaces[id]
	if w == nil || w.owner != fp {
		return nil, fmt.Errorf("workspace unavailable: unknown, closed, or not owned by this controller; never fall back to another workspace")
	}
	return w, nil
}

func (a *Agent) openWorkspace(fp string, m protocol.Message) (*workspace, error) {
	if !validWorkspaceID(m.WorkspaceID) {
		return nil, fmt.Errorf("invalid workspace_id")
	}
	if !filepath.IsAbs(m.Path) {
		return nil, fmt.Errorf("workspace root must be an absolute device path")
	}
	root := filepath.Clean(m.Path)
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory")
	}
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	if a.workspacesClosed {
		return nil, fmt.Errorf("device is shutting down")
	}
	if w := a.workspaces[m.WorkspaceID]; w != nil {
		if w.owner != fp || w.root != root {
			return nil, fmt.Errorf("workspace_id already names a different workspace")
		}
		return w, nil
	}
	if len(a.workspaces) >= maxWorkspaces {
		return nil, fmt.Errorf("workspace limit reached (%d); explicitly close an unused workspace", maxWorkspaces)
	}
	shell, err := server.NewShellSessionInDir(a.opts.Shell, root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &workspace{id: m.WorkspaceID, owner: fp, root: root, shell: shell, ctx: ctx, cancel: cancel, jobs: map[string]*workspaceJob{}}
	if a.workspaces == nil {
		a.workspaces = map[string]*workspace{}
	}
	a.workspaces[w.id] = w
	return w, nil
}

func (w *workspace) stateLocked() string {
	if w.closed {
		return "closed"
	}
	if w.stopping || w.shell.Closed() {
		return "invalid"
	}
	return "open"
}

func (w *workspace) snapshot(id string, offset int64) (*protocol.WorkspaceResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := &protocol.WorkspaceResult{ID: w.id, Root: w.root, State: w.stateLocked(), ActiveRequestID: w.active}
	if id == "" {
		return r, nil
	}
	j := w.jobs[id]
	if j == nil {
		return nil, fmt.Errorf("unknown request_id in this workspace; do not guess whether a command ran")
	}
	if offset < 0 || offset > int64(len(j.output)) {
		return nil, fmt.Errorf("offset is outside retained output")
	}
	end := min(int64(len(j.output)), offset+workspaceOutputPage)
	// JSON strings must not split a UTF-8 rune across pages. While a command
	// is still writing, hold an incomplete trailing rune for the next poll.
	if end < int64(len(j.output)) {
		for end > offset && !utf8.RuneStart(j.output[end]) {
			end--
		}
		if end == offset {
			end = min(int64(len(j.output)), offset+workspaceOutputPage)
		}
	} else if !j.done && j.total == int64(len(j.output)) {
		start := end
		for start > offset && end-start < utf8.UTFMax {
			start--
			if utf8.RuneStart(j.output[start]) {
				if !utf8.FullRune(j.output[start:end]) {
					end = start
				}
				break
			}
		}
	}
	r.RequestID, r.Done, r.Code, r.Error = id, j.done, j.code, j.err
	r.RequestState = j.state
	r.Output, r.NextOffset, r.OutputBytes = string(j.output[offset:end]), end, j.total
	r.RetainedBytes = int64(len(j.output))
	r.Truncated = j.total > int64(len(j.output))
	return r, nil
}

// resolve is only path interpretation. File policy and rooted file opens still
// run afterwards; a workspace root does not claim to sandbox arbitrary exec.
func (w *workspace) resolve(path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", fmt.Errorf("workspace is closed")
	}
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.root, path)
	}
	return filepath.Clean(path), nil
}

type workspaceWriter struct {
	w  *workspace
	id string
}

func (o workspaceWriter) Write(p []byte) (int, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	j := o.w.jobs[o.id]
	j.total += int64(len(p))
	n := min(len(p), maxWorkspaceOutput-o.w.outputBytes)
	if n > 0 {
		j.output = append(j.output, p[:n]...)
		o.w.outputBytes += n
	}
	return len(p), nil
}

// reserve records the request before approval. Concurrent delivery of the same
// request therefore shares even the pending decision, not just the execution.
func (w *workspace) reserve(m protocol.Message) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !validWorkspaceRequest(m.RequestID) {
		return false, fmt.Errorf("request_id must be 1-128 letters, digits, '-' or '_'")
	}
	if j := w.jobs[m.RequestID]; j != nil {
		if j.command != m.Command || j.cwd != m.Cwd {
			return false, fmt.Errorf("request_id conflict: different command or cwd; nothing was started")
		}
		return false, nil
	}
	if w.stateLocked() != "open" {
		return false, fmt.Errorf("workspace shell is unavailable; state was lost, explicitly close and open a new workspace")
	}
	if w.active != "" {
		return false, fmt.Errorf("workspace busy: poll request_id %s before submitting another command", w.active)
	}
	if len(w.jobs) >= maxWorkspaceRequests {
		return false, fmt.Errorf("workspace request ledger full (%d); close and open a new workspace; old IDs are never silently reused", maxWorkspaceRequests)
	}
	if len(m.Command)+len(m.Cwd) > maxWorkspaceInput-w.inputBytes {
		return false, fmt.Errorf("workspace command ledger byte limit reached; close and open a new workspace")
	}
	w.inputBytes += len(m.Command) + len(m.Cwd)
	w.jobs[m.RequestID] = &workspaceJob{command: m.Command, cwd: m.Cwd, state: "approving"}
	w.active = m.RequestID
	return true, nil
}

func (w *workspace) finishLocked(id string, code int, err error) {
	j := w.jobs[id]
	j.code, j.done, j.state = code, true, "done"
	if err != nil {
		j.err = err.Error()
	}
	if w.active == id {
		w.active = ""
	}
}

func (a *Agent) startWorkspaceCommand(w *workspace, fp, peer string, m protocol.Message, audit sessionAudit) error {
	if m.Command == "" {
		return fmt.Errorf("command is required")
	}
	if m.OneShot || m.Elevate || m.Via != "" {
		return fmt.Errorf("workspace exec does not support oneshot or elevation")
	}
	fresh, err := w.reserve(m)
	if err != nil || !fresh {
		return err
	}
	cwd := w.root
	if m.Cwd != "" {
		cwd, err = w.resolve(m.Cwd)
		if err != nil {
			w.mu.Lock()
			w.finishLocked(m.RequestID, -1, err)
			w.mu.Unlock()
			return err
		}
	}
	ok, decision := a.gate(policy.Request{Kind: policy.KindExec, Cmd: m.Command, Cwd: cwd, Peer: fp})
	a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peer, Detail: "[workspace " + w.id + " request " + m.RequestID + "] " + m.Command, Cwd: cwd, Decision: decision})
	w.mu.Lock()
	defer w.mu.Unlock()
	if !ok || w.stateLocked() != "open" {
		why := fmt.Errorf("command denied by device policy: %s", m.Command)
		if w.stateLocked() != "open" {
			why = fmt.Errorf("workspace stopped before execution")
		}
		w.finishLocked(m.RequestID, -1, why)
		return nil
	}
	w.jobs[m.RequestID].state = "running"
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ctx, cancel := context.WithTimeout(w.ctx, limits.JobRunTimeout)
		defer cancel()
		changeDir := ""
		if m.Cwd != "" {
			changeDir = cwd
		}
		code, runErr := w.shell.ExecInDirContext(ctx, m.Command, changeDir, workspaceWriter{w, m.RequestID})
		w.mu.Lock()
		w.finishLocked(m.RequestID, code, runErr)
		w.mu.Unlock()
		a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peer, Detail: "[workspace " + w.id + " request " + m.RequestID + "] " + m.Command, Cwd: cwd, Decision: "finished", Exit: &code})
	}()
	return nil
}

func (w *workspace) stop() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
	w.wg.Wait()
	w.shell.Close()
}

func (a *Agent) closeWorkspaces() {
	a.workspaceMu.Lock()
	a.workspacesClosed = true
	ws := a.workspaces
	a.workspaces = nil
	a.workspaceMu.Unlock()
	for _, w := range ws {
		w.stop()
	}
}

func (a *Agent) workspacesBusy() bool {
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	return len(a.workspaces) > 0
}

// handleWorkspace either handles a control/exec request, or unwraps a file
// request for the existing capability/policy/file pipeline. It never tunnels a
// caller-supplied kind past the capability gate.
func (a *Agent) handleWorkspace(conn io.ReadWriter, fp, peer string, m *protocol.Message, caps sessionauth.Capabilities, audit sessionAudit) bool {
	fail := func(err error) bool {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return true
	}
	file := m.Action == protocol.KindFileRead || m.Action == protocol.KindFileEdit || m.Action == protocol.KindFileWrite
	required := sessionauth.Exec
	if file {
		required = requiredCapability(m.Action)
	}
	if m.Action == "open" {
		required |= sessionauth.Read
	}
	if !caps.Has(required) {
		return fail(fmt.Errorf("session capability denied: %s", required.String()))
	}
	var w *workspace
	var err error
	if m.Action == "open" {
		ok, decision, _ := a.gateFile(policy.Request{Kind: policy.KindRead, Path: m.Path, Peer: fp})
		a.logSessionEvent(audit, eventlog.Event{Type: "workspace", PeerFP: fp, PeerName: peer, Detail: "open " + m.WorkspaceID + " " + m.Path, Decision: decision})
		if !ok {
			return fail(fmt.Errorf("read denied by device policy: %s", m.Path))
		}
		w, err = a.openWorkspace(fp, *m)
	} else {
		w, err = a.getWorkspace(fp, m.WorkspaceID)
	}
	if err != nil {
		return fail(err)
	}
	if file {
		m.Path, err = w.resolve(m.Path)
		if err != nil {
			return fail(err)
		}
		m.Kind = m.Action
		return false
	}
	switch m.Action {
	case "open", "status", "poll":
	case "exec":
		if err := a.startWorkspaceCommand(w, fp, peer, *m, audit); err != nil {
			return fail(err)
		}
	case "cancel":
		w.mu.Lock()
		if m.RequestID == "" || w.active != m.RequestID {
			w.mu.Unlock()
			return fail(fmt.Errorf("cancel must name the currently active request_id"))
		}
		w.stopping = true
		w.cancel()
		w.mu.Unlock()
		// Reliable cancellation destroys the shell (ADR 0011). Keep the
		// workspace and its ledger so callers can still collect the outcome.
		w.wg.Wait()
		w.shell.Close()
	case "close":
		w.stop()
		a.workspaceMu.Lock()
		delete(a.workspaces, w.id)
		a.workspaceMu.Unlock()
	default:
		return fail(fmt.Errorf("unknown workspace action %q", m.Action))
	}
	if m.Action == "cancel" || m.Action == "close" {
		a.logSessionEvent(audit, eventlog.Event{Type: "workspace", PeerFP: fp, PeerName: peer, Detail: m.Action + " " + w.id + " request " + m.RequestID, Decision: "accepted"})
	}
	r, err := w.snapshot(m.RequestID, m.Offset)
	if err != nil {
		return fail(err)
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindWorkspace, Workspace: r})
	return true
}
