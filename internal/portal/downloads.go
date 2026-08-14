package portal

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"wanctl/internal/release"
)

// Downloads answers "where do I get wanctl for this machine", which until now
// had no answer inside the product: the URLs existed only in the repository's
// docs, so installing on a phone required knowing a path nobody is shown.
//
// The list is not written down here. It is read from the release manifest the
// relay is serving right now, so this page cannot claim a version that is not
// deployed, and a rollout that changes the artifacts changes the page with no
// edit. A hand-maintained list would drift on exactly the release where it
// matters most.
type Downloads struct {
	Version     string     `json:"version"`
	PublishedAt time.Time  `json:"published_at"`
	Base        string     `json:"base"`       // where the files are, absolute
	InstallSh   string     `json:"install_sh"` // one-line installer, POSIX
	InstallPs1  string     `json:"install_ps1"`
	Artifacts   []Download `json:"artifacts"`
}

type Download struct {
	release.Artifact
	URL string `json:"url"`
}

// downloadsTTL is how long a fetched manifest is reused. Short, because the
// interesting moment is right after a deploy and a stale answer then is the
// one that misleads; long enough that opening the page repeatedly does not
// hammer the relay.
const downloadsTTL = 30 * time.Second

type downloadsCache struct {
	mu        sync.Mutex
	value     *Downloads
	fetchedAt time.Time
}

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.relayURL == "" {
		http.Error(w, "portal not wired", http.StatusServiceUnavailable)
		return
	}

	s.downloads.mu.Lock()
	cached := s.downloads.value
	fresh := cached != nil && time.Since(s.downloads.fetchedAt) < downloadsTTL
	s.downloads.mu.Unlock()
	if fresh {
		writeJSON(w, cached)
		return
	}

	req, _ := http.NewRequestWithContext(r.Context(), "GET", s.relayURL+"/dl/manifest.json", nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A relay serving no release distribution is a real state (it starts
		// that way in development), and it is not this page's job to dress it
		// up as an empty list.
		http.Error(w, "the relay is not serving a release", http.StatusBadGateway)
		return
	}
	var m release.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		http.Error(w, "the relay's release manifest is unreadable", http.StatusBadGateway)
		return
	}

	out := &Downloads{
		Version:     m.Version,
		PublishedAt: m.PublishedAt,
		Base:        s.relayURL + "/dl/",
		InstallSh:   s.relayURL + "/install.sh",
		InstallPs1:  s.relayURL + "/install.ps1",
	}
	for _, a := range m.Artifacts {
		out.Artifacts = append(out.Artifacts, Download{Artifact: a, URL: s.relayURL + "/dl/" + a.Name})
	}

	s.downloads.mu.Lock()
	s.downloads.value, s.downloads.fetchedAt = out, time.Now()
	s.downloads.mu.Unlock()
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
