package agent

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/elevate"
)

// The adb link (v0.20.2). The app's 提权通道 switch used to read "on" while
// elevation could not work at all — the switch is the owner's consent, not a
// connection — and the portal said "配对成功" once and then nothing. The agent
// is the one party that can tell: it probes the adb channel itself and reports
// the answer to both, so the app and the portal always say the same thing.
//
// It probes when the app reports a new wireless-debugging port (turned on, off
// or moved), right after a pairing, and otherwise once a minute, which also
// notices a pairing Android revoked. A probe on a live connection is one `id`.

// adbLinkLinePrefix starts the stdout line that tells the app the link state.
// Like approvalLinePrefix, nothing a controller chose can start a line with it.
const adbLinkLinePrefix = "wanctl-adb "

const (
	adbLinkPoll  = 5 * time.Second  // how often the discovered port is checked
	adbLinkEvery = 60 * time.Second // the longest a state goes unconfirmed
	// adbLinkErrorEvery spaces probes out while adbd is asking the owner to
	// allow wanctl's key (every probe raises that dialog again) or failing in
	// some other way a minute will not change.
	adbLinkErrorEvery = 5 * time.Minute
)

type adbLinkState struct {
	mu   sync.Mutex
	cur  *console.ADBLink
	kick chan struct{}
}

// adbLink is the last state, for the console snapshot; nil off Android.
func (a *Agent) adbLink() *console.ADBLink {
	a.adb.mu.Lock()
	defer a.adb.mu.Unlock()
	if a.adb.cur == nil {
		return nil
	}
	l := *a.adb.cur
	return &l
}

// kickADBLink asks for a probe now, e.g. after a pairing.
func (a *Agent) kickADBLink() {
	select {
	case a.adb.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) watchADBLink(ctx context.Context) {
	if runtime.GOOS != "android" || a.elevator == nil {
		return
	}
	tick := time.NewTicker(adbLinkPoll)
	defer tick.Stop()
	lastPort, lastOn, kicked := -1, false, true
	var lastProbe time.Time
	every := adbLinkEvery
	for {
		port, on := elevate.DiscoveredPort(), a.elevator.Enabled()
		if kicked || port != lastPort || on != lastOn || time.Since(lastProbe) >= every {
			link := console.ADBLink{State: "off"}
			if st, ok := a.elevator.Fresh(ctx, elevate.KindADB); ok {
				link = console.ADBLink{State: st.Link, Reason: st.Reason}
			}
			a.setADBLink(link)
			lastPort, lastOn, kicked, lastProbe = port, on, false, time.Now()
			every = adbLinkEvery
			if link.State == elevate.LinkConfirm || link.State == elevate.LinkError {
				every = adbLinkErrorEvery
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.adb.kick:
			kicked = true
		}
	}
}

// setADBLink records a state and, when it changed, tells the app.
func (a *Agent) setADBLink(l console.ADBLink) {
	a.adb.mu.Lock()
	changed := a.adb.cur == nil || *a.adb.cur != l
	a.adb.cur = &l
	a.adb.mu.Unlock()
	if !changed || a.phone == nil {
		return
	}
	// The app shows a sentence per state; the reason stays in the portal.
	b, _ := json.Marshal(map[string]string{"state": l.State})
	a.phone.writeLine(append(append([]byte(adbLinkLinePrefix), b...), '\n'))
}
