package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"
)

// Hosted (HTTP) MCP sessions are bounded in number and closed when idle.
//
// Opening one needs an OAuth bearer (oauthGate), and each is state in the relay
// process every tenant shares: mcp-go keeps a notification buffer and
// bookkeeping per session, and this package a record of it. mcp-go frees its
// part only when a client ends the session with DELETE, which a client that
// simply goes away never sends, so without these bounds a client could grow
// the process one initialize at a time.
const (
	// maxHostedSessions bounds the sessions open at once. One costs about
	// 10 KiB here, so this is a few tens of MiB at most.
	maxHostedSessions = 2048
	// sessionIdle is how long a session is kept without a request. Closing
	// one loses nothing: the login travels with every request in the bearer,
	// and a connector that opens a new session for every tool call would
	// otherwise leave hundreds.
	sessionIdle = 10 * time.Minute
)

const sessionIDPrefix = "mcp-session-"

var errTooManySessions = errors.New("too many MCP sessions are open on this server; try again in a few minutes")

// transportSession is one transport session the endpoint has admitted.
type transportSession struct {
	lastUsed time.Time
}

func (s *sessionStore) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *sessionStore) room() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.open) >= maxHostedSessions {
		return errTooManySessions
	}
	return nil
}

// admit records a request on session id, taking it in if it is not already
// open and there is room for it.
func (s *sessionStore) admit(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.open[id]; o != nil {
		o.lastUsed = s.now()
		return nil
	}
	if len(s.open) >= maxHostedSessions {
		return errTooManySessions
	}
	s.open[id] = &transportSession{lastUsed: s.now()}
	return nil
}

// forget drops everything this package holds for session id.
func (s *sessionStore) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, id)
}

// sweep closes the sessions idle past their limit and has mcp-go free its own
// state for each. It also drops a bearer's state (keyed by its token, not by a
// transport session) once that token has expired or gone unused.
func (s *sessionStore) sweep() {
	now := s.now()
	var closed []string
	s.mu.Lock()
	for id, o := range s.open {
		if now.Sub(o.lastUsed) > sessionIdle {
			delete(s.open, id)
			closed = append(closed, id)
		}
	}
	for id, r := range s.m {
		r.mu.Lock()
		gone := now.Sub(r.lastUsed) > sessionIdle || !now.Before(r.oauthClaim.Expiry())
		r.mu.Unlock()
		if gone {
			delete(s.m, id)
		}
	}
	terminate := s.terminate
	s.mu.Unlock()
	if terminate != nil {
		for _, id := range closed {
			terminate(id)
		}
	}
}

// gate refuses a new session the server has no room for, with an error a
// client can show. mcp-go names a new session through Generate, which cannot
// refuse, so the refusal has to happen before mcp-go sees the request: a POST
// or GET without a session ID is how a session is opened.
func (s *sessionStore) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		opening := (req.Method == http.MethodPost || req.Method == http.MethodGet) &&
			req.Header.Get(server.HeaderKeySessionID) == ""
		if opening {
			if err := s.room(); err != nil {
				refuseSession(w, err)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func refuseSession(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(int(time.Minute/time.Second)))
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": map[string]any{"code": -32000, "message": err.Error()},
	})
}

// sessionIDs is the SessionIdManager mcp-go names, checks and ends sessions
// through, so that every one it knows is one this store bounds and expires.
type sessionIDs struct{ store *sessionStore }

// Generate names a new session. The gate has already checked there is room;
// if another request took the last place since, the answer is no session at
// all, and the client, finding it has none, comes back to the gate's error.
func (a sessionIDs) Generate() string {
	id := sessionIDPrefix + uuid.NewString()
	if a.store.admit(id) != nil {
		return ""
	}
	return id
}

// Validate takes back a well-formed ID this process does not know rather than
// refusing it. A relay restart forgets every session, and so does idling out;
// the login rides in the bearer, so a client carrying on with its old ID loses
// nothing, where a refusal would leave the client to notice and reconnect by
// itself. Taking one back still needs room.
func (a sessionIDs) Validate(id string) (bool, error) {
	rest, ok := strings.CutPrefix(id, sessionIDPrefix)
	if !ok {
		return false, errors.New("invalid session id")
	}
	if _, err := uuid.Parse(rest); err != nil {
		return false, errors.New("invalid session id")
	}
	return false, a.store.admit(id)
}

// Terminate is a client ending its session with DELETE, or sweep ending it.
func (a sessionIDs) Terminate(id string) (bool, error) {
	a.store.forget(id)
	return false, nil
}

// terminateVia ends a session the way a client does, through the handler
// itself: DELETE is the one path on which mcp-go frees every piece of state it
// keeps for a session.
func terminateVia(h http.Handler) func(id string) {
	return func(id string) {
		req, err := http.NewRequest(http.MethodDelete, "/", nil)
		if err != nil {
			return
		}
		req.Header.Set(server.HeaderKeySessionID, id)
		h.ServeHTTP(discardResponse{header: http.Header{}}, req)
	}
}

type discardResponse struct{ header http.Header }

func (d discardResponse) Header() http.Header         { return d.header }
func (d discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (d discardResponse) WriteHeader(int)             {}
