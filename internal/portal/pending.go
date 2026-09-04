package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// waitingItem is one thing waiting for a human, flattened across devices.
// It carries either an approval (ID/Kind/Cmd/Path) or a pairing request (FP);
// the caller tells them apart by which one is set.
type waitingItem struct {
	Device string `json:"device"`

	// approval
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
	Path string `json:"path,omitempty"`
	Cwd  string `json:"cwd,omitempty"`
	Peer string `json:"peer,omitempty"`

	// pairing
	FP    string `json:"fp,omitempty"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"`

	Created time.Time `json:"created"`
}

// perDeviceWait bounds one unreachable machine's effect on the whole answer.
// A device the relay still calls online can be gone (killed, suspended, network
// dropped) and its dial will hang until something times out; without this the
// slowest device sets the latency of the entire main screen.
const perDeviceWait = 4 * time.Second

// handleWaiting fans out over the caller's own online devices and returns
// everything waiting on a decision.
//
// Why this exists: the product's claim is that nothing runs until you nod, but
// until now the portal opened on a list of machines and buried the requests one
// level down, inside whichever device happened to be asking. You had to guess
// where to look. This lets the main screen answer "is anyone waiting on me"
// directly.
//
// Why it is server-side: the portal already holds a console connection per
// online device, so fanning out here costs one RPC each over links that are
// already up. Doing it from the browser would mean N HTTP round trips through
// the relay on every poll.
//
// Shared devices are excluded on purpose, matching requireOwnedConsole: an
// ACL grant carries the right to use a device, never the right to read or
// answer its owner's approvals.
//
// There is deliberately no cache. A stale entry here would resurrect a card the
// user just answered — the client re-reads this endpoint 340ms after a verdict —
// and a card that comes back from the dead reads as "my click did nothing".
func (s *Server) handleWaiting(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}

	resp, err := s.adminReq("GET", "/admin/devices", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "relay admin error", resp.StatusCode)
		return
	}
	var list struct {
		Devices []struct {
			Name   string `json:"name"`
			Owner  string `json:"owner"`
			Shared bool   `json:"shared"`
			Online bool   `json:"online"`
		} `json:"devices"`
	}
	json.NewDecoder(resp.Body).Decode(&list)

	var names []string
	seen := map[string]bool{}
	for _, d := range list.Devices {
		if !d.Online || d.Shared || (d.Owner != "" && d.Owner != ns) || seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		names = append(names, d.Name)
	}

	var (
		mu    sync.Mutex
		items = []waitingItem{}
		wg    sync.WaitGroup
	)
	for _, name := range names {
		wg.Add(1)
		go func(device string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), perDeviceWait)
			defer cancel()
			d, err := s.deviceConnFor(ctx, ns, device)
			if err != nil {
				return // offline or not yet trusted; nothing to report, not an error
			}
			st, err := d.state()
			if err != nil {
				s.dropConn(ns, device)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range st.Pending {
				items = append(items, waitingItem{
					Device: device, ID: p.ID, Kind: p.Kind, Cmd: p.Cmd,
					Path: p.Path, Cwd: p.Cwd, Peer: p.Peer, Created: p.Created,
				})
			}
			for _, p := range st.PendingPairings {
				items = append(items, waitingItem{
					Device: device, FP: p.FP, Name: p.Name, Label: p.Label, Created: p.Created,
				})
			}
		}(name)
	}
	wg.Wait()

	// Oldest first: whoever has been blocked longest goes on top.
	sort.SliceStable(items, func(i, j int) bool { return items[i].Created.Before(items[j].Created) })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Items []waitingItem `json:"items"`
	}{items})
}
