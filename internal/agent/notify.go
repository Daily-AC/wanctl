package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/console"
	"wanctl/internal/eventlog"
)

const (
	notifyPolicyRefresh = 5 * time.Minute
	notifyPolicyRetry   = 2 * time.Second
)

type agentNotifyPolicy struct {
	IncludeDetail bool `json:"include_detail"`
}

type agentEventReport struct {
	ID     string    `json:"id"`
	Event  string    `json:"event"`
	TS     time.Time `json:"ts"`
	Detail string    `json:"detail,omitempty"`
	Peer   string    `json:"peer,omitempty"`
	Exit   *int      `json:"exit,omitempty"`
}

func (a *Agent) refreshNotifyPolicy(ctx context.Context) error {
	q := url.Values{"device": {a.opts.Name}, "inst": {a.inst}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpBase(a.opts.RelayURL)+"/agent/notify-policy?"+q, nil)
	if err != nil {
		return err
	}
	admission.SetBearer(req, a.opts.Token)
	resp, err := a.notifyHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("notify policy returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var policy agentNotifyPolicy
	if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
		return err
	}
	a.notifyMu.Lock()
	a.notifyPolicy = policy
	a.notifyMu.Unlock()
	return nil
}

func (a *Agent) runNotifyPolicy(ctx context.Context) {
	wait := time.Duration(0)
	for {
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		err := a.refreshNotifyPolicy(ctx)
		if err != nil {
			// Detail is a privacy expansion. A stale true value after a failed
			// refresh could leak a command after the owner disabled the switch, so
			// policy refresh fails closed even though event reporting remains live.
			a.notifyMu.Lock()
			a.notifyPolicy.IncludeDetail = false
			a.notifyMu.Unlock()
			wait = notifyPolicyRetry
			continue
		}
		wait = notifyPolicyRefresh
	}
}

func (a *Agent) notifyHTTPClient() *http.Client {
	if a.notifyClient != nil {
		return a.notifyClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (a *Agent) reportNotify(report agentEventReport) {
	a.notifyMu.RLock()
	includeDetail := a.notifyPolicy.IncludeDetail
	a.notifyMu.RUnlock()
	if !includeDetail {
		// Keep the event type, device (bound in the URL), timestamp, and exit
		// status, but remove all device content before serialization. This keeps
		// eventlog's "relay never sees this" property true on the default path.
		report.Detail = ""
		report.Peer = ""
	} else {
		report.Detail = eventlog.RedactText(report.Detail)
		report.Peer = eventlog.RedactText(report.Peer)
	}
	body, err := json.Marshal(report)
	if err != nil {
		return
	}
	go a.postNotifyEvent(body, report.Event)
}

func (a *Agent) postNotifyEvent(body []byte, event string) {
	q := url.Values{"device": {a.opts.Name}, "inst": {a.inst}}.Encode()
	target := httpBase(a.opts.RelayURL) + "/agent/events?" + q
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			admission.SetBearer(req, a.opts.Token)
			resp, doErr := a.notifyHTTPClient().Do(req)
			if doErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					cancel()
					return
				}
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
					cancel()
					break
				}
			} else {
				lastErr = doErr
			}
		} else {
			lastErr = err
		}
		cancel()
		if attempt < 2 {
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
		}
	}
	fmt.Fprintf(os.Stderr, "wanctl: report webhook event %s: %v\n", event, lastErr)
}

func randomEventID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + ":" + fmt.Sprint(time.Now().UnixNano())
	}
	return prefix + ":" + hex.EncodeToString(b[:])
}

func stableEventID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + ":" + hex.EncodeToString(sum[:16])
}

func (a *Agent) notifyApproval(p console.Pending) {
	detail := p.Cmd
	if detail == "" {
		detail = p.Path
	}
	a.reportNotify(agentEventReport{
		ID: stableEventID("approval", p.ID), Event: "approval.pending", TS: p.Created,
		Detail: detail, Peer: p.Peer,
	})
}

func (a *Agent) notifyPairing(p console.PendingPairing) {
	detail := strings.TrimSpace(strings.Join([]string{p.Name, p.Label, p.FP}, " "))
	a.reportNotify(agentEventReport{
		ID:    stableEventID("pairing", p.FP+"/"+p.Created.UTC().Format(time.RFC3339Nano)),
		Event: "pairing.requested", TS: p.Created, Detail: detail, Peer: p.Name,
	})
}

func (a *Agent) notifyTrustChanged(fp, name, action string) {
	a.reportNotify(agentEventReport{
		ID: randomEventID("trust"), Event: "trust.changed", TS: time.Now().UTC(),
		Detail: action + " " + fp, Peer: name,
	})
}

func (a *Agent) notifyExecFinished(command, cwd, peer string, code int) {
	exit := code
	detail := command
	if cwd != "" {
		detail += " (cwd " + cwd + ")"
	}
	a.reportNotify(agentEventReport{
		ID: randomEventID("exec"), Event: "exec.finished", TS: time.Now().UTC(),
		Detail: detail, Peer: peer, Exit: &exit,
	})
}
