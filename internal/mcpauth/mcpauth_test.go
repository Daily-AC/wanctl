package mcpauth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var seed = []byte(strings.Repeat("s", 32))

func TestAccessTokenRoundTrips(t *testing.T) {
	now := time.Now()
	token, claim, err := SealAccess(seed, "alice", "wanctl_raw", "client-1", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, AccessPrefix) || !IsAccessToken(token) {
		t.Fatalf("token %q is not recognizable as one of ours", token)
	}
	// The relay token is the thing this envelope carries; it must not be
	// readable by anyone who merely has the string.
	if strings.Contains(token, "wanctl_raw") {
		t.Fatal("the sealed token contains the relay token in the clear")
	}
	got, err := OpenAccess(seed, token, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "alice" || got.Token != "wanctl_raw" || got.ClientID != "client-1" || got.JTI != claim.JTI {
		t.Fatalf("claim = %+v", got)
	}
}

func TestAccessTokenFailsClosed(t *testing.T) {
	now := time.Now()
	token, _, _ := SealAccess(seed, "alice", "wanctl_raw", "c", now, time.Hour)

	if _, err := OpenAccess([]byte(strings.Repeat("x", 32)), token, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a different seed opened the token: %v", err)
	}
	if _, err := OpenAccess(seed, token, now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("an expired token opened: %v", err)
	}
	if _, err := OpenAccess(seed, strings.TrimPrefix(token, AccessPrefix), now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a token with no prefix opened: %v", err)
	}
	flipped := token[:len(token)-1] + string(rune(token[len(token)-1]^1))
	if _, err := OpenAccess(seed, flipped, now); err == nil {
		t.Error("a tampered token opened")
	}
}

// The two envelope kinds share a seed but must not be interchangeable: a grant
// is stored in the database and an access token is handed to a client, so one
// being usable as the other would turn a database read into device access.
func TestGrantAndAccessEnvelopesAreNotInterchangeable(t *testing.T) {
	now := time.Now()
	access, _, _ := SealAccess(seed, "alice", "tok", "c", now, time.Hour)
	grant, _, _ := SealGrant(seed, "alice", "tok", "c", now, 30*24*time.Hour)

	if _, err := OpenGrant(seed, access, now); err == nil {
		t.Error("an access token opened as a grant")
	}
	if _, err := OpenAccess(seed, grant, now); err == nil {
		t.Error("a grant opened as an access token")
	}
	if IsAccessToken(grant) {
		t.Error("a grant is mistaken for an access token by prefix")
	}
	got, err := OpenGrant(seed, grant, now)
	if err != nil || got.Token != "tok" {
		t.Fatalf("grant round trip: %+v %v", got, err)
	}
}

func TestSealRefusesEmptyIdentity(t *testing.T) {
	if _, _, err := SealAccess(seed, "", "tok", "c", time.Now(), time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("sealed a token with no namespace: %v", err)
	}
	if _, _, err := SealAccess(seed, "alice", "", "c", time.Now(), time.Hour); !errors.Is(err, ErrInvalid) {
		t.Errorf("sealed a token with no relay token: %v", err)
	}
}
