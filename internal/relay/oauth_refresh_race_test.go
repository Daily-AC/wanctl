package relay

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// refreshBarrier holds every refresh lookup until the second one arrives, so
// both requests read the row as live before either rotates it.
type refreshBarrier struct {
	*oauthBackend
	arrived sync.WaitGroup
}

func (b *refreshBarrier) OAuthRefresh(hash string) (OAuthRefresh, bool, error) {
	row, ok, err := b.oauthBackend.OAuthRefresh(hash)
	b.arrived.Done()
	b.arrived.Wait()
	return row, ok, err
}

// A refresh token is single use even when two requests present it at once:
// only the one whose rotation actually revoked the row gets new tokens.
func TestRefreshTokenRedeemsOnceUnderConcurrency(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "state", "alice")
	rr := formPost(t, r, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "client_secret": {secret}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect}})
	if rr.Code != http.StatusOK {
		t.Fatalf("initial exchange status %d", rr.Code)
	}
	refresh := decode(t, rr)["refresh_token"].(string)
	store := &refreshBarrier{oauthBackend: b}
	store.arrived.Add(2)
	r.oauthStore = store
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			results <- formPost(t, r, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "client_secret": {secret}, "refresh_token": {refresh}}).Code
		}()
	}
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == http.StatusOK {
			success++
		}
	}
	if success != 1 {
		t.Errorf("one refresh token produced %d successful exchanges; want 1", success)
	}
}
