package lark

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

const expiredGrantRetention = 5 * time.Minute

// Sentinel errors let callback handlers distinguish authorization failures.
var (
	ErrInvalidGrant   = errors.New("invalid lark grant")
	ErrGrantNotFound  = errors.New("lark grant not found")
	ErrChatMismatch   = errors.New("lark grant chat mismatch")
	ErrNonceConsumed  = errors.New("lark grant nonce already consumed")
	ErrGrantExpired   = errors.New("lark grant expired")
	ErrGrantUnbound   = errors.New("lark grant is not bound to a chat")
	ErrEventDuplicate = errors.New("lark callback event already processed")
)

// Grant binds one card action to its device-side target and original 1:1 chat.
type Grant struct {
	Nonce     string    `json:"nonce"`
	NS        string    `json:"ns"`
	Device    string    `json:"device"`
	PendingID string    `json:"pending_id,omitempty"`
	PairingFP string    `json:"pairing_fp,omitempty"`
	ChatID    string    `json:"chat_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Grants is an in-memory, concurrency-safe one-shot authorization store.
type Grants struct {
	mu       sync.Mutex
	now      func() time.Time
	active   map[string]Grant
	consumed map[string]time.Time
	expired  map[string]time.Time
	events   map[string]time.Time
}

// NewGrants creates an empty authorization store.
func NewGrants() *Grants {
	return &Grants{
		now:      time.Now,
		active:   make(map[string]Grant),
		consumed: make(map[string]time.Time),
		expired:  make(map[string]time.Time),
		events:   make(map[string]time.Time),
	}
}

// Issue creates a one-shot grant. Exactly one of pendingID and pairingFP must
// identify the target. chatID may be empty until SendCard returns; callers must
// then call BindChat before the grant can be consumed.
func (g *Grants) Issue(ns, device, pendingID, pairingFP, chatID string, ttl time.Duration) (Grant, error) {
	if ns == "" || device == "" || ttl <= 0 || (pendingID == "") == (pairingFP == "") {
		return Grant{}, ErrInvalidGrant
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return Grant{}, fmt.Errorf("generate lark grant nonce: %w", err)
	}
	now := g.currentTime()
	record := Grant{
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
		NS:        ns,
		Device:    device,
		PendingID: pendingID,
		PairingFP: pairingFP,
		ChatID:    chatID,
		ExpiresAt: now.Add(ttl),
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	g.cleanupLocked(now)
	g.active[record.Nonce] = record
	return record, nil
}

// BindChat attaches the chat returned by SendCard to a pre-issued grant. A
// grant already bound to another chat cannot be rebound.
func (g *Grants) BindChat(nonce, chatID string) (Grant, error) {
	if nonce == "" || chatID == "" {
		return Grant{}, ErrInvalidGrant
	}
	now := g.currentTime()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	record, exists := g.active[nonce]
	g.cleanupLocked(now)
	if !exists {
		if _, spent := g.consumed[nonce]; spent {
			return Grant{}, ErrNonceConsumed
		}
		if _, expired := g.expired[nonce]; expired {
			return Grant{}, ErrGrantExpired
		}
		return Grant{}, ErrGrantNotFound
	}
	if !now.Before(record.ExpiresAt) {
		return Grant{}, ErrGrantExpired
	}
	if record.ChatID != "" && record.ChatID != chatID {
		return Grant{}, ErrChatMismatch
	}
	record.ChatID = chatID
	g.active[nonce] = record
	return record, nil
}

// Consume validates and atomically spends a grant for one callback event.
func (g *Grants) Consume(nonce, chatID, eventID string) (Grant, error) {
	if nonce == "" || chatID == "" || eventID == "" {
		return Grant{}, ErrInvalidGrant
	}
	now := g.currentTime()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	record, exists := g.active[nonce]
	g.cleanupLocked(now)
	if _, exists := g.events[eventID]; exists {
		return Grant{}, ErrEventDuplicate
	}
	if !exists {
		if _, spent := g.consumed[nonce]; spent {
			return Grant{}, ErrNonceConsumed
		}
		if _, expired := g.expired[nonce]; expired {
			return Grant{}, ErrGrantExpired
		}
		return Grant{}, ErrGrantNotFound
	}
	if !now.Before(record.ExpiresAt) {
		delete(g.active, nonce)
		return Grant{}, ErrGrantExpired
	}
	if record.ChatID == "" {
		return Grant{}, ErrGrantUnbound
	}
	g.events[eventID] = record.ExpiresAt
	if record.ChatID != chatID {
		return Grant{}, ErrChatMismatch
	}
	delete(g.active, nonce)
	g.consumed[nonce] = record.ExpiresAt
	return record, nil
}

func (g *Grants) currentTime() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *Grants) initLocked() {
	if g.active == nil {
		g.active = make(map[string]Grant)
	}
	if g.consumed == nil {
		g.consumed = make(map[string]time.Time)
	}
	if g.expired == nil {
		g.expired = make(map[string]time.Time)
	}
	if g.events == nil {
		g.events = make(map[string]time.Time)
	}
}

func (g *Grants) cleanupLocked(now time.Time) {
	for nonce, record := range g.active {
		if !now.Before(record.ExpiresAt) {
			delete(g.active, nonce)
			g.expired[nonce] = now.Add(expiredGrantRetention)
		}
	}
	for nonce, expiresAt := range g.consumed {
		if !now.Before(expiresAt) {
			delete(g.consumed, nonce)
		}
	}
	for eventID, expiresAt := range g.events {
		if !now.Before(expiresAt) {
			delete(g.events, eventID)
		}
	}
	for nonce, removeAt := range g.expired {
		if !now.Before(removeAt) {
			delete(g.expired, nonce)
		}
	}
}
