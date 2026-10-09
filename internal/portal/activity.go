package portal

import (
	"encoding/json"
)

// mergeConnectRuns folds each run of consecutive connect events from one
// source into its newest event, with "count" and "first_ts" added. The portal
// itself dials every device it watches and logs two connects each time, so
// without this the 200 lines the activity table shows were mostly the
// portal's own reconnects. Every other event stays a line of its own.
//
// raw is the device's JSON array of eventlog.Event, oldest first. Fields are
// passed through as the device wrote them.
func mergeConnectRuns(raw json.RawMessage) (json.RawMessage, error) {
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, err
	}
	str := func(e map[string]json.RawMessage, k string) string {
		var s string
		json.Unmarshal(e[k], &s)
		return s
	}
	// A source is the controller as the device authenticated it, and the
	// outcome: an accepted connect never absorbs a refused one.
	source := func(e map[string]json.RawMessage) []string {
		return []string{str(e, "peer_fp"), str(e, "peer_name"), str(e, "decision"), str(e, "grant_id"), str(e, "credential_id")}
	}
	same := func(a, b []string) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	out := make([]map[string]json.RawMessage, 0, len(events))
	var runSource []string
	var runCount int
	var runFirst json.RawMessage
	for _, e := range events {
		if str(e, "type") != "connect" {
			out = append(out, e)
			runSource = nil
			continue
		}
		src := source(e)
		if runSource != nil && same(src, runSource) {
			runCount++
			count, _ := json.Marshal(runCount)
			e["count"], e["first_ts"] = count, runFirst
			out[len(out)-1] = e
			continue
		}
		out = append(out, e)
		runSource, runCount, runFirst = src, 1, e["ts"]
	}
	return json.Marshal(out)
}
