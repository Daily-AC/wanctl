package portal

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Release is one entry of the user-facing changelog.
type Release struct {
	Version string `json:"version"` // "v0.1.5"
	Body    string `json:"body"`    // markdown, rendered by the SPA
}

var (
	changelogOnce sync.Once
	changelogList []Release
)

// releases returns every embedded release note, newest first. Parsed once: the
// set cannot change without a redeploy.
func releases() []Release {
	changelogOnce.Do(func() {
		entries, err := fs.ReadDir(changelogFS, "changelog")
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			body, err := fs.ReadFile(changelogFS, "changelog/"+e.Name())
			if err != nil {
				continue
			}
			changelogList = append(changelogList, Release{
				Version: strings.TrimSuffix(e.Name(), ".md"),
				Body:    string(body),
			})
		}
		sort.Slice(changelogList, func(i, j int) bool {
			return compareVersions(changelogList[i].Version, changelogList[j].Version) > 0
		})
	})
	return changelogList
}

// compareVersions orders vMAJOR.MINOR.PATCH numerically. Sorting these as
// strings would put v0.1.10 before v0.1.9, which is exactly the point at which
// nobody is watching the changelog page any more.
func compareVersions(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionParts(v string) []int {
	fields := strings.Split(strings.TrimPrefix(v, "v"), ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// CurrentVersion is the newest release the deployed portal knows about.
func CurrentVersion() string {
	if rs := releases(); len(rs) > 0 {
		return rs[0].Version
	}
	return ""
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"current":  CurrentVersion(),
		"releases": releases(),
	})
}
