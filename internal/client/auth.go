package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/config"
)

// ResolveTokenNamespace asks the relay to authenticate token and return the
// namespace it actually resolves to. Callers must not derive identity or
// authorization state from a namespace bundled alongside a bearer token.
func ResolveTokenNamespace(ctx context.Context, relayURL, token, transport string) (string, error) {
	path := "/peers"
	if transport == "http" {
		path = "/h/peers"
	}
	base, err := config.RelayHTTPOrigin(relayURL)
	if err != nil {
		return "", err
	}
	u := base + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	// This was wanctl's last internal caller of the deprecated ?token= form.
	// Keep credentials out of URLs and proxy/access logs.
	admission.SetBearer(req, token)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve token namespace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relay rejected rebind token (status %d)", resp.StatusCode)
	}
	var out struct{ Namespace string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token namespace: %w", err)
	}
	if out.Namespace == "" {
		return "", fmt.Errorf("relay returned an empty namespace")
	}
	return out.Namespace, nil
}
