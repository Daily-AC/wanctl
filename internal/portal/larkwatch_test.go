package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/lark"
)

type fakeCardSend struct {
	mu      sync.Mutex
	sends   []fakeCardMessage
	updates []fakeCardMessage
	sendErr error
}

type fakeCardMessage struct {
	email     string
	messageID string
	card      any
}

func (f *fakeCardSend) SendCard(_ context.Context, email string, card any) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", "", f.sendErr
	}
	messageID := "om-" + string(rune('1'+len(f.sends)))
	f.sends = append(f.sends, fakeCardMessage{email: email, messageID: messageID, card: card})
	return messageID, "oc-owner", nil
}

func (f *fakeCardSend) setSendError(err error) {
	f.mu.Lock()
	f.sendErr = err
	f.mu.Unlock()
}

func (f *fakeCardSend) UpdateCard(_ context.Context, messageID string, card any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, fakeCardMessage{messageID: messageID, card: card})
	return nil
}

func (f *fakeCardSend) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends), len(f.updates)
}

func (f *fakeCardSend) firstCard(t *testing.T) any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		t.Fatal("no card was sent")
	}
	return f.sends[0].card
}

func (f *fakeCardSend) lastUpdateCard(t *testing.T) any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updates) == 0 {
		t.Fatal("no card was updated")
	}
	return f.updates[len(f.updates)-1].card
}

type fakeDeviceSession struct {
	states chan console.State

	mu           sync.Mutex
	timeoutCalls []int
	decisions    []fakeDecision
	pairings     []fakeDecision
	decideErr    error
	timeoutErr   error
}

type fakeDecision struct {
	id       string
	verdict  string
	approver string
}

func newFakeDeviceSession() *fakeDeviceSession {
	return &fakeDeviceSession{states: make(chan console.State, 16)}
}

func (f *fakeDeviceSession) subscribe() (<-chan console.State, func()) {
	return f.states, func() {}
}

func (f *fakeDeviceSession) decide(id, verdict, approver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, fakeDecision{id: id, verdict: verdict, approver: approver})
	return f.decideErr
}

func (f *fakeDeviceSession) pairDecide(fp, verdict string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairings = append(f.pairings, fakeDecision{id: fp, verdict: verdict})
	return f.decideErr
}

func (f *fakeDeviceSession) setApprovalTimeout(sec int) (int, error) {
	f.mu.Lock()
	f.timeoutCalls = append(f.timeoutCalls, sec)
	err := f.timeoutErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return sec, nil
}

func (f *fakeDeviceSession) snapshot() ([]int, []fakeDecision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.timeoutCalls...), append([]fakeDecision(nil), f.decisions...)
}

func newTestLarkSupervisor(sender cardSender, sessionFor sessionForFunc) (*larkSupervisor, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &larkSupervisor{
		sender: sender, grants: lark.NewGrants(), sessionFor: sessionFor,
		portalOrigin: "https://portal.test", logf: func(string, ...any) {},
		retryMin: time.Millisecond, retryMax: 4 * time.Millisecond,
		ctx: ctx, cancel: cancel, watchers: make(map[string]*larkWatcher),
		stopped:  make(map[string]larkWatcherStopped),
		health:   make(map[string]larkDeliveryHealth),
		dialLogs: make(map[string]time.Time),
		actions:  make(map[string]larkActionRecord), wake: make(chan struct{}, 1),
	}
	return s, cancel
}

func enabledLarkConfig() deviceLarkApproval {
	return deviceLarkApproval{
		Namespace: "alice", Device: "legion", ApprovalEnabled: true,
		PairingFromCard: true, NotifyEmail: "alice@example.com",
	}
}

