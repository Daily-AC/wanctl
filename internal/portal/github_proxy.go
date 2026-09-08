package portal

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GitHubProxyTransport keeps a deployment's OAuth egress separate from relay,
// notifications and console traffic. An empty value preserves normal routing.
func GitHubProxyTransport(raw string) (http.RoundTripper, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("WANCTL_GITHUB_PROXY must be an http(s) or socks5 proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("WANCTL_GITHUB_PROXY has an unsupported scheme")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	return tr, nil
}

func (s *Server) githubHTTP() *http.Client {
	if s.ghc != nil {
		return s.ghc
	}
	return s.hc
}
