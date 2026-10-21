package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

const OpenCUIdle = 10 * time.Minute

type OpenCUConnection interface {
	Do(context.Context, protocol.DesktopRequest) (protocol.OpenCUResult, error)
	Close()
}
type OpenCUStart func(context.Context) (OpenCUConnection, error)
type openCUSession struct {
	conn       OpenCUConnection
	stopped    bool
	active     bool
	timer      *time.Timer
	generation uint64
}

// DoOpenCU shares the legacy device lock, including while approval is pending.
// key is the authenticated controller fingerprint. The Service belongs to one
// device, so conversations under that identity share a connection and stop latch.
// approve(true) MUST ask a person directly, ignoring rules and bypass.
func (s *Service) DoOpenCU(ctx context.Context, key, requestID string, req protocol.DesktopRequest, approve func(bool) bool, start OpenCUStart) protocol.DesktopResult {
	res := protocol.DesktopResult{RequestID: requestID, Status: "rejected", FailedIndex: -1}
	if err := Validate("act", &req); err != nil {
		res.Error = err.Error()
		return res
	}
	if len(requestID) != 32 || !validID(requestID) {
		res.Error = "act needs a random 32-digit hexadecimal request_id"
		return res
	}
	if !s.busy.TryLock() {
		res.Error = "desktop busy; this request was not queued or replayed"
		return res
	}
	defer s.busy.Unlock()
	s.cuMu.Lock()
	if s.cuClosed {
		s.cuMu.Unlock()
		res.Error = "OpenCU service closed"
		return res
	}
	if s.cu == nil {
		s.cu = map[string]*openCUSession{}
	}
	e := s.cu[key]
	if e == nil {
		e = &openCUSession{}
		s.cu[key] = e
	}
	e.active = true
	e.generation++
	if e.timer != nil {
		e.timer.Stop()
	}
	stopped := e.stopped
	s.cuMu.Unlock()
	defer func() {
		s.cuMu.Lock()
		defer s.cuMu.Unlock()
		e.active = false
		if s.cu[key] != e {
			return
		}
		idle := s.cuIdle
		if idle == 0 {
			idle = OpenCUIdle
		}
		generation := e.generation
		e.timer = time.AfterFunc(idle, func() {
			s.cuMu.Lock()
			defer s.cuMu.Unlock()
			if s.cu[key] == e && e.generation == generation && !e.active && e.conn != nil {
				e.conn.Close()
				e.conn = nil
			}
		})
	}()
	if !approve(stopped) {
		res.Error = "command denied by device policy"
		if stopped {
			res.OpenCU = &protocol.OpenCUResult{Code: "OPENCU_STOPPED", Message: "The person stopped OpenCU. Fresh explicit approval is required; no connection was opened. Ask the person; do not retry automatically."}
		}
		return res
	}
	if ctx.Err() != nil {
		res.Error = "OpenCU request cancelled before execution"
		return res
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s.cuMu.Lock()
	if s.cuClosed || s.cu[key] != e {
		s.cuMu.Unlock()
		res.Error = "OpenCU service closed"
		return res
	}
	if stopped {
		if e.conn != nil {
			e.conn.Close()
			e.conn = nil
		}
		e.stopped = false
	}
	if e.conn == nil {
		conn, err := start(callCtx)
		if err != nil {
			s.cuMu.Unlock()
			res.Error = err.Error()
			return res
		}
		e.conn = conn
	}
	conn := e.conn
	s.cuMu.Unlock()

	reply, err := conn.Do(callCtx, req)
	s.cuMu.Lock()
	defer s.cuMu.Unlock()
	if err != nil {
		conn.Close()
		if e.conn == conn {
			e.conn = nil
		}
		res.Status = "unknown"
		res.Error = opencu.ErrUnknown.Error()
		return res
	}
	code := opencu.ErrorCode(reply.Result)
	if code == "USER_STOPPED" || code == "STOPPED_BY_PERSON" {
		e.stopped = true
	}
	// RPC failure is known; retain its exact error object and do not replay it.
	if reply.Code == "OPENCU_NOT_INSTALLED" || reply.Code == "OPENCU_START_FAILED" {
		conn.Close()
		if e.conn == conn {
			e.conn = nil
		}
	}
	res.Status = "completed"
	res.OpenCU = &reply
	if reply.Code != "" || len(reply.RPCError) > 0 {
		res.Status = "rejected"
		res.Error = reply.Message
	}
	if code == "PERSON_ACTIVE" {
		res.Warning = "Stop and ask your user; never retry automatically."
	}
	return res
}

func (s *Service) Close() {
	s.cuMu.Lock()
	defer s.cuMu.Unlock()
	s.cuClosed = true
	for key, e := range s.cu {
		delete(s.cu, key)
		if e.timer != nil {
			e.timer.Stop()
		}
		if e.conn != nil {
			e.conn.Close()
		}
	}
}

// A persistent instance of the existing user-session helper owns the MCP peer.
func StartOpenCU(ctx context.Context) (OpenCUConnection, error) {
	p, err := startPlatformOpenCUHelper(ctx)
	if err != nil {
		return nil, err
	}
	h := &openCUHelper{p: p, done: make(chan struct{})}
	go func() { _ = p.wait(); close(h.done) }()
	return h, nil
}

type openCUHelper struct {
	p    *helperProcess
	done chan struct{}
	once sync.Once
}

func (h *openCUHelper) Close() {
	h.once.Do(func() {
		h.p.stdin.Close()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			_ = h.p.kill()
			<-h.done
		}
		h.p.stdout.Close()
	})
}
func (h *openCUHelper) Do(ctx context.Context, req protocol.DesktopRequest) (protocol.OpenCUResult, error) {
	if ctx.Err() != nil {
		return protocol.OpenCUResult{}, ctx.Err()
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { h.Close(); close(finished) })
	defer func() {
		if !stop() {
			<-finished
		}
	}()
	raw, err := json.Marshal(Job{Action: "act", Request: req})
	if err == nil {
		err = protocol.WriteFrame(h.p.stdin, protocol.FrameJSON, raw)
	}
	if err != nil {
		return protocol.OpenCUResult{}, err
	}
	res, _, err := ReadResult(h.p.stdout)
	if err != nil {
		return protocol.OpenCUResult{}, err
	}
	if res.OpenCU == nil {
		return protocol.OpenCUResult{}, errors.New("missing OpenCU result")
	}
	return *res.OpenCU, nil
}

