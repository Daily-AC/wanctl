// Package delegation describes temporary device-use grants. Device policy is
// still authoritative for individual operations; grants add no action matrix.
package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

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
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	Devices               []Device   `json:"devices,omitempty"`
	TokenID               int        `json:"token_id,omitempty"`
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
	// BeginJob atomically records an operation before execution. Existing IDs
	// return new=false and cannot be executed again, including after restart.
	BeginJob(context.Context, string, string, string, json.RawMessage) (Job, bool, error)
	FinishJob(context.Context, string, string, string, json.RawMessage) error
	GetJob(context.Context, string, string) (Job, error)
}