func TestLarkDeliveryHealthRecordsSendFailuresAndResetsAfterSuccess(t *testing.T) {
	sender := &fakeCardSend{}
	sender.setSendError(errors.New("upstream rejected recipient"))
	sup, cancel := newTestLarkSupervisor(sender, nil)
	defer cancel()
	watcher := &larkWatcher{sup: sup, ns: "alice", device: "legion", config: enabledLarkConfig()}
	pendingSeen := make(map[string]seenPending)
	pairingSeen := make(map[string]seenPairing)
	state := console.State{PendingPairings: []console.PendingPairing{{FP: "SHA256:new", Name: "new controller"}}}

	watcher.reconcileState(context.Background(), state, pendingSeen, pairingSeen, time.Minute)
	watcher.reconcileState(context.Background(), state, pendingSeen, pairingSeen, time.Minute)
	health := sup.deliveryHealth("alice", "legion")
	if health == nil || health.Result != "failure" || health.Kind != "pairing" ||
		health.Error != "upstream rejected recipient" || health.ConsecutiveFailures != 2 || health.AttemptedAt.IsZero() {
		t.Fatalf("health after failures = %+v", health)
	}

	sender.setSendError(nil)
	watcher.reconcileState(context.Background(), state, pendingSeen, pairingSeen, time.Minute)
	health = sup.deliveryHealth("alice", "legion")
	if health == nil || health.Result != "success" || health.Kind != "pairing" ||
		health.Error != "" || health.ConsecutiveFailures != 0 {
		t.Fatalf("health after success = %+v", health)
	}
}

func TestLarkDeliveryHealthIsConcurrentSafe(t *testing.T) {
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, nil)
	defer cancel()
	const failures = 64
	var wg sync.WaitGroup
	for i := 0; i < failures; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup.recordHealth("alice", "legion", "approval", errors.New("send failed"))
			_ = sup.deliveryHealth("alice", "legion")
		}()
	}
	wg.Wait()
	health := sup.deliveryHealth("alice", "legion")
	if health == nil || health.ConsecutiveFailures != failures {
		t.Fatalf("health after concurrent failures = %+v, want %d failures", health, failures)
	}
	sup.recordHealth("alice", "legion", "approval", nil)
	if got := sup.deliveryHealth("alice", "legion"); got == nil || got.ConsecutiveFailures != 0 || got.Result != "success" {
		t.Fatalf("health after reset = %+v", got)
	}
}

func TestLarkDialSuccessDoesNotHideDeliveryFailure(t *testing.T) {
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, nil)
	defer cancel()
	sup.recordHealth("alice", "legion", "pairing", errors.New("send failed"))
	sup.recordHealth("alice", "legion", "dial", nil)
	health := sup.deliveryHealth("alice", "legion")
	if health == nil || health.Kind != "pairing" || health.Result != "failure" || health.Error != "send failed" {
		t.Fatalf("dial success hid delivery failure: %+v", health)
	}

	sup.recordHealth("bob", "tablet", "dial", errors.New("offline"))
	sup.recordHealth("bob", "tablet", "dial", nil)
	health = sup.deliveryHealth("bob", "tablet")
	if health == nil || health.Kind != "dial" || health.Result != "success" || health.ConsecutiveFailures != 0 {
		t.Fatalf("dial success did not clear dial failure: %+v", health)
	}
}

func TestLarkDeliveryHealthTruncatesErrors(t *testing.T) {
	message := strings.Repeat("错", larkHealthErrorRunes+20)
	got := truncateLarkHealthError(message)
	if !strings.HasSuffix(got, "...") || len([]rune(strings.TrimSuffix(got, "..."))) != larkHealthErrorRunes {
		t.Fatalf("truncated error has %d runes and suffix %q", len([]rune(got)), got[len(got)-3:])
	}
}

func TestLarkWatcherDiffSendsOnceAndResolvesDisappearedPending(t *testing.T) {
	sender := &fakeCardSend{}
	session := newFakeDeviceSession()
	sup, cancel := newTestLarkSupervisor(sender, func(context.Context, string, string) (deviceSession, error) {
		return session, nil
	})
	defer cancel()
	sup.applyConfigs([]deviceLarkApproval{enabledLarkConfig()})
	waitFor(t, func() bool {
		calls, _ := session.snapshot()
		return len(calls) > 0 && calls[0] == 180
	})

	pending := console.Pending{ID: "approval-1", Kind: "exec", Cmd: "echo ok", Cwd: "/tmp", Peer: "controller"}
	state := console.State{Pending: []console.Pending{pending}}
	session.states <- state
	session.states <- state
	session.states <- state
	waitFor(t, func() bool {
		sends, _ := sender.counts()
		return sends == 1
	})
	time.Sleep(10 * time.Millisecond)
	if sends, _ := sender.counts(); sends != 1 {
		t.Fatalf("repeated identical State sent %d cards, want 1", sends)
	}

	nonce := cardNonce(t, sender.firstCard(t))
	grant, err := sup.grants.Consume(nonce, "oc-owner", "event-card-proof")
	if err != nil {
		t.Fatalf("card nonce has no consumable grant: %v", err)
	}
	if grant.NS != "alice" || grant.Device != "legion" || grant.PendingID != pending.ID {
		t.Fatalf("card nonce grant = %+v", grant)
	}

	session.states <- console.State{}
	waitFor(t, func() bool {
		_, updates := sender.counts()
		return updates == 1
	})
	sup.applyConfigs(nil)
}

