package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wanrelease "wanctl/internal/release"
)

// TestDecideAutoUpdate is the decision table. Every row is a reason a device
// might not be updated right now, and the order matters: a check must never
// reach the network to discover that the setting is off, and must never be
// told it is "up to date" when what actually happened is that its directory is
// read-only.
func TestDecideAutoUpdate(t *testing.T) {
	const (
		current = "v1.0.0"
		newer   = "v2.0.0"
	)
	fetched := false
	offers := func(version string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			fetched = true
			return version, err
		}
	}

	for _, tc := range []struct {
		name        string
		probe       autoUpdateProbe
		want        autoUpdateAction
		wantVersion string
		wantFetch   bool
	}{
		{
			name:  "setting off",
			probe: autoUpdateProbe{enabled: func() bool { return false }, version: current},
			want:  autoUpdateSkip,
		},
		{
			name:  "development build",
			probe: autoUpdateProbe{enabled: yes, version: "dev"},
			want:  autoUpdateSkip,
		},
		{
			name:  "unversioned build",
			probe: autoUpdateProbe{enabled: yes, version: ""},
			want:  autoUpdateSkip,
		},
		{
			name: "the copy inside an installed APK",
			probe: autoUpdateProbe{
				enabled: yes, version: current,
				self:  "/data/app/~~a==/dev.wanctl.agent-1==/lib/arm64/libwanctl.so",
				isAPK: func(string) bool { return true },
			},
			want: autoUpdateSkip,
		},
		{
			name: "already current",
			probe: autoUpdateProbe{
				enabled: yes, version: current, isAPK: no1,
				offered: offers("", fmt.Errorf("select: %w", wanrelease.ErrUpToDate)),
			},
			want:      autoUpdateCurrent,
			wantFetch: true,
		},
		{
			name: "the manifest could not be verified",
			probe: autoUpdateProbe{
				enabled: yes, version: current, isAPK: no1,
				offered: offers("", errors.New("verify release manifest: bad signature")),
			},
			want:      autoUpdateFailed,
			wantFetch: true,
		},
		{
			name: "a newer release, in a directory this process cannot write",
			probe: autoUpdateProbe{
				enabled: yes, version: current, isAPK: no1, self: "/usr/local/bin/wanctl",
				offered:  offers(newer, nil),
				writable: func(string) bool { return false },
				busy:     no0,
			},
			want:        autoUpdateBlocked,
			wantVersion: newer,
			wantFetch:   true,
		},
		{
			name: "a newer release, but the agent is mid-session",
			probe: autoUpdateProbe{
				enabled: yes, version: current, isAPK: no1,
				offered:  offers(newer, nil),
				writable: func(string) bool { return true },
				busy:     func() bool { return true },
			},
			want:        autoUpdateBusy,
			wantVersion: newer,
			wantFetch:   true,
		},
		{
			name: "a newer release and nothing in the way",
			probe: autoUpdateProbe{
				enabled: yes, version: current, isAPK: no1,
				offered:  offers(newer, nil),
				writable: func(string) bool { return true },
				busy:     no0,
			},
			want:        autoUpdateReady,
			wantVersion: newer,
			wantFetch:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetched = false
			got := decideAutoUpdate(t.Context(), tc.probe)
			if got.action != tc.want {
				t.Errorf("action = %v, want %v", got.action, tc.want)
			}
			if got.version != tc.wantVersion {
				t.Errorf("version = %q, want %q", got.version, tc.wantVersion)
			}
			if fetched != tc.wantFetch {
				t.Errorf("fetched the manifest = %v, want %v", fetched, tc.wantFetch)
			}
			if (got.err != nil) != (tc.want == autoUpdateFailed) {
				t.Errorf("err = %v for action %v", got.err, got.action)
			}
		})
	}
}

