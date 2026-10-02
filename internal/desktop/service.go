package desktop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"wanctl/internal/protocol"
)

type Job struct {
	Action     string                    `json:"action"`
	Request    protocol.DesktopRequest   `json:"request"`
	Reference  *protocol.DesktopSnapshot `json:"reference,omitempty"`
	Controller string                    `json:"controller"`
}
type Runner func(context.Context, Job) (protocol.DesktopResult, []byte, error)

// Service is embedded in Agent, not a resident desktop process. TryLock avoids
// queueing input behind another controller or behind a stale screenshot.
type Service struct {
	busy  sync.Mutex
	Store Store
}

func (s *Service) Do(ctx context.Context, peer, label, action, requestID string, req *protocol.DesktopRequest, run Runner) (protocol.DesktopResult, []byte) {
	res := protocol.DesktopResult{RequestID: requestID, Status: "rejected", FailedIndex: -1}
	if err := ctx.Err(); err != nil {
		res.Error = "desktop request cancelled before execution"
		return res, nil
	}
	if err := Validate(action, req); err != nil {
		res.Error = err.Error()
		return res, nil
	}
	if action == "act" && (len(requestID) != 32 || !validID(requestID)) {
		res.Error = "act needs a random 32-digit hexadecimal request_id"
		return res, nil
	}
	if !s.busy.TryLock() {
		res.Error = "desktop busy; another call is in progress"
		if action == "act" {
			res.Status = "unknown"
			res.Error = "state unknown: desktop busy; an earlier call may be partially completed; this request was not queued or replayed"
		}
		return res, nil
	}
	defer s.busy.Unlock()
	job := Job{Action: action, Request: *req, Controller: label}
	if req.ScreenshotID != "" {
		snap, err := s.Store.Get(peer, req.ScreenshotID, action == "act", time.Now())
		if err != nil {
			res.Error = err.Error()
			if strings.HasPrefix(res.Error, "state unknown") {
				res.Status = "unknown"
			}
			return res, nil
		}
		job.Reference = &snap
		if req.Region != nil {
			// Region is always relative to the full-desktop image, never nested crops.
			if snap.Source.X != snap.Origin.X || snap.Source.Y != snap.Origin.Y || !isFullSnapshot(snap) {
				res.Error = "region needs a full-desktop screenshot_id"
				return res, nil
			}
			if _, err := CropSource(snap, *req.Region); err != nil {
				res.Error = err.Error()
				return res, nil
			}
		}
	}
	res, data, err := run(ctx, job)
	res.RequestID = requestID
	if err != nil {
		res.Status = "unknown"
		res.Error = "state unknown: desktop helper failed or disconnected; input may be partially completed; do not replay"
		res.Snapshot = nil
		res.ImageBytes = 0
		res.FailedIndex = -1
		return res, nil
	}
	if res.Snapshot != nil {
		snap, err := s.Store.Put(peer, *res.Snapshot, time.Now())
		if err != nil {
			res.Snapshot = nil
			data = nil
			if res.Error == "" {
				res.Error = err.Error()
				res.Status = "partial"
			}
		} else {
			res.Snapshot = &snap
		}
	}
	res.ImageBytes = len(data)
	if len(data) > 0 {
		res.MIME = "image/jpeg"
	}
	return res, data
}
func validID(id string) bool {
	for _, c := range id {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func isFullSnapshot(s protocol.DesktopSnapshot) bool {
	right, bottom := s.Origin.X, s.Origin.Y
	for _, m := range s.Monitors {
		right = max(right, m.Rect.X+m.Rect.Width)
		bottom = max(bottom, m.Rect.Y+m.Rect.Height)
	}
	return s.Source.Width == right-s.Origin.X && s.Source.Height == bottom-s.Origin.Y
}

var ErrWindowsOnly = errors.New(protocol.DesktopWindowsOnly)

// Busy lets the agent defer its ordinary self-update until input has stopped.
func (s *Service) Busy() bool {
	if !s.busy.TryLock() {
		return true
	}
	s.busy.Unlock()
	return false
}