// TestLarkWatcherSurvivesAnAgentWithoutTimeoutSet covers every device running a
// build older than the timeout_set verb: it answers "unknown RPC kind". Treating
// that as a dial failure would make Feishu approvals silently never work on any
// agent that has not been upgraded, which is far worse than a shorter window — so
// the watcher carries on and the card must state the wait the device actually
// uses, not the one we asked for.
func TestLarkWatcherSurvivesAnAgentWithoutTimeoutSet(t *testing.T) {
	sender := &fakeCardSend{}
	session := newFakeDeviceSession()
	session.timeoutErr = errors.New("unknown RPC kind")
	sup, cancel := newTestLarkSupervisor(sender, func(context.Context, string, string) (deviceSession, error) {
		return session, nil
	})
	defer cancel()
	sup.applyConfigs([]deviceLarkApproval{enabledLarkConfig()})

	session.states <- console.State{Pending: []console.Pending{
		{ID: "approval-1", Kind: "exec", Cmd: "echo ok", Cwd: "/tmp", Peer: "controller"},
	}}
	waitFor(t, func() bool {
		sends, _ := sender.counts()
		return sends == 1
	})

	encoded, err := json.Marshal(sender.firstCard(t))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded)
	if !strings.Contains(raw, "1 分钟") {
		t.Fatalf("card should quote the device's own 60s default, got: %s", raw)
	}
	if strings.Contains(raw, "3 分钟") {
		t.Fatalf("card quoted a 3m wait the device never accepted: %s", raw)
	}
	sup.applyConfigs(nil)
}

func TestPermanentLarkDialErrorClassification(t *testing.T) {
	permanent := &deviceRegistrationError{target: "alice/legion", reason: "relay has no registered identity"}
	if !isPermanentLarkDialError(fmt.Errorf("portal device identity: %w", permanent)) {
		t.Fatal("wrapped device registration error was not classified as permanent")
	}
	for _, err := range []error{
		errors.New("device offline"),
		errors.New(`relay has no registered identity for "lookalike text"`),
		errors.New("controller is not authorized as this device's console administrator"),
	} {
		if isPermanentLarkDialError(err) {
			t.Fatalf("transient/untyped error %q was classified as permanent", err)
		}
	}
}

// The first dial failure is the one an operator is waiting for, so it must be
// logged immediately. Further lines are limited by elapsed time per watcher;
// counting attempts still produces an unbounded stream over an unbounded outage.
func TestLarkWatcherLimitsTransientDialLogsPerDeviceTimeWindow(t *testing.T) {
	const dialError = "controller is not authorized as this device's console administrator"
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	attempts := 0
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		attempts++
		return nil, errors.New(dialError)
	})
	defer cancel()
	sup.now = func() time.Time { return now }
	sup.waitRetryFn = func(context.Context, time.Duration) bool {
		now = now.Add(20 * time.Minute)
		return attempts < 10
	}
	var logged []string
	sup.logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	watcher := &larkWatcher{sup: sup, ns: "alice", device: "legion", config: enabledLarkConfig()}
	if err := watcher.run(context.Background()); err != nil {
		t.Fatalf("transient watcher stopped with error: %v", err)
	}

	if attempts != 10 {
		t.Fatalf("dial attempts = %d, want 10", attempts)
	}
	if len(logged) != 4 {
		t.Fatalf("logged %d lines over three hours, want 4: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], dialError) || !strings.Contains(logged[0], "dial attempt 1") {
		t.Fatalf("first line = %q, want the immediate first failure", logged[0])
	}
}

