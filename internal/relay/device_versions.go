package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"time"

	"wanctl/internal/release"
	"wanctl/internal/versionmeta"
)

const maxDeviceVersionBytes = versionmeta.MaxBytes
const deviceVersionWriteTimeout = 5 * time.Second
const deviceVersionWriteInterval = 10 * time.Second

func cleanDeviceVersion(value string) string { return versionmeta.Clean(value) }

func jsonVersion(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

type deviceVersionStore interface {
	RecordDeviceVersions(ctx context.Context, namespace, device, wanctlVersion, opencuVersion string, reported bool) error
}

type versionReport struct {
	inst, wanctl, opencu string
	reported             bool
}

type deviceVersionState struct {
	want            versionReport
	stored          *versionReport
	revision        uint64
	attempted       uint64
	running, failed bool
	offline         bool
	lastWrite       time.Time
	wake            chan struct{}
}

// Called under registrationMu so accepted registrations retain their order.
// The returned kick MUST run after unlocking: no storage I/O belongs in the
// registration critical path. One writer per device serializes old/new reports.
func (r *Relay) queueDeviceVersions(ns, device, inst, wanctl, opencu string, reported bool) func() {
	store, ok := r.admin.(deviceVersionStore)
	if !ok {
		return func() {}
	}
	key := ns + "/" + device
	r.versionsMu.Lock()
	if r.versions == nil {
		r.versions = make(map[string]*deviceVersionState)
	}
	state := r.versions[key]
	if state == nil {
		state = &deviceVersionState{wake: make(chan struct{}, 1)}
		r.versions[key] = state
	}
	state.offline = false
	state.want = versionReport{inst, cleanDeviceVersion(wanctl), cleanDeviceVersion(opencu), reported}
	state.revision++
	r.versionsMu.Unlock()
	return func() {
		r.versionsMu.Lock()
		if r.versions[key] != state || state.offline || state.running || (state.stored != nil && *state.stored == state.want) {
			r.versionsMu.Unlock()
			return
		}
		state.running = true
		r.versionsMu.Unlock()
		go r.writeDeviceVersions(store, ns, device, state)
	}
}

func (r *Relay) writeDeviceVersions(store deviceVersionStore, ns, device string, state *deviceVersionState) {
	interval := r.versionWriteInterval
	if interval <= 0 {
		interval = deviceVersionWriteInterval
	}
	for {
		r.versionsMu.Lock()
		report, revision := state.want, state.revision
		unchanged := state.stored != nil && *state.stored == report
		if unchanged || (state.offline && state.attempted == revision) {
			if !state.offline || !time.Now().Before(state.lastWrite.Add(interval)) {
				if state.offline {
					delete(r.versions, ns+"/"+device)
				}
				state.running = false
				r.versionsMu.Unlock()
				return
			}
		}

		if delay := time.Until(state.lastWrite.Add(interval)); delay > 0 {
			r.versionsMu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-state.wake:
			}
			timer.Stop()
			continue
		}
		state.attempted = revision
		state.lastWrite = time.Now()
		r.versionsMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), deviceVersionWriteTimeout)
		err := store.RecordDeviceVersions(ctx, ns, device, report.wanctl, report.opencu, report.reported)
		cancel()
		r.versionsMu.Lock()
		logFailure := err != nil && !state.failed
		if err == nil {
			state.stored = &report
			state.failed = false
		} else {
			state.failed = true
		}
		r.versionsMu.Unlock()
		if logFailure {
			// Never log driver errors, report values, raw device names or credentials.
			// Logging also stays outside the registration/cache locks.
			id := sha256.Sum256([]byte(ns + "/" + device))
			log.Printf("relay: device version metadata write failed (device %x)", id[:8])
		}
		r.versionsMu.Lock()
		done := state.revision == revision && !state.offline
		if done {
			state.running = false
		}
		r.versionsMu.Unlock()
		if done {
			return
		}
		// A newer report arrived while writing. Only this worker can write it, so
		// a slow older poll can never finish after a newer value has been stored.
	}
}

func (p *PGStore) RecordDeviceVersions(ctx context.Context, ns, device, wanctlVersion, opencuVersion string, reported bool) error {
	ctx, cancel := context.WithTimeout(ctx, deviceVersionWriteTimeout)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `UPDATE devices SET wanctl_version=$3, opencu_version=$4,
 versions_reported_at=CASE WHEN $5 THEN now() ELSE NULL END
 WHERE owner_namespace=$1 AND device_id=$2`, ns, device, cleanDeviceVersion(wanctlVersion), cleanDeviceVersion(opencuVersion), reported)
	return err
}

// Unknown includes development builds and an unavailable stable distribution.
// A newer beta keeps its installed label and is never marked as outdated.
func deviceVersionStatus(installed, stable string) string {
	cmp, err := release.CompareVersions(installed, stable)
	if err != nil {
		return "unknown"
	}
	if cmp < 0 {
		return "behind"
	}
	if cmp == 0 {
		return "latest"
	}
	return "ahead"
}

// Recheck under registrationMu so cleanup cannot remove a replacement's state.
// Retain the cooldown until its interval ends, including across reconnects.
// A running writer retires its own state, keeping reconnects serialized with it.
func (r *Relay) pruneDeviceVersions(ns, device string) {
	r.registrationMu.Lock()
	defer r.registrationMu.Unlock()
	if r.deviceLive(ns, device) {
		return
	}
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()
	key := ns + "/" + device
	if state := r.versions[key]; state != nil {
		state.offline = true
		if !state.running {
			state.running = true
			store := r.admin.(deviceVersionStore)
			go r.writeDeviceVersions(store, ns, device, state)
		} else {
			select {
			case state.wake <- struct{}{}:
			default:
			}
		}
	}
}