// openCUHelperMain reads jobs while idle AND while a call is in progress, so
// agent EOF cancels a running MCP call and closes its stdin before helper exit.
func openCUHelperMain(in io.Reader, out io.Writer, first Job) int {
	return openCUHelperMainUsing(in, out, first, checkOpenCUSession)
}

func openCUHelperMainUsing(in io.Reader, out io.Writer, first Job, checkSession func() error) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan Job)
	go func() {
		defer cancel()
		defer close(jobs)
		for {
			ft, raw, err := protocol.ReadFrame(in)
			if err != nil || ft != protocol.FrameJSON {
				return
			}
			var j Job
			if json.Unmarshal(raw, &j) != nil {
				return
			}
			select {
			case jobs <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	var peer *opencu.Client
	defer func() {
		if peer != nil {
			peer.Close()
		}
	}()
	job := first
	for {
		reply := protocol.OpenCUResult{}
		if err := Validate(job.Action, &job.Request); err != nil {
			return 1
		}
		if peer == nil {
			err := checkSession()
			if err == nil {
				var m opencu.Manifest
				m, err = opencu.ReadManifest()
				if err == nil {
					cmd := exec.Command(m.MCP.Command, m.MCP.Args...)
					helperProcessAttrs(cmd)
					initCtx, stop := context.WithTimeout(ctx, 30*time.Second)
					peer, err = opencu.Start(initCtx, cmd)
					stop()
				}
			}
			if err != nil {
				reply.Code = "OPENCU_START_FAILED"
				reply.Message = err.Error()
				if errors.Is(err, opencu.ErrNotInstalled) {
					reply.Code = "OPENCU_NOT_INSTALLED"
				}
			}
		}
		if peer != nil {
			callCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
			var raw json.RawMessage
			var err error
			if job.Request.Discover {
				raw, err = peer.Discover(callCtx)
				reply.Discovery = raw
			} else {
				raw, err = peer.Call(callCtx, job.Request.Tool, job.Request.Arguments)
				reply.Result = raw
			}
			stop()
			if err != nil {
				var rpc *opencu.RPCError
				if errors.As(err, &rpc) {
					reply.RPCError = rpc.Raw
					reply.Message = rpc.Error()
				} else {
					return 1
				}
			}
		}
		if WriteResult(out, protocol.DesktopResult{OpenCU: &reply}, nil) != nil {
			return 1
		}
		select {
		case next, ok := <-jobs:
			if !ok {
				return 0
			}
			job = next
		case <-ctx.Done():
			return 0
		}
	}
}

// PauseForUpdate drains desktop work and closes idle peers before replacing the
// binary. The returned release also allows recovery after a failed update.
func (s *Service) PauseForUpdate() func() {
	s.busy.Lock()
	s.cuMu.Lock()
	for _, e := range s.cu {
		if e.timer != nil {
			e.timer.Stop()
		}
		if e.conn != nil {
			e.conn.Close()
			e.conn = nil
		}
	}
	s.cuMu.Unlock()
	return s.busy.Unlock
}

// TryPauseForInstall reserves the desktop without waiting for a live call.
// CloseIdleOpenCU runs after the console acknowledgement: closing many idle
// helper processes must not turn an accepted install into an RPC timeout.
func (s *Service) TryPauseForInstall() (func(), bool) {
	if !s.busy.TryLock() {
		return nil, false
	}
	return s.busy.Unlock, true
}

// CloseIdleOpenCU requires the desktop reservation returned above. No new
// call can start until that reservation is released.
func (s *Service) CloseIdleOpenCU() {
	s.cuMu.Lock()
	defer s.cuMu.Unlock()
	for _, e := range s.cu {
		if e.timer != nil {
			e.timer.Stop()
		}
		if e.conn != nil {
			e.conn.Close()
			e.conn = nil
		}
	}
}
