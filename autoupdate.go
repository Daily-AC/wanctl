package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"wanctl/internal/config"
	wanrelease "wanctl/internal/release"
)

// Auto-update timing. The first check is late enough that a device rebooting
// into a broken release still spends its first minute serving, and the interval
// is long enough that a fleet of any size is a trickle of manifest fetches
// rather than a stampede — which is what the jitter is for.
const (
	autoUpdateFirstDelay = time.Minute
	autoUpdateInterval   = 6 * time.Hour
	autoUpdateMaxJitter  = 30 * time.Minute
	autoUpdateBusyRetry  = 5 * time.Minute
)

// autoUpdateAction is what one check concluded.
type autoUpdateAction int

const (
	// autoUpdateSkip: this binary is not a candidate at all (setting off, a
	// development build, the copy inside an APK). Nothing is said, every time.
	autoUpdateSkip autoUpdateAction = iota
	// autoUpdateCurrent: the manifest verified and offers nothing newer.
	autoUpdateCurrent
	// autoUpdateFailed: the manifest could not be fetched or verified.
	autoUpdateFailed
	// autoUpdateBlocked: a newer release exists but this process cannot write
	// the directory its own binary lives in.
	autoUpdateBlocked
	// autoUpdateBusy: a newer release exists and the agent is mid-work.
	autoUpdateBusy
	// autoUpdateReady: install it.
	autoUpdateReady
)

// autoUpdateResult is one check's conclusion plus what it needs to be acted on
// or reported.
type autoUpdateResult struct {
	action  autoUpdateAction
	version string // the release on offer, when one is
	err     error  // set only for autoUpdateFailed
}

// autoUpdateProbe is everything a decision depends on, injected. Nothing in
// decideAutoUpdate touches the network, the filesystem, the clock or the config
// directly, so the decision table is a table.
type autoUpdateProbe struct {
	enabled  func() bool                               // the auto_update setting, re-read every check
	version  string                                    // this build's version
	self     string                                    // the binary that would be replaced
	isAPK    func(self string) bool                    // the copy inside an installed Android app
	offered  func(ctx context.Context) (string, error) // verified manifest -> version, or ErrUpToDate
	writable func(dir string) bool                     // can this process create files next to itself
	busy     func() bool                               // is the agent mid-session / mid-job
}

// decideAutoUpdate runs one check's decision in the order the cheapest and most
// final answers come first: a switch nobody has to reach the network to read,
// then two properties of this binary that no release can change, and only then
// the manifest. Writability is asked after the manifest rather than before, so
// a device in /usr/local/bin is told which version it is missing rather than
// that it is unwritable in the abstract; and the busy gate is last, because
// deferring a check that would have concluded "already current" would postpone
// nothing.
func decideAutoUpdate(ctx context.Context, p autoUpdateProbe) autoUpdateResult {
	if !p.enabled() {
		return autoUpdateResult{action: autoUpdateSkip}
	}
	// A developer's own build has no version to compare and no business being
	// replaced under them by a release.
	if p.version == "" || p.version == "dev" {
		return autoUpdateResult{action: autoUpdateSkip}
	}
	if p.isAPK(p.self) {
		return autoUpdateResult{action: autoUpdateSkip}
	}
	version, err := p.offered(ctx)
	if errors.Is(err, wanrelease.ErrUpToDate) {
		return autoUpdateResult{action: autoUpdateCurrent}
	}
	if err != nil {
		return autoUpdateResult{action: autoUpdateFailed, err: err}
	}
	if !p.writable(filepath.Dir(p.self)) {
		return autoUpdateResult{action: autoUpdateBlocked, version: version}
	}
	if p.busy() {
		return autoUpdateResult{action: autoUpdateBusy, version: version}
	}
	return autoUpdateResult{action: autoUpdateReady, version: version}
}

// autoUpdater is the agent-side loop: check, decide, install, hand over.
type autoUpdater struct {
	probe autoUpdateProbe

	// Timing, injected so the tests are instant rather than six hours long.
	first     time.Duration
	interval  time.Duration
	maxJitter time.Duration
	busyRetry time.Duration
	jitter    func(max time.Duration) time.Duration
	after     func(d time.Duration) <-chan time.Time

	// install swaps the binary and returns the version now on disk.
	install func(ctx context.Context) (string, error)
	// restart tells the agent to stop so the new image can take over.
	restart func()
	logf    func(format string, args ...any)

	// Repetition guards. A daemon's log is read after something went wrong,
	// weeks later; the same line every six hours buries whatever else it holds.
	lastErr    string
	warnedFor  string
	handedOver atomic.Bool
}

// newAutoUpdater wires the loop to a live agent. self must already be resolved
// through any symlink: it is the file that gets replaced.
func newAutoUpdater(self, version string, busy func() bool, restart func()) *autoUpdater {
	u := &autoUpdater{
		probe: autoUpdateProbe{
			enabled:  config.AutoUpdateEnabled,
			version:  version,
			self:     self,
			isAPK:    runningFromAPK,
			writable: canWriteDir,
			busy:     busy,
		},
		first:     autoUpdateFirstDelay,
		interval:  autoUpdateInterval,
		maxJitter: autoUpdateMaxJitter,
		busyRetry: autoUpdateBusyRetry,
		jitter:    func(max time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(max) + 1)) },
		after:     time.After,
		restart:   restart,
		logf:      func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	u.probe.offered = u.offered
	u.install = u.installLatest
	return u
}