func TestLarkWatcherLogWindowSurvivesWatcherRestart(t *testing.T) {
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		return nil, errors.New("temporary network failure")
	})
	defer cancel()
	sup.now = func() time.Time { return now }
	sup.waitRetryFn = func(context.Context, time.Duration) bool {
		return false
	}
	var logged []string
	sup.logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	for _, advance := range []time.Duration{0, 20 * time.Minute, 40 * time.Minute} {
		now = now.Add(advance)
		watcher := &larkWatcher{sup: sup, ns: "alice", device: "legion", config: enabledLarkConfig()}
		if err := watcher.run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d lines across watcher restarts in one hour, want 2: %v", len(logged), logged)
	}
}

func TestLarkWatcherStopsAfterPermanentDialErrorAndRecordsHealth(t *testing.T) {
	var attempts int64
	dialErr := &deviceRegistrationError{target: "alice/legion", reason: "relay has no registered identity"}
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		atomic.AddInt64(&attempts, 1)
		return nil, fmt.Errorf("portal device identity: %w", dialErr)
	})
	defer cancel()

	var logged []string
	var logMu sync.Mutex
	sup.logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	sup.applyConfigs([]deviceLarkApproval{enabledLarkConfig()})
	waitFor(t, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		_, stopped := sup.stopped["alice/legion"]
		return stopped
	})
	time.Sleep(20 * time.Millisecond)

	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Fatalf("permanent error made %d dial attempts, want 1", got)
	}
	sup.mu.Lock()
	stop := sup.stopped["alice/legion"]
	_, watching := sup.watchers["alice/legion"]
	sup.mu.Unlock()
	if watching {
		t.Fatal("permanently failed watcher remained active")
	}
	if stop.StopReason == "" || stop.StoppedAt.IsZero() {
		t.Fatalf("stopped record = %+v, want reason and timestamp", stop)
	}
	// The stop must also surface through the outward-facing health record, or the
	// portal switch goes on reading "on" with nothing naming the cause.
	if health := sup.deliveryHealth("alice", "legion"); health == nil ||
		health.Result != "failure" || health.Kind != "dial" || health.Error == "" {
		t.Fatalf("delivery health after a permanent stop = %+v", health)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "dial attempt 1") {
		t.Fatalf("permanent failure logs = %v, want exactly the immediate first line", logged)
	}
}

func TestLarkWatcherTransientDialErrorsKeepRetryingWithBackoff(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		attempts++
		return nil, errors.New("temporary network failure")
	})
	defer cancel()
	sup.waitRetryFn = func(_ context.Context, delay time.Duration) bool {
		waits = append(waits, delay)
		return len(waits) < 5
	}

	watcher := &larkWatcher{sup: sup, ns: "alice", device: "legion", config: enabledLarkConfig()}
	if err := watcher.run(context.Background()); err != nil {
		t.Fatalf("transient watcher stopped with error: %v", err)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
	if attempts != len(want) {
		t.Fatalf("dial attempts = %d, want %d", attempts, len(want))
	}
}

func TestLarkWatcherRestartsAfterStoppedConditionChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume func(deviceLarkApproval) deviceLarkApproval
	}{
		{
			name: "configuration changed",
			resume: func(cfg deviceLarkApproval) deviceLarkApproval {
				cfg.NotifyEmail = "new@example.com"
				cfg.UpdatedAt = "2026-08-13T11:00:00Z"
				return cfg
			},
		},
		{
			name: "device registered",
			resume: func(cfg deviceLarkApproval) deviceLarkApproval {
				cfg.RegisteredFingerprint = "SHA256:registered-again"
				return cfg
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			registered := false
			session := newFakeDeviceSession()
			sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
				mu.Lock()
				defer mu.Unlock()
				if !registered {
					return nil, &deviceRegistrationError{target: "alice/legion", reason: "relay has no registered identity"}
				}
				return session, nil
			})
			defer cancel()
			cfg := enabledLarkConfig()
			sup.applyConfigs([]deviceLarkApproval{cfg})
			waitFor(t, func() bool {
				sup.mu.Lock()
				defer sup.mu.Unlock()
				_, stopped := sup.stopped["alice/legion"]
				return stopped
			})

			mu.Lock()
			registered = true
			mu.Unlock()
			sup.applyConfigs([]deviceLarkApproval{tc.resume(cfg)})
			waitFor(t, func() bool {
				calls, _ := session.snapshot()
				return len(calls) > 0 && calls[0] == 180
			})
			sup.mu.Lock()
			_, stopped := sup.stopped["alice/legion"]
			_, watching := sup.watchers["alice/legion"]
			sup.mu.Unlock()
			if stopped || !watching {
				t.Fatalf("after resume: stopped=%v watching=%v", stopped, watching)
			}
			sup.applyConfigs(nil)
		})
	}
}

