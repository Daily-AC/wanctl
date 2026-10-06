package desktop

import (
	"context"

	"wanctl/internal/protocol"
)

// executeJobWithBackend keeps job handling testable without a live desktop.
// The platform wrapper owns thread affinity, DPI setup and panic recovery.
func executeJobWithBackend(ctx context.Context, job Job, backend Backend, checkSession func() error) (res protocol.DesktopResult, data []byte) {
	res.Status = "rejected"
	res.FailedIndex = -1
	if err := checkSession(); err != nil {
		res.Error = err.Error()
		return
	}
	var source *protocol.Rect
	if job.Reference != nil && job.Action == "screenshot" {
		if err := backend.Check(job.Reference.Layout); err != nil {
			res.Error = err.Error()
			return
		}
		if job.Request.Region != nil {
			r, err := CropSource(*job.Reference, *job.Request.Region)
			if err != nil {
				res.Error = err.Error()
				return
			}
			source = &r
		}
	}
	if job.Action == "act" {
		if job.Reference == nil {
			res.Error = "act needs a screenshot reference"
			return
		}
		res = (Engine{Backend: backend}).Run(ctx, job.Controller, *job.Reference, job.Request.Actions)
		if res.Error == ErrLocked.Error() {
			return // Never capture after observing a lock, even if it is lifted.
		}
	} else {
		res.Status = "completed"
	}
	if ctx.Err() != nil {
		if res.Error == "" {
			res.Status = "partial"
			res.Error = "controller disconnected; input may be partially completed; do not replay"
		}
		return
	}
	snap, jpeg, err := backend.Capture(source)
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
			if res.Completed > 0 {
				res.Status = "partial"
			} else {
				res.Status = "rejected"
			}
		}
		return
	}
	res.Snapshot = &snap
	data = jpeg
	return
}