// TestSuccessorArgs pins what a replacement agent is started with. The Windows
// respawn supplies "agent" itself, so handing it the whole os.Args produced
// `<self> agent <self> agent --relay …`; the child's FlagSet stops at the first
// positional, so the successor came up with no relay, no name, no mode and no
// portal fingerprints while looking like a healthy restart.
//
// Not a Windows-only test: the bug is in argument arithmetic, and arithmetic
// that only runs on the platform nobody develops on is arithmetic nobody checks.
func TestSuccessorArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		osArgs []string
		want   []string
	}{
		{
			name:   "the flags the agent was started with",
			osArgs: []string{"wanctl", "agent", "--relay", "wss://relay.example", "--mode", "bypass"},
			want:   []string{"--relay", "wss://relay.example", "--mode", "bypass"},
		},
		{
			name:   "a supervised agent keeps its --managed marker",
			osArgs: []string{"wanctl", "agent", "--managed", "--portal-fps", "SHA256:abc"},
			want:   []string{"--managed", "--portal-fps", "SHA256:abc"},
		},
		{
			name:   "no flags at all",
			osArgs: []string{"wanctl", "agent"},
			want:   nil,
		},
		{
			name:   "no subcommand, which cmdAgent cannot be reached without",
			osArgs: []string{"wanctl"},
			want:   nil,
		},
		{
			name:   "an empty argv",
			osArgs: nil,
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := successorArgs(tc.osArgs); !slices.Equal(got, tc.want) {
				t.Fatalf("successorArgs(%q) = %q, want %q", tc.osArgs, got, tc.want)
			}
		})
	}
}

// Named stubs, because a decision table reads better as "enabled: yes" than as
// a column of identical closures.
func yes() bool       { return true }
func no0() bool       { return false }
func no1(string) bool { return false }

// testUpdater is an updater whose timing is fixed and whose log is captured.
type testUpdater struct {
	*autoUpdater
	self     string
	logs     []string
	restarts int
}

