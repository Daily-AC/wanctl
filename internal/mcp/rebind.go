package mcp

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
	rebindPrefix   = "wrb1."
	rebindVersion  = 1
	rebindAudience = "wanctl-mcp-rebind"
	rebindTTL      = 7 * 24 * time.Hour
)

var (
	ErrLegacyRebind  = errors.New("legacy rebind credential is not accepted")
	ErrInvalidRebind = errors.New("invalid rebind credential")
	ErrExpiredRebind = errors.New("expired rebind credential")
	ErrRevokedRebind = errors.New("revoked rebind credential")
)

type rebindClaim struct {
	Version   int    `json:"v"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
	JTI       string `json:"jti"`
	Namespace string `json:"ns"`
	Token     string `json:"token"`
}

func rebindAEAD(seed []byte) (cipher.AEAD, error) {
	h := hkdf.New(sha256.New, seed, nil, []byte("wanctl-mcp:rebind:aead:v1"))
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

func sealRebind(seed []byte, namespace, token string, now time.Time) (string, rebindClaim, error) {
	if namespace == "" || token == "" {
		return "", rebindClaim{}, fmt.Errorf("%w: empty namespace or token", ErrInvalidRebind)
	}
	var jtiBytes [16]byte
	if _, err := rand.Read(jtiBytes[:]); err != nil {
		return "", rebindClaim{}, err
	}
	claim := rebindClaim{
		Version:   rebindVersion,
		Audience:  rebindAudience,
		ExpiresAt: now.Add(rebindTTL).Unix(),
		JTI:       hex.EncodeToString(jtiBytes[:]),
		Namespace: namespace,
		Token:     token,
	}
	plaintext, err := json.Marshal(claim)
	if err != nil {
		return "", rebindClaim{}, err
	}
	aead, err := rebindAEAD(seed)
	if err != nil {
		return "", rebindClaim{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", rebindClaim{}, err
	}
	sealed := aead.Seal(nil, nonce, plaintext, []byte(rebindPrefix))
	payload := append(nonce, sealed...)
	return rebindPrefix + base64.RawURLEncoding.EncodeToString(payload), claim, nil
}

func openRebind(seed []byte, credential string, now time.Time) (rebindClaim, error) {
	credential = strings.TrimSpace(credential)
	if !strings.HasPrefix(credential, rebindPrefix) {
		return rebindClaim{}, ErrLegacyRebind
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(credential, rebindPrefix))
	if err != nil {
		return rebindClaim{}, fmt.Errorf("%w: malformed envelope", ErrInvalidRebind)
	}
	aead, err := rebindAEAD(seed)
	if err != nil {
		return rebindClaim{}, err
	}
	if len(payload) < aead.NonceSize()+aead.Overhead() {
		return rebindClaim{}, fmt.Errorf("%w: short envelope", ErrInvalidRebind)
	}
	nonce, ciphertext := payload[:aead.NonceSize()], payload[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(rebindPrefix))
	if err != nil {
		return rebindClaim{}, fmt.Errorf("%w: authentication failed", ErrInvalidRebind)
	}
	var claim rebindClaim
	if err := json.Unmarshal(plaintext, &claim); err != nil {
		return rebindClaim{}, fmt.Errorf("%w: malformed claim", ErrInvalidRebind)
	}
	if claim.Version != rebindVersion || claim.Audience != rebindAudience || claim.JTI == "" || claim.Namespace == "" || claim.Token == "" {
		return rebindClaim{}, fmt.Errorf("%w: unsupported claim", ErrInvalidRebind)
	}
	if claim.ExpiresAt <= now.Unix() {
		return rebindClaim{}, ErrExpiredRebind
	}
	return claim, nil
}