func TestLarkSupervisorReconcileRestartsWatcherAfterDeviceRegisters(t *testing.T) {
	var mu sync.Mutex
	registered := false
	session := newFakeDeviceSession()
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		mu.Lock()
		defer mu.Unlock()
		if !registered {
			return nil, &deviceRegistrationError{target: "alice/legion", reason: "relay has no registered identity"}
		}
		return session, nil
	})
	defer cancel()
	sup.adminReq = func(_ string, path string, _ url.Values, _ any) (*http.Response, error) {
		body := `{"namespaces":["alice"]}`
		switch path {
		case "/admin/devices":
			mu.Lock()
			isRegistered := registered
			mu.Unlock()
			if isRegistered {
				body = `{"devices":[{"name":"legion","owner":"alice","fingerprint":"SHA256:registered-again"}]}`
			} else {
				body = `{"devices":[]}`
			}
		case "/admin/devices/lark":
			body = `{"devices":[{"device":"legion","approval_enabled":true,"notify_email":"alice@example.com","updated_at":"2026-08-13T10:00:00Z"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}

	if err := sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		_, stopped := sup.stopped["alice/legion"]
		return stopped
	})
	mu.Lock()
	registered = true
	mu.Unlock()
	if err := sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		calls, _ := session.snapshot()
		return len(calls) > 0 && calls[0] == 180
	})
	sup.applyConfigs(nil)
}

// TestLarkWatcherLogsWhenItCannotReachTheDevice covers the failure mode that is
// invisible by construction. A transient dial failure keeps the watcher retrying
// while the switch still reads "on", so the first failure must be reported at
// once and the rest throttled — otherwise one unreachable device drowns the log,
// which is exactly how a single device reached 17700 logged attempts.
func TestLarkWatcherLogsWhenItCannotReachTheDevice(t *testing.T) {
	const dialError = "controller is not authorized as this device's console administrator"
	var attempts int64
	sup, cancel := newTestLarkSupervisor(&fakeCardSend{}, func(context.Context, string, string) (deviceSession, error) {
		atomic.AddInt64(&attempts, 1)
		return nil, errors.New(dialError)
	})
	defer cancel()

	var mu sync.Mutex
	var logged []string
	sup.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), logged...)
	}

	sup.applyConfigs([]deviceLarkApproval{enabledLarkConfig()})
	waitFor(t, func() bool {
		health := sup.deliveryHealth("alice", "legion")
		return health != nil && health.ConsecutiveFailures >= 3
	})
	health := sup.deliveryHealth("alice", "legion")
	if health == nil || health.Result != "failure" || health.Kind != "dial" ||
		health.Error != dialError || health.ConsecutiveFailures < 3 {
		t.Fatalf("dial health = %+v", health)
	}
	// A transient error must not stop the watcher the way a registration error does.
	sup.mu.Lock()
	_, stopped := sup.stopped["alice/legion"]
	sup.mu.Unlock()
	if stopped {
		t.Fatal("a transient dial error stopped the watcher; only permanent errors may do that")
	}
	sup.applyConfigs(nil)
	if health := sup.deliveryHealth("alice", "legion"); health != nil {
		t.Fatalf("disabled watcher retained health: %+v", health)
	}

	lines := snapshot()
	if len(lines) == 0 {
		t.Fatal("the watcher retried an unreachable device without logging anything")
	}
	if !strings.Contains(lines[0], dialError) || !strings.Contains(lines[0], "alice/legion") {
		t.Fatalf("first line = %q, want it to name the device and the underlying error", lines[0])
	}
	if tried := int(atomic.LoadInt64(&attempts)); len(lines) >= tried {
		t.Fatalf("logged %d lines for %d dial attempts; the throttle is not working", len(lines), tried)
	}
}

func TestLarkWatcherClosedChannelDoesNotResolveAndDisableRestoresTimeout(t *testing.T) {
	sender := &fakeCardSend{}
	session := newFakeDeviceSession()
	var calls int
	var callsMu sync.Mutex
	sup, cancel := newTestLarkSupervisor(sender, func(context.Context, string, string) (deviceSession, error) {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls++
		if calls == 1 {
			return session, nil
		}
		return nil, errors.New("device offline")
	})
	defer cancel()
	sup.applyConfigs([]deviceLarkApproval{enabledLarkConfig()})
	session.states <- console.State{Pending: []console.Pending{{ID: "approval-1", Kind: "exec", Cmd: "date"}}}
	waitFor(t, func() bool {
		sends, _ := sender.counts()
		return sends == 1
	})
	close(session.states)
	time.Sleep(20 * time.Millisecond)
	if _, updates := sender.counts(); updates != 0 {
		t.Fatalf("closed session channel produced %d terminal updates, want 0", updates)
	}

	sup.applyConfigs(nil)
	callsSnapshot, _ := session.snapshot()
	foundReset := false
	for _, sec := range callsSnapshot {
		if sec == 0 {
			foundReset = true
		}
	}
	if !foundReset {
		t.Fatalf("timeout calls = %v, want a restore call with 0", callsSnapshot)
	}
}

func TestLarkSupervisorLoadsEnabledDevicesAcrossNamespaces(t *testing.T) {
	sender := &fakeCardSend{}
	sessions := map[string]*fakeDeviceSession{
		"alice/legion": newFakeDeviceSession(),
		"bob/build":    newFakeDeviceSession(),
	}
	sup, cancel := newTestLarkSupervisor(sender, func(_ context.Context, ns, device string) (deviceSession, error) {
		return sessions[larkDeviceKey(ns, device)], nil
	})
	defer cancel()
	var requested []string
	sup.adminReq = func(_ string, path string, query url.Values, _ any) (*http.Response, error) {
		requested = append(requested, path+"?"+query.Encode())
		body := `{"namespaces":["bob","alice"]}`
		if path == "/admin/devices" {
			switch query.Get("namespace") {
			case "alice":
				body = `{"devices":[{"name":"legion","owner":"alice","fingerprint":"SHA256:alice"}]}`
			case "bob":
				body = `{"devices":[{"name":"build","owner":"bob","fingerprint":"SHA256:bob"}]}`
			}
		} else if path == "/admin/devices/lark" {
			switch query.Get("namespace") {
			case "alice":
				body = `{"devices":[{"device":"legion","approval_enabled":true,"notify_email":"alice@example.com"}]}`
			case "bob":
				body = `{"devices":[{"device":"build","approval_enabled":true,"notify_email":"bob@example.com"},{"device":"off","approval_enabled":false,"notify_email":"bob@example.com"}]}`
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if err := sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, session := range sessions {
			calls, _ := session.snapshot()
			if len(calls) == 0 || calls[0] != 180 {
				return false
			}
		}
		return true
	})
	if strings.Join(requested, ",") != "/admin/users?,/admin/devices/lark?namespace=alice,/admin/devices/lark?namespace=bob" {
		t.Fatalf("relay requests = %v", requested)
	}
	sup.mu.Lock()
	aliceFingerprint := sup.watchers["alice/legion"].getConfig().RegisteredFingerprint
	bobFingerprint := sup.watchers["bob/build"].getConfig().RegisteredFingerprint
	sup.mu.Unlock()
	if aliceFingerprint != "" || bobFingerprint != "" {
		t.Fatalf("healthy watchers unexpectedly loaded registered fingerprints = %q, %q", aliceFingerprint, bobFingerprint)
	}
	sup.applyConfigs(nil)
}

func TestLarkActionHandlerAllowsAndRejectsInvalidGrants(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		sup, session, sender, nonce, cancel := actionFixture(t)
		defer cancel()
		reply := sup.actionHandler(context.Background(), lark.CardAction{
			Verdict: "g", Nonce: nonce, ChatID: "oc-owner", MessageID: "om-card", EventID: "event-1",
		})
		if reply.ToastText == "" {
			t.Fatal("empty success toast")
		}
		waitFor(t, func() bool {
			_, decisions := session.snapshot()
			return len(decisions) == 1
		})
		_, decisions := session.snapshot()
		if decisions[0].id != "approval-1" || decisions[0].verdict != "g" || decisions[0].approver != "lark:alice@example.com" {
			t.Fatalf("decision = %+v", decisions[0])
		}
		waitFor(t, func() bool {
			_, updates := sender.counts()
			return updates == 1
		})
	})

	t.Run("chat mismatch", func(t *testing.T) {
		sup, session, sender, nonce, cancel := actionFixture(t)
		defer cancel()
		reply := sup.actionHandler(context.Background(), lark.CardAction{
			Verdict: "y", Nonce: nonce, ChatID: "oc-forwarded", MessageID: "om-forward", EventID: "event-wrong-chat",
		})
		if reply.ToastText == "" {
			t.Fatal("empty chat-mismatch toast")
		}
		time.Sleep(10 * time.Millisecond)
		_, decisions := session.snapshot()
		if len(decisions) != 0 {
			t.Fatalf("chat mismatch made decisions: %+v", decisions)
		}
		// An unauthorized callback must not cause any write at all. Its
		// message ID is attacker-supplied, and the tenant token can edit every
		// message this (shared) Feishu app has ever sent.
		if _, updates := sender.counts(); updates != 0 {
			t.Fatalf("unauthorized callback wrote %d card updates, want 0", updates)
		}
	})

	t.Run("device decision failure resolves card", func(t *testing.T) {
		sup, session, sender, nonce, cancel := actionFixture(t)
		defer cancel()
		session.mu.Lock()
		session.decideErr = errors.New("pending no longer exists")
		session.mu.Unlock()
		reply := sup.actionHandler(context.Background(), lark.CardAction{
			Verdict: "y", Nonce: nonce, ChatID: "oc-owner", MessageID: "om-card", EventID: "event-stale",
		})
		if reply.ToastText == "" {
			t.Fatal("empty decision-failure toast")
		}
		waitFor(t, func() bool {
			_, updates := sender.counts()
			return updates == 1
		})
		raw, err := json.Marshal(sender.lastUpdateCard(t))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "请求已失效") {
			t.Fatalf("decision failure card stayed actionable: %s", raw)
		}
	})

	t.Run("nonce reuse", func(t *testing.T) {
		sup, session, _, nonce, cancel := actionFixture(t)
		defer cancel()
		first := lark.CardAction{Verdict: "y", Nonce: nonce, ChatID: "oc-owner", MessageID: "om-card", EventID: "event-first"}
		if reply := sup.actionHandler(context.Background(), first); reply.ToastText == "" {
			t.Fatal("empty first toast")
		}
		waitFor(t, func() bool {
			_, decisions := session.snapshot()
			return len(decisions) == 1
		})
		first.EventID = "event-reuse"
		if reply := sup.actionHandler(context.Background(), first); reply.ToastText == "" {
			t.Fatal("empty nonce-reuse toast")
		}
		time.Sleep(10 * time.Millisecond)
		_, decisions := session.snapshot()
		if len(decisions) != 1 {
			t.Fatalf("nonce reuse made %d decisions, want 1", len(decisions))
		}
	})

	t.Run("unknown nonce", func(t *testing.T) {
		sup, session, _, _, cancel := actionFixture(t)
		defer cancel()
		reply := sup.actionHandler(context.Background(), lark.CardAction{
			Verdict: "y", Nonce: "unknown", ChatID: "oc-owner", MessageID: "om-card", EventID: "event-unknown",
		})
		if reply.ToastText == "" {
			t.Fatal("empty unknown-nonce toast")
		}
		time.Sleep(10 * time.Millisecond)
		_, decisions := session.snapshot()
		if len(decisions) != 0 {
			t.Fatalf("unknown nonce made decisions: %+v", decisions)
		}
	})
}

func TestLarkActionHandlerRoutesPairingGrant(t *testing.T) {
	sender := &fakeCardSend{}
	session := newFakeDeviceSession()
	sup, cancelContext := newTestLarkSupervisor(sender, func(context.Context, string, string) (deviceSession, error) {
		return session, nil
	})
	defer func() {
		sup.actionMu.Lock()
		sup.closing = true
		sup.actionMu.Unlock()
		cancelContext()
		sup.wg.Wait()
	}()
	grant, err := sup.grants.Issue("alice", "legion", "", "SHA256:controller", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pairing := console.PendingPairing{FP: "SHA256:controller", Name: "build", Label: "release"}
	sup.storeAction(grant.Nonce, larkActionRecord{
		ns: "alice", device: "legion", email: "alice@example.com", messageID: "om-pair",
		expiresAt: grant.ExpiresAt, pairing: &pairing,
	})
	reply := sup.actionHandler(context.Background(), lark.CardAction{
		Verdict: "y", Nonce: grant.Nonce, ChatID: "oc-owner", MessageID: "om-pair", EventID: "event-pair",
	})
	if reply.ToastText == "" {
		t.Fatal("empty pairing toast")
	}
	waitFor(t, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return len(session.pairings) == 1
	})
	session.mu.Lock()
	got := session.pairings[0]
	decisions := len(session.decisions)
	session.mu.Unlock()
	if got.id != pairing.FP || got.verdict != "y" {
		t.Fatalf("pairing decision = %+v", got)
	}
	if decisions != 0 {
		t.Fatalf("pairing grant routed through ordinary decide %d times", decisions)
	}
}

func actionFixture(t *testing.T) (*larkSupervisor, *fakeDeviceSession, *fakeCardSend, string, context.CancelFunc) {
	t.Helper()
	sender := &fakeCardSend{}
	session := newFakeDeviceSession()
	sup, cancelContext := newTestLarkSupervisor(sender, func(context.Context, string, string) (deviceSession, error) {
		return session, nil
	})
	grant, err := sup.grants.Issue("alice", "legion", "approval-1", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pending := console.Pending{ID: "approval-1", Kind: "exec", Cmd: "echo ok"}
	sup.storeAction(grant.Nonce, larkActionRecord{
		ns: "alice", device: "legion", email: "alice@example.com", messageID: "om-card",
		expiresAt: grant.ExpiresAt, pending: &pending,
	})
	cancel := func() {
		sup.actionMu.Lock()
		sup.closing = true
		sup.actionMu.Unlock()
		cancelContext()
		sup.wg.Wait()
	}
	return sup, session, sender, grant.Nonce, cancel
}

func cardNonce(t *testing.T, card any) string {
	t.Helper()
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var nonce string
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if value["type"] == "callback" {
				if payload, ok := value["value"].(map[string]any); ok {
					nonce, _ = payload["n"].(string)
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(root)
	if nonce == "" {
		t.Fatal("sent card has no callback nonce")
	}
	return nonce
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

// A Lark card is rendered inside a Lark client, which has no origin to resolve a
// relative path against: the "open in portal" button just does nothing when
// tapped. PORTAL_PUBLIC_ORIGIN is optional, so the unset case must still yield an
// absolute URL. This was live: portal ran without PORTAL_PUBLIC_ORIGIN and every
// card shipped a dead button.
func TestDeviceURLIsAlwaysAbsolute(t *testing.T) {
	for _, origin := range []string{"", "https://wanctl.example.com"} {
		s := &larkSupervisor{portalOrigin: origin}
		got := s.deviceURL("vpn box")
		if !strings.HasPrefix(got, "https://") {
			t.Fatalf("portalOrigin=%q: deviceURL = %q, want an absolute https URL", origin, got)
		}
		if !strings.HasSuffix(got, "/#device/vpn%20box") {
			t.Fatalf("portalOrigin=%q: deviceURL = %q, want the escaped device path", origin, got)
		}
	}
}
