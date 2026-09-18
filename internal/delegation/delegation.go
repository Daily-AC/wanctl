// Package delegation describes temporary device-use grants. Device policy is
// still authoritative for individual operations; grants add no action matrix.
package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// MaxGrantMinutes is the longest access an owner may approve. A full day,
// because the grant now has to outlive the work rather than the conversation:
// a web AI starts a build, a render or an install, the human closes the tab and
// comes back in the same chat hours later to read the result. Everything that
// used to restate "a grant lives at most an hour" — the browser ticket
// envelope, the per-grant job allowance, the retention floor, the manifest —
// derives from this one number instead.
const MaxGrantMinutes = 1440

// RequestWindow is how long an unapproved browser ticket may still create or
// hold its pending request. It is also the slack between a ticket's timestamp
// and the earliest moment a grant approved on it can start running.
const RequestWindow = 10 * time.Minute

// TicketLifetime bounds a browser ticket's own timestamp. A ticket has to stay
// readable for as long as the grant it may carry: the request can be created up
// to RequestWindow after the ticket was issued, and the owner may then approve
// MaxGrantMinutes on it. Anything shorter would expire the session URL out from
// under a grant that is still valid, which is exactly the long-task case.
const TicketLifetime = RequestWindow + MaxGrantMinutes*time.Minute

// MinRetention is the floor for deleting inactive delegation records. Deletion
// may never free a request ID that a live ticket can still name, and retention
// is measured from the request row's own created_at — at worst RequestWindow
// after the ticket was issued, so the row must survive MaxGrantMinutes from
// there. An extra hour keeps that strictly, not exactly, satisfied.
const MinRetention = MaxGrantMinutes*time.Minute + time.Hour

// Job ledger allowance. The old flat 64 was sized for a one-hour grant; kept
// flat, a day-long grant would have spent it in the first hour and then refused
// every further operation while still being valid. The rate is therefore per
// approved hour, and the old number survives as the floor so that short grants
// are unchanged.
const (
	JobsPerGrantHour = 64
	MinJobsPerGrant  = 64
)

// MaxJobs is the number of jobs one grant's ledger accepts, the single place
// that turns an approved duration into that allowance. Partial hours round up:
// a 15-minute grant and a 61-minute one are both charged a whole hour, so the
// owner never has to reason about the boundary.
func MaxJobs(granted time.Duration) int {
	hours := int((granted + time.Hour - 1) / time.Hour)
	if n := hours * JobsPerGrantHour; n > MinJobsPerGrant {
		return n
	}
	return MinJobsPerGrant
}

var (
	ErrNotFound  = errors.New("delegation not found")
	ErrForbidden = errors.New("delegation forbidden")
	ErrConflict  = errors.New("delegation conflict")
	ErrExpired   = errors.New("delegation expired")
	ErrInvalid   = errors.New("invalid delegation")
	ErrLimit     = errors.New("delegation limit exceeded")
)

type Device struct {
	Namespace   string `json:"namespace"`
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

func (d Device) Target() string { return d.Namespace + "/" + d.ID }

// Access is admission metadata, never the bearer credential itself.
type Access struct {
	Namespace             string    `json:"namespace"`
	CredentialID          string    `json:"credential_id"`
	Delegated             bool      `json:"delegated"`
	GrantID               string    `json:"grant_id,omitempty"`
	ExpiresAt             time.Time `json:"expires_at,omitempty"`
	Devices               []Device  `json:"devices,omitempty"`
	ControllerFingerprint string    `json:"controller_fingerprint,omitempty"`
	// GrantedMinutes is the whole window the owner approved, not what is left
	// of it. The job allowance is a property of the approval, so it must not
	// shrink as the grant runs down.
	GrantedMinutes int `json:"granted_minutes,omitempty"`
}

func (a Access) Allows(target string) bool {
	if !a.Delegated {
		return true
	}
	if a.ExpiresAt.IsZero() || !time.Now().Before(a.ExpiresAt) {
		return false
	}
	for _, d := range a.Devices {
		if d.Target() == target {
			return true
		}
	}
	return false
}

type Request struct {
	ID                    string     `json:"id"`
	Label                 string     `json:"label"`
	ControllerFingerprint string     `json:"controller_fingerprint"`
	Status                string     `json:"status"`
	Namespace             string     `json:"namespace,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	RequestExpiresAt      time.Time  `json:"request_expires_at"`
	DecidedAt             *time.Time `json:"decided_at,omitempty"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	Devices               []Device   `json:"devices,omitempty"`
	TokenID               int        `json:"token_id,omitempty"`
}

// GrantedDuration is the window the owner approved: the decision instant to the
// token's expiry, both written by the same transaction. It is zero unless the
// request was approved, which is the only state that has a duration at all.
func (r Request) GrantedDuration() time.Duration {
	if r.DecidedAt == nil || r.ExpiresAt == nil {
		return 0
	}
	return r.ExpiresAt.Sub(*r.DecidedAt)
}

// NewRequest contains hashes only. The adapter derives the relay credential
// from its seed and the browser ticket; neither plaintext is stored in Postgres.
type NewRequest struct {
	ID                    string
	TicketHash            string
	TokenHash             string
	Label                 string
	ControllerFingerprint string
	RequestExpiresAt      time.Time
}

type Approval struct {
	RequestID             string            `json:"request_id"`
	Namespace             string            `json:"namespace"`
	Devices               []string          `json:"devices"`
	Minutes               int               `json:"minutes"`
	ControllerFingerprint string            `json:"controller_fingerprint"`
	DeviceFingerprints    map[string]string `json:"device_fingerprints"`
}

type Store interface {
	CreateDelegation(context.Context, NewRequest) (Request, error)
	GetDelegation(context.Context, string) (Request, error)
	GetDelegationByTicket(context.Context, string, string) (Request, error)
	ApproveDelegation(context.Context, Approval) (Request, error)
	RejectDelegation(context.Context, string, string) error
	ResolveAccess(string) (Access, bool)
}

// Jobs are an execution ledger, not an authorization source. The adapter must
// resolve the live grant before dispatch or returning any result.
type Job struct {
	ID          string          `json:"id"`
	GrantID     string          `json:"grant_id"`
	RequestID   string          `json:"request_id"`
	PayloadHash string          `json:"-"`
	Payload     json.RawMessage `json:"-"`
	State       string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type JobStore interface {
	// FindJob reads the job a (grant, rid) pair already names, without creating
	// one. A client recovering a lost response has to be able to find its job
	// even when the adapter cannot start anything new.
	FindJob(ctx context.Context, grant, rid string) (Job, error)
	// BeginJob atomically records an operation before execution. Existing IDs
	// return new=false and cannot be executed again, including after restart.
	BeginJob(context.Context, string, string, string, json.RawMessage) (Job, bool, error)
	FinishJob(context.Context, string, string, string, json.RawMessage) error
	GetJob(context.Context, string, string) (Job, error)
}