// startAutoUpdate launches the self-update loop beside a running agent, or
// returns nil when this process is not a candidate for one. The loop runs on
// its own goroutine and never gates the agent's relay connection: a device that
// cannot reach a release source must still be controllable.
//
// Android is out of scope. Its two shapes are the copy inside an installed APK,
// whose directory is system-owned, and a Termux install, whose binary sits in a
// private data directory that Android refuses to exec directly — so neither can
// complete the restart half, whatever the swap does.
func startAutoUpdate(ctx context.Context, version string, busy func() bool, restart func()) *autoUpdater {
	if runtime.GOOS == "android" {
		return nil
	}
	self, err := selfPath()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	u := newAutoUpdater(self, version, busy, restart)
	go u.run(ctx)
	return u
}

// offered asks each configured release source what it has for this platform.
func (u *autoUpdater) offered(ctx context.Context) (string, error) {
	bases, err := updateSources()
	if err != nil {
		return "", err
	}
	got, err := overSources(bases, func(base string) (updateFetch, error) {
		version, err := checkSignedUpdate(ctx, base, runtime.GOOS, runtime.GOARCH, u.probe.version)
		return updateFetch{version: version}, err
	})
	return got.version, err
}

// installLatest downloads, verifies and swaps in the newest release. The
// download is a second round trip after the check that decided to do it: the
// alternative is holding a verified tempfile across the busy gate, and a
// half-installed update parked in a bin directory is worse than a repeated
// fetch of a file that is already in a CDN's cache.
func (u *autoUpdater) installLatest(ctx context.Context) (string, error) {
	dir := filepath.Dir(u.probe.self)
	bases, err := updateSources()
	if err != nil {
		return "", err
	}
	got, err := overSources(bases, func(base string) (updateFetch, error) {
		path, version, err := downloadSignedUpdate(ctx, base, dir, runtime.GOOS, runtime.GOARCH, u.probe.version)
		return updateFetch{path: path, version: version}, err
	})
	if err != nil {
		return "", err
	}
	defer os.Remove(got.path) // a no-op once the rename consumes it
	if err := os.Chmod(got.path, 0o755); err != nil {
		return "", fmt.Errorf("chmod new binary: %w", err)
	}
	if err := replaceBinary(got.path, u.probe.self); err != nil {
		return "", fmt.Errorf("replace binary at %s: %w", u.probe.self, err)
	}
	return got.version, nil
}

// updated reports whether a swap succeeded and the process should hand over to
// the binary now on disk.
func (u *autoUpdater) updated() bool { return u.handedOver.Load() }

// binaryPath is the file the updater replaces: this binary, with any symlink
// already resolved. It is what the restart must exec.
func (u *autoUpdater) binaryPath() string { return u.probe.self }

// successorArgs is the flag list to hand a replacement agent process: this
// invocation's arguments with the program name and the subcommand removed.
//
// It exists because the two restart shapes need different things from the same
// os.Args. A Unix exec replaces the image and takes the whole argv, subcommand
// included. A Windows respawn rebuilds the command line and supplies "agent"
// itself, so passing the whole argv there produced
// `<self> agent <self> agent --relay …`: the child's FlagSet stops at the first
// positional, so the successor came up with none of the flags the agent was
// started with — no relay override, no name, no mode, no portal fingerprints —
// and looked like it had started fine.
func successorArgs(osArgs []string) []string {
	if len(osArgs) < 2 {
		return nil
	}
	return osArgs[2:]
}

// tick runs one check, acts on it, says whatever needs saying, and reports how
// long to wait before the next one. The second return is true when this agent
// is done: the binary has been replaced and the restart has been asked for.
func (u *autoUpdater) tick(ctx context.Context) (time.Duration, bool) {
	res := decideAutoUpdate(ctx, u.probe)
	if res.action != autoUpdateFailed {
		u.lastErr = ""
	}
	switch res.action {
	case autoUpdateFailed:
		// Consecutive identical failures are one fact, not many: a device off
		// the network for a week would otherwise write the same DNS error into
		// its log 28 times.
		if msg := res.err.Error(); msg != u.lastErr {
			u.lastErr = msg
			u.logf("wanctl: 自动更新检查失败: %s", msg)
		}
	case autoUpdateBlocked:
		// Once per version, so the hint arrives when a release the device is
		// missing appears and not every six hours thereafter.
		if u.warnedFor != res.version {
			u.warnedFor = res.version
			u.logf("wanctl: 新版本 %s 可用，但 %s 不可写，自动更新无法进行；请手动运行 sudo wanctl update",
				res.version, filepath.Dir(u.probe.self))
		}
	case autoUpdateBusy:
		return u.busyRetry, false
	case autoUpdateReady:
		installed, err := u.install(ctx)
		if err != nil {
			if msg := err.Error(); msg != u.lastErr {
				u.lastErr = msg
				u.logf("wanctl: 自动更新检查失败: %s", msg)
			}
			break
		}
		u.logf("wanctl: 自动更新 %s → %s，已验签，正在重启 agent", u.probe.version, installed)
		u.handedOver.Store(true)
		u.restart()
		return 0, true
	}
	return u.interval + u.jitter(u.maxJitter), false
}

// run checks until ctx is cancelled or this agent hands over to a newer binary.
func (u *autoUpdater) run(ctx context.Context) {
	wait := u.first
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.after(wait):
		}
		if ctx.Err() != nil {
			return
		}
		next, done := u.tick(ctx)
		if done || ctx.Err() != nil {
			return
		}
		wait = next
	}
}