func newTestUpdater(t *testing.T, releaseBase, version string, busy func() bool) *testUpdater {
	t.Helper()
	dir := t.TempDir()
	self := filepath.Join(dir, "wanctl")
	if err := os.WriteFile(self, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_RELEASE_BASE", releaseBase)
	t.Setenv("WANCTL_AUTO_UPDATE", "")

	tu := &testUpdater{self: self}
	tu.autoUpdater = newAutoUpdater(self, version, busy, func() { tu.restarts++ })
	tu.first = 0
	tu.interval = time.Hour
	tu.maxJitter = 0
	tu.busyRetry = 5 * time.Minute
	tu.jitter = func(time.Duration) time.Duration { return 0 }
	tu.logf = func(format string, args ...any) { tu.logs = append(tu.logs, fmt.Sprintf(format, args...)) }
	return tu
}

func (tu *testUpdater) binary(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(tu.self)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAutoUpdaterInstallsAndHandsOver is the whole feature end to end minus the
// exec: a signed release the device does not have becomes the file on disk, and
// the agent is asked to stop so the new image can take over.
func TestAutoUpdaterInstallsAndHandsOver(t *testing.T) {
	srv := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer srv.Close()
	tu := newTestUpdater(t, srv.URL+"/dl", "v1.0.0", no0)

	_, done := tu.tick(t.Context())
	if !done {
		t.Fatal("tick did not report a hand-over")
	}
	if got := tu.binary(t); got != "new binary" {
		t.Fatalf("binary on disk = %q", got)
	}
	if tu.restarts != 1 {
		t.Fatalf("restart hook called %d times, want 1", tu.restarts)
	}
	if !tu.updated() {
		t.Fatal("updated() = false after a successful swap")
	}
	if len(tu.logs) != 1 || !strings.Contains(tu.logs[0], "自动更新 v1.0.0 → v2.0.0，已验签，正在重启 agent") {
		t.Fatalf("logs = %q", tu.logs)
	}
	// Nothing but the binary is left in its directory: a tempfile abandoned
	// next to the binary is a file someone has to explain later.
	entries, err := os.ReadDir(filepath.Dir(tu.self))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want just the binary", len(entries))
	}
}

// A device in the middle of a shell session must not have its binary swapped
// under it — but it must not be stranded on the old build either, so the very
// next check once it goes idle installs.
func TestAutoUpdaterWaitsForIdle(t *testing.T) {
	srv := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer srv.Close()
	var busy atomic.Bool
	busy.Store(true)
	tu := newTestUpdater(t, srv.URL+"/dl", "v1.0.0", busy.Load)

	wait, done := tu.tick(t.Context())
	if done {
		t.Fatal("swapped the binary while the agent was busy")
	}
	if wait != tu.busyRetry {
		t.Fatalf("retry in %s, want %s", wait, tu.busyRetry)
	}
	if got := tu.binary(t); got != "old binary" {
		t.Fatalf("binary on disk = %q, want it untouched", got)
	}
	if tu.restarts != 0 {
		t.Fatal("restart hook called while busy")
	}

	busy.Store(false)
	if _, done := tu.tick(t.Context()); !done {
		t.Fatal("did not install once idle")
	}
	if got := tu.binary(t); got != "new binary" {
		t.Fatalf("binary on disk = %q", got)
	}
}

// The setting is read at every check, so switching it off reaches a running
// agent without a restart — and nothing is logged about a device that was
// asked to stay where it is.
func TestAutoUpdaterObeysTheSetting(t *testing.T) {
	srv := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer srv.Close()
	tu := newTestUpdater(t, srv.URL+"/dl", "v1.0.0", no0)
	t.Setenv("WANCTL_AUTO_UPDATE", "off")

	wait, done := tu.tick(t.Context())
	if done {
		t.Fatal("installed with auto_update off")
	}
	if wait != tu.interval {
		t.Fatalf("wait = %s, want the ordinary interval %s", wait, tu.interval)
	}
	if got := tu.binary(t); got != "old binary" {
		t.Fatalf("binary on disk = %q, want it untouched", got)
	}
	if len(tu.logs) != 0 {
		t.Fatalf("logged %q; a switch someone deliberately set is not news", tu.logs)
	}

	t.Setenv("WANCTL_AUTO_UPDATE", "on")
	if _, done := tu.tick(t.Context()); !done {
		t.Fatal("switching the setting back on did not take effect")
	}
}

// /usr/local/bin: the daemon cannot write there and must never try sudo. It
// says so once for the version it cannot install, not every six hours forever.
func TestAutoUpdaterWarnsOncePerVersionWhenUnwritable(t *testing.T) {
	srv := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer srv.Close()
	tu := newTestUpdater(t, srv.URL+"/dl", "v1.0.0", no0)
	tu.probe.writable = no1

	for i := 0; i < 3; i++ {
		if _, done := tu.tick(t.Context()); done {
			t.Fatal("installed into a directory it cannot write")
		}
	}
	if len(tu.logs) != 1 {
		t.Fatalf("logged %d times, want exactly one hint: %q", len(tu.logs), tu.logs)
	}
	want := "wanctl: 新版本 v2.0.0 可用，但 " + filepath.Dir(tu.self) + " 不可写，自动更新无法进行；请手动运行 sudo wanctl update"
	if tu.logs[0] != want {
		t.Fatalf("log = %q\nwant %q", tu.logs[0], want)
	}
	if got := tu.binary(t); got != "old binary" {
		t.Fatalf("binary on disk = %q, want it untouched", got)
	}
}

// A device that is off the network for a week must not write the same DNS
// error into its log every six hours — but a different failure is still news,
// and so is the same one after something else happened in between.
func TestAutoUpdaterLogsRepeatedFailureOnce(t *testing.T) {
	tu := newTestUpdater(t, "https://unused.example/dl", "v1.0.0", no0)
	failure := errors.New("fetch manifest: dial tcp: no such host")
	tu.probe.offered = func(context.Context) (string, error) { return "", failure }

	for i := 0; i < 3; i++ {
		tu.tick(t.Context())
	}
	if len(tu.logs) != 1 {
		t.Fatalf("logged %d times, want one: %q", len(tu.logs), tu.logs)
	}
	if !strings.Contains(tu.logs[0], "wanctl: 自动更新检查失败: fetch manifest") {
		t.Fatalf("log = %q", tu.logs[0])
	}

	failure = errors.New("verify release manifest: no trusted key")
	tu.tick(t.Context())
	if len(tu.logs) != 2 {
		t.Fatalf("a different failure was swallowed: %q", tu.logs)
	}

	// An intervening success resets the guard: the next occurrence of the old
	// error describes a new outage, not the one already reported.
	tu.probe.offered = func(context.Context) (string, error) {
		return "", fmt.Errorf("select: %w", wanrelease.ErrUpToDate)
	}
	tu.tick(t.Context())
	tu.probe.offered = func(context.Context) (string, error) {
		return "", errors.New("verify release manifest: no trusted key")
	}
	tu.tick(t.Context())
	if len(tu.logs) != 3 {
		t.Fatalf("failure after a healthy check was swallowed: %q", tu.logs)
	}
}

// The loop itself: it waits the first delay before the first check, and stops
// on its own once the binary has been handed over.
func TestAutoUpdateLoopStopsAfterHandOver(t *testing.T) {
	srv := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer srv.Close()
	tu := newTestUpdater(t, srv.URL+"/dl", "v1.0.0", no0)
	tu.first = autoUpdateFirstDelay

	var waits []time.Duration
	tu.after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	done := make(chan struct{})
	go func() { tu.run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after handing over")
	}
	if len(waits) != 1 || waits[0] != autoUpdateFirstDelay {
		t.Fatalf("waits = %v, want one first delay of %s", waits, autoUpdateFirstDelay)
	}
	if tu.restarts != 1 {
		t.Fatalf("restart hook called %d times", tu.restarts)
	}
}

// A cancelled agent takes its updater with it, without running a check.
func TestAutoUpdateLoopStopsWithTheAgent(t *testing.T) {
	tu := newTestUpdater(t, "https://unused.example/dl", "v1.0.0", no0)
	checked := false
	tu.probe.offered = func(context.Context) (string, error) {
		checked = true
		return "", errors.New("should not be reached")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() { tu.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop ignored a cancelled context")
	}
	if checked {
		t.Fatal("ran a check after the agent stopped")
	}
}

// TestUpdateSourceFallsBackToTheMirror: a release page the device cannot reach
// is the ordinary state behind a restrictive egress, and the relay's mirror
// serves the same signed manifest — so an unreachable primary must not be the
// end of the story.
func TestUpdateSourceFallsBackToTheMirror(t *testing.T) {
	var primaryHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer primary.Close()
	mirror := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
	defer mirror.Close()

	tu := newTestUpdater(t, primary.URL+"/dl", "v1.0.0", no0)
	t.Setenv("WANCTL_RELAY", mirror.URL)

	if _, done := tu.tick(t.Context()); !done {
		t.Fatalf("no install; logs = %q", tu.logs)
	}
	if got := tu.binary(t); got != "new binary" {
		t.Fatalf("binary on disk = %q", got)
	}
	if primaryHits.Load() == 0 {
		t.Fatal("the mirror was used without the release page being tried first")
	}
}

// The other half of the rule: "nothing newer" is an answer, not a failure. A
// second round trip to a mirror that would say the same thing is waste, and on
// a fleet it is waste multiplied by every device every six hours.
func TestUpToDatePrimaryDoesNotConsultTheMirror(t *testing.T) {
	var mirrorHits atomic.Int64
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer mirror.Close()
	primary := signedUpdateServer(t, []byte("new binary"), runtime.GOOS, runtime.GOARCH, "v1.0.0", nil)
	defer primary.Close()

	tu := newTestUpdater(t, primary.URL+"/dl", "v1.0.0", no0)
	t.Setenv("WANCTL_RELAY", mirror.URL)

	if _, done := tu.tick(t.Context()); done {
		t.Fatal("installed a release that is not newer")
	}
	if mirrorHits.Load() != 0 {
		t.Fatalf("mirror was consulted %d times after a verified up-to-date manifest", mirrorHits.Load())
	}
	if len(tu.logs) != 0 {
		t.Fatalf("being current is not news: %q", tu.logs)
	}
}
