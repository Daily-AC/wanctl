package relay

import "time"

// The carrier waits 250 ms before retrying a failed down poll. Leave room for
// that retry plus a round trip, while cancelling an abandoned desktop/exec
// session promptly instead of waiting for the 60-second idle reaper.
const controllerDisconnectGrace = 2 * time.Second

// Every controller poll, including a numbered one, proves that it is still
// here. The generation also rejects a cancellation from an older handler
// which only notices its dead connection after the replacement poll arrives.
func (r *Relay) controllerPollStarted(sid string, s *httpSession) uint64 {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	if r.hsess[sid] != s {
		return 0
	}
	s.clientPollGeneration++
	if s.clientDisconnectTimer != nil {
		s.clientDisconnectTimer.Stop()
		s.clientDisconnectTimer = nil
	}
	return s.clientPollGeneration
}

func (r *Relay) controllerPollCancelled(sid string, s *httpSession, generation uint64) {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	if r.hsess[sid] != s || s.clientPollGeneration != generation || !s.closedAt.IsZero() || s.endReason != "" {
		return
	}
	// At most one timer per session, even if cancellation is reported twice.
	if s.clientDisconnectTimer == nil {
		s.clientDisconnectTimer = time.AfterFunc(controllerDisconnectGrace, func() { r.expireControllerDisconnect(sid, s, generation) })
	}
}

func (r *Relay) expireControllerDisconnect(sid string, s *httpSession, generation uint64) {
	r.hmu.Lock()
	// Stop may race an already-started callback. Its generation must still be
	// current, so an old callback cannot erase or expire a newer grace period.
	if s.clientPollGeneration != generation || s.clientDisconnectTimer == nil {
		r.hmu.Unlock()
		return
	}
	s.clientDisconnectTimer = nil
	if r.hsess[sid] != s || !s.closedAt.IsZero() || s.endReason != "" {
		r.hmu.Unlock()
		return
	}
	// Retire under the SAME lock as poll arrival. Checking then unlocking before
	// removal would let a retry clear the mark but still have its session closed.
	delete(r.hsess, sid)
	r.hmu.Unlock()
	r.closeHTTPSession(sid, s)
}
