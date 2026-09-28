package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wanctl/internal/clientip"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"
)

// Hosted (HTTP) MCP sessions are bounded in number and closed when idle.
//
// Opening one needs no credential — logging in is a tool call made inside a
// session — and each is state in the relay process every tenant shares: mcp-go
// keeps a notification buffer and bookkeeping per session, and this package a
// remoteSession. mcp-go frees its part only when a client ends the session with
// DELETE, which a client that simply goes away never sends, so without these
// bounds anyone could grow the process one initialize at a time.
const (
	// maxHostedSessions bounds the sessions open at once. One costs about
	// 10 KiB here, so this is a few tens of MiB at most.
	maxHostedSessions = 2048
	// maxAnonymousSessionsPerClient bounds the sessions one client address
	// (clientip.Key) holds open without logging in, so that no single caller
	// can take the whole ceiling. A client normally has one or two, and a
	// session stops counting once it logs in. Sessions opened with an OAuth
	// bearer are not counted per address: a hosted AI's connectors all arrive
	// from the same few addresses, and a bearer names an account instead.
	maxAnonymousSessionsPerClient = 32
	// anonymousSessionIdle is how long a session nobody has logged in to is
	// kept without a request. So is one opened with a bearer: its login travels
	// with every request, so closing it loses nothing, and a connector that
	// opens a new session for every tool call would otherwise leave hundreds.
	anonymousSessionIdle = 10 * time.Minute
	// loggedInSessionIdle is how long a session logged in with wanctl_login
	// keeps that login without a request. Past it the model finds the session
	// logged out and restores it with its rebind credential, without the user.
	loggedInSessionIdle = 2 * time.Hour
)

const sessionIDPrefix = "mcp-session-"

var (
	errTooManySessions       = errors.New("too many MCP sessions are open on this server; try again in a few minutes")
	errTooManyClientSessions = errors.New("too many MCP sessions are open from this address without logging in; log in to one you have, or let them expire, then try again")
)

// transportSession is one transport session the endpoint has admitted.
type transportSession struct {
	client   string // clientip.Key of whoever opened it
	bearer   bool   // opened with an OAuth bearer, whose login is not the session's
	lastUsed time.Time
}

func (s *sessionStore) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// loggedInLocked reports whether the session keyed id has logged in with
// wanctl_login. Caller holds s.mu.
func (s *sessionStore) loggedInLocked(id string) bool {
	r := s.m[id]
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token != ""
}

// roomLocked says why one more session for client may not be opened, if it may
// not. Caller holds s.mu.
func (s *sessionStore) roomLocked(client string, bearer bool) error {
	if len(s.open) >= maxHostedSessions {
		return errTooManySessions
	}
	if bearer {
		return nil
	}
	n := 0
	for id, o := range s.open {
		if o.client == client && !o.bearer && !s.loggedInLocked(id) {
			n++
		}
	}
	if n >= maxAnonymousSessionsPerClient {
		return errTooManyClientSessions
	}
	return nil
}

func (s *sessionStore) room(client string, bearer bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roomLocked(client, bearer)
}

// admit records a request on session id, taking it in if it is not already
// open and there is room for it.
func (s *sessionStore) admit(id, client string, bearer bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.open[id]; o != nil {
		o.lastUsed = s.now()
		return nil
	}
	if err := s.roomLocked(client, bearer); err != nil {
		return err
	}
	s.open[id] = &transportSession{client: client, bearer: bearer, lastUsed: s.now()}
	return nil
}

// forget drops everything this package holds for session id.
func (s *sessionStore) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, id)
	delete(s.m, id)
}

// sweep closes the sessions idle past their limit and has mcp-go free its own
// state for each. It also drops login state no open session refers to: an
// OAuth session (keyed by its token, not by a transport session) once that
// token has expired or gone unused.
func (s *sessionStore) sweep() {
	now := s.now()
	var closed []string
	s.mu.Lock()
	for id, o := range s.open {
		idle := anonymousSessionIdle
		if !o.bearer && s.loggedInLocked(id) {
			idle = loggedInSessionIdle
		}
		if now.Sub(o.lastUsed) > idle {
			delete(s.open, id)
			delete(s.m, id)
			closed = append(closed, id)
		}
	}
	for id, r := range s.m {
		if _, open := s.open[id]; open {
			continue
		}
		r.mu.Lock()
		idle := anonymousSessionIdle
		if r.token != "" {
			idle = loggedInSessionIdle
		}
		gone := now.Sub(r.lastUsed) > idle || (r.oauth && !now.Before(r.oauthClaim.Expiry()))
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
			_, bearer := oauthClaimFrom(req.Context())
			if err := s.room(clientip.Key(req), bearer); err != nil {
				refuseSession(w, err)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func refuseSession(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, errTooManyClientSessions) {
		status = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(int(time.Minute/time.Second)))
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": map[string]any{"code": -32000, "message": err.Error()},
	})
}

// sessionIDs hands mcp-go a SessionIdManager that knows who is asking, which a
// plain SessionIdManager cannot: its methods see only the session ID.
type sessionIDs struct{ store *sessionStore }

func (i sessionIDs) ResolveSessionIdManager(req *http.Request) server.SessionIdManager {
	a := sessionAdmission{store: i.store}
	if req != nil {
		a.client = clientip.Key(req)
		_, a.bearer = oauthClaimFrom(req.Context())
	}
	return a
}

type sessionAdmission struct {
	store  *sessionStore
	client string
	bearer bool
}

// Generate names a new session. The gate has already checked there is room;
// if another request took the last place since, the answer is no session at
// all, and the client, finding it has none, comes back to the gate's error.
func (a sessionAdmission) Generate() string {
	id := sessionIDPrefix + uuid.NewString()
	if a.store.admit(id, a.client, a.bearer) != nil {
		return ""
	}
	return id
}

// Validate takes back a well-formed ID this process does not know rather than
// refusing it. A relay restart forgets every session, and so does idling out;
// a client carrying on with its old ID finds a logged-out session, which the
// model's rebind credential repairs, where a refusal would leave the client
// to notice and reconnect by itself. Taking one back still needs room.
func (a sessionAdmission) Validate(id string) (bool, error) {
	rest, ok := strings.CutPrefix(id, sessionIDPrefix)
	if !ok {
		return false, errors.New("invalid session id")
	}
	if _, err := uuid.Parse(rest); err != nil {
		return false, errors.New("invalid session id")
	}
	return false, a.store.admit(id, a.client, a.bearer)
}

// Terminate is a client ending its session with DELETE, or sweep ending it.
func (a sessionAdmission) Terminate(id string) (bool, error) {
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
