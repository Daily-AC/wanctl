package portal

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"wanctl/internal/console"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

// The portal dials every device it watches and logs two connects each time,
// so the 200 lines of the activity table were mostly its own reconnects. A run
// of connects from one source becomes one line with a count and a time range;
// a connect from another source, a refused connect, or any other event breaks
// the run, and nothing else is ever merged.
func TestActivityMergesRunsOfConnectsFromOneSource(t *testing.T) {
	s := newTestPortal(relayFor("alice", "pc", transport.Fingerprint([]byte("pc"))))
	withDialer(t, s)
	local, remote := net.Pipe()
	d := newDeviceConn(local)
	defer d.close()
	defer remote.Close()
	s.conns["alice/pc"] = d
	ev := func(ts, typ, fp, decision, detail string) map[string]any {
		e := map[string]any{"ts": "2026-10-09T" + ts + ":00Z", "type": typ, "peer_fp": fp, "peer_name": fp + "-host", "decision": decision}
		if detail != "" {
			e["detail"] = detail
		}
		return e
	}
	events := []map[string]any{
		ev("10:00", "connect", "portal", "accepted", ""),
		ev("10:00", "connect", "portal", "accepted", ""),
		ev("10:05", "connect", "portal", "accepted", ""),
		ev("10:05", "connect", "portal", "accepted", ""),
		ev("10:06", "connect", "laptop", "accepted", ""),
		ev("10:06", "exec", "laptop", "approved", "make test"),
		ev("10:06", "exec", "laptop", "approved", "make test"),
		ev("10:07", "connect", "laptop", "accepted", ""),
		ev("10:08", "connect", "laptop", "rejected:unpaired", ""),
		ev("10:09", "connect", "laptop", "rejected:unpaired", ""),
		ev("10:10", "connect", "portal", "accepted", ""),
	}
	go func() {
		for {
			m, err := protocol.ReadMessage(remote)
			if err != nil {
				return
			}
			switch m.Kind {
			case protocol.KindConsoleState:
				state, _ := json.Marshal(console.State{})
				protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindConsoleState, Data: state})
			case protocol.KindLogs:
				data, _ := json.Marshal(events)
				protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindLogs, Data: data})
			}
		}
	}()
	rec := httptest.NewRecorder()
	s.handleDeviceLogs(rec, userReq("GET", "/api/devices/logs?device=pc", nil))
	if rec.Code != 200 {
		t.Fatalf("logs = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Logs []struct {
			Ts       string `json:"ts"`
			FirstTs  string `json:"first_ts"`
			Count    int    `json:"count"`
			Type     string `json:"type"`
			PeerFP   string `json:"peer_fp"`
			PeerName string `json:"peer_name"`
			Decision string `json:"decision"`
			Detail   string `json:"detail"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	var lines []string
	for _, e := range got.Logs {
		line := e.Ts[11:16] + " " + e.Type + " " + e.PeerFP + " " + e.Decision + " " + e.Detail
		if e.Count > 0 {
			line += " x" + strings.Repeat("|", e.Count) + " from " + e.FirstTs[11:16]
		}
		if e.PeerName != e.PeerFP+"-host" {
			t.Fatalf("a field was lost in the merge: %+v", e)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	want := []string{
		"10:05 connect portal accepted  x|||| from 10:00",
		"10:06 connect laptop accepted",
		"10:06 exec laptop approved make test",
		"10:06 exec laptop approved make test",
		"10:07 connect laptop accepted",
		"10:09 connect laptop rejected:unpaired  x|| from 10:08",
		"10:10 connect portal accepted",
	}
	for i := range want {
		want[i] = strings.TrimSpace(want[i])
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("activity:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// The verdict column shows what the device wrote, and the device writes
// English. Every value the agent can write has a Chinese label in app.js, so
// the Chinese page never shows a raw "accepted". The values are read from the
// agent's source: a new one fails here until it has a label.
func TestEveryAgentVerdictHasAChineseLabel(t *testing.T) {
	js := readWeb(t, "web/app.js")
	block := regexp.MustCompile(`(?s)verdicts: \{(.*?)\n      \}`).FindStringSubmatch(js)
	if block == nil {
		t.Fatal("app.js has no Chinese verdict table")
	}
	labels := map[string]string{}
	for _, m := range regexp.MustCompile(`'([^']+)': '([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		labels[m[1]] = m[2]
	}

	// "remembered:" + scope is built at run time.
	written := map[string]bool{"remembered:" + string(policy.ScopeDir): true, "remembered:" + string(policy.ScopeGlobal): true}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`Decision: "([^"]+)"`),
		regexp.MustCompile(`return (?:true|false), "([^"]+)"`),
		regexp.MustCompile(`decision = (?:true|false), "([^"]+)"`),
		regexp.MustCompile(`"(rejected:[a-z-]+)"`),
	}
	files, err := os.ReadDir("../agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile("../agent/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range patterns {
			for _, m := range p.FindAllStringSubmatch(string(src), -1) {
				// "denied: <reason>" is a prefix; the reason is the device's own text.
				if v := strings.TrimSpace(m[1]); v != "" && !strings.HasPrefix(v, "denied:") && !strings.HasSuffix(v, ":") {
					written[v] = true
				}
			}
		}
	}
	if len(written) < 15 {
		t.Fatalf("found only %d verdicts in the agent; the patterns no longer match its source", len(written))
	}
	var missing []string
	for v := range written {
		if label := labels[v]; label == "" || regexp.MustCompile(`^[\x00-\x7f]*$`).MatchString(label) {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("verdicts the agent writes with no Chinese label in app.js: %s", strings.Join(missing, ", "))
	}
	if labels["denied"] == "" {
		t.Fatal(`"denied: <reason>" is shown with the label of "denied", which is missing`)
	}
}
