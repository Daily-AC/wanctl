// Package mcpauth holds the sealed credentials the hosted MCP endpoint's
// OAuth 2.1 flow hands out. It exists as its own package because two packages
// need the same format and neither may import the other: the relay mints these
// (internal/relay, which owns the database and the admin API) and the MCP
// server verifies them (internal/mcp, which owns the tools).
//
// The format is the one internal/mcp already uses for rebind credentials: an
// AES-GCM envelope whose key is HKDF-derived from the relay's MCP seed. That
// matters more than it sounds. It means an OAuth access token needs no new
// key, no new rotation story and no server-side lookup to be useful: the MCP
// server opens it and has the namespace and the relay token in hand. Rotating
// WANCTL_MCP_SEED still invalidates every credential of every kind at once,
// which is the property the deployment docs already promise.
package mcpauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const (
	// AccessPrefix marks a bearer token a client sends as
	// `Authorization: Bearer woa1.…`. The prefix is what lets the MCP endpoint
	// tell an OAuth bearer apart from anything else a client might send, and
	// what lets a leaked string be recognized in a log.
	AccessPrefix = "woa1."
	// GrantPrefix marks the long-lived envelope the relay stores next to a
	// refresh token. It never leaves the relay.
	GrantPrefix = "wog1."

	accessAudience = "wanctl-mcp-oauth"
	grantAudience  = "wanctl-mcp-oauth-grant"
	version        = 1
)

var (
	ErrInvalid = errors.New("invalid credential")
	ErrExpired = errors.New("expired credential")
)

// Claim is what a sealed credential carries. Token is the relay namespace
// token the MCP session will dial with, so the envelope is as sensitive as the
// token itself — which is why it is sealed rather than signed.
type Claim struct {
	Version   int    `json:"v"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	JTI       string `json:"jti"`
	Namespace string `json:"ns"`
	Token     string `json:"token"`
	ClientID  string `json:"cid"`
}

// Expiry is the claim's expiry as a time.
func (c Claim) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0) }

func aead(seed []byte, info string) (cipher.AEAD, error) {
	h := hkdf.New(sha256.New, seed, nil, []byte(info))
	key := make([]byte, 32)
	if _, err := io.ReadFull(h, key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(seed []byte, prefix, audience, info, namespace, token, clientID string, now time.Time, ttl time.Duration) (string, Claim, error) {
	if namespace == "" || token == "" {
		return "", Claim{}, fmt.Errorf("%w: empty namespace or token", ErrInvalid)
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", Claim{}, err
	}
	claim := Claim{
		Version:   version,
		Audience:  audience,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		JTI:       hex.EncodeToString(jti[:]),
		Namespace: namespace,
		Token:     token,
		ClientID:  clientID,
	}
	plaintext, err := json.Marshal(claim)
	if err != nil {
		return "", Claim{}, err
	}
	a, err := aead(seed, info)
	if err != nil {
		return "", Claim{}, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", Claim{}, err
	}
	// The prefix is authenticated data, so an access envelope cannot be
	// replayed as a grant envelope even under the same derived key.
	sealed := a.Seal(nil, nonce, plaintext, []byte(prefix))
	return prefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), claim, nil
}

func open(seed []byte, prefix, audience, info, credential string, now time.Time) (Claim, error) {
	credential = strings.TrimSpace(credential)
	if !strings.HasPrefix(credential, prefix) {
		return Claim{}, fmt.Errorf("%w: wrong prefix", ErrInvalid)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(credential, prefix))
	if err != nil {
		return Claim{}, fmt.Errorf("%w: malformed envelope", ErrInvalid)
	}
	a, err := aead(seed, info)
	if err != nil {
		return Claim{}, err
	}
	if len(payload) < a.NonceSize()+a.Overhead() {
		return Claim{}, fmt.Errorf("%w: short envelope", ErrInvalid)
	}
	nonce, ciphertext := payload[:a.NonceSize()], payload[a.NonceSize():]
	plaintext, err := a.Open(nil, nonce, ciphertext, []byte(prefix))
	if err != nil {
		return Claim{}, fmt.Errorf("%w: authentication failed", ErrInvalid)
	}
	var claim Claim
	if err := json.Unmarshal(plaintext, &claim); err != nil {
		return Claim{}, fmt.Errorf("%w: malformed claim", ErrInvalid)
	}
	if claim.Version != version || claim.Audience != audience || claim.JTI == "" || claim.Namespace == "" || claim.Token == "" {
		return Claim{}, fmt.Errorf("%w: unsupported claim", ErrInvalid)
	}
	if claim.ExpiresAt <= now.Unix() {
		return Claim{}, ErrExpired
	}
	return claim, nil
}

const accessInfo = "wanctl-mcp:oauth:access:aead:v1"
const grantInfo = "wanctl-mcp:oauth:grant:aead:v1"

// SealAccess mints the bearer token a client will send on every MCP request.
func SealAccess(seed []byte, namespace, token, clientID string, now time.Time, ttl time.Duration) (string, Claim, error) {
	return seal(seed, AccessPrefix, accessAudience, accessInfo, namespace, token, clientID, now, ttl)
}

// OpenAccess verifies a bearer token and returns what it carries.
func OpenAccess(seed []byte, credential string, now time.Time) (Claim, error) {
	return open(seed, AccessPrefix, accessAudience, accessInfo, credential, now)
}

// SealGrant wraps the relay token for storage beside a refresh token, so the
// database holds a ciphertext rather than a live credential. Whoever reads the
// database still needs WANCTL_MCP_SEED, which lives only in the relay's
// environment.
func SealGrant(seed []byte, namespace, token, clientID string, now time.Time, ttl time.Duration) (string, Claim, error) {
	return seal(seed, GrantPrefix, grantAudience, grantInfo, namespace, token, clientID, now, ttl)
}

// OpenGrant unwraps a stored grant.
func OpenGrant(seed []byte, credential string, now time.Time) (Claim, error) {
	return open(seed, GrantPrefix, grantAudience, grantInfo, credential, now)
}

// IsAccessToken reports whether a bearer string is shaped like one of ours.
// A client that sends some other bearer gets told to authorize rather than
// having its string decrypted and rejected as corrupt.
func IsAccessToken(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), AccessPrefix) }
