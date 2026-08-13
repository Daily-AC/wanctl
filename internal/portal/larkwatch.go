package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"wanctl/internal/config"
	"wanctl/internal/console"
	"wanctl/internal/lark"
)

const (
	larkReconcileInterval = 30 * time.Second
	larkApprovalWait      = 180 * time.Second
	larkPairingWait       = 5 * time.Minute
	larkRetryMin          = time.Second
	larkRetryMax          = 30 * time.Second
	larkHealthErrorRunes  = 512
	// larkDialLogEvery throttles the "cannot reach device" line after the first
	// one: at the ceiling backoff that is roughly one line every five minutes.
	larkDialLogEvery = 10
)

// cardSender is the complete outbound Feishu surface used by the orchestrator.
type cardSender interface {
	SendCard(ctx context.Context, email string, card any) (messageID, chatID string, err error)
	UpdateCard(ctx context.Context, messageID string, card any) error
}

// deviceSession is deliberately limited to the resident-session operations the
// Feishu workflow needs. *deviceConn satisfies it directly.
type deviceSession interface {
	subscribe() (<-chan console.State, func())
	decide(id, verdict, approver string) error
	pairDecide(fp, verdict string) error
	setApprovalTimeout(sec int) (int, error)
}

type sessionForFunc func(ctx context.Context, ns, device string) (deviceSession, error)
type adminRequestFunc func(method, path string, query url.Values, body any) (*http.Response, error)

type larkSupervisor struct {
	sender       cardSender
	grants       *lark.Grants
	sessionFor   sessionForFunc
	adminReq     adminRequestFunc
	portalOrigin string
	logf         func(string, ...any)

	reconcileEvery time.Duration
	retryMin       time.Duration
	retryMax       time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	watchers map[string]*larkWatcher
	wake     chan struct{}

	actionMu sync.Mutex
	actions  map[string]larkActionRecord
	closing  bool

	healthMu sync.RWMutex
	health   map[string]larkDeliveryHealth

	wg sync.WaitGroup
}

type larkRuntime struct {
	cancel     context.CancelFunc
	supervisor *larkSupervisor
	consumerWG sync.WaitGroup
}

type larkActionRecord struct {
	ns        string
	device    string
	email     string
	messageID string
	expiresAt time.Time
	pending   *console.Pending
	pairing   *console.PendingPairing

	inFlight       bool
	naturalClaimed bool
	naturalDone    chan struct{}
	waitNatural    <-chan struct{}
}

type larkDeliveryHealth struct {
	AttemptedAt         time.Time `json:"attempted_at"`
	Result              string    `json:"result"`
	Kind                string    `json:"kind"`
	Error               string    `json:"error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

type larkWatcher struct {
	sup    *larkSupervisor
	ns     string
	device string

	mu     sync.RWMutex
	config deviceLarkApproval
	cancel context.CancelFunc
	done   chan struct{}
}

type seenPending struct {
	pending   console.Pending
	nonce     string
	messageID string
}

type seenPairing struct {
	pairing   console.PendingPairing
	nonce     string
	messageID string
}

func newLarkSupervisor(s *Server, sender cardSender, grants *lark.Grants) *larkSupervisor {
	return &larkSupervisor{
		sender: sender,
		grants: grants,
		sessionFor: func(ctx context.Context, ns, device string) (deviceSession, error) {
			return s.deviceConnFor(ctx, ns, device)
		},
		adminReq:       s.adminReq,
		portalOrigin:   s.publicOrigin,
		logf:           log.Printf,
		reconcileEvery: larkReconcileInterval,
		retryMin:       larkRetryMin,
		retryMax:       larkRetryMax,
		watchers:       make(map[string]*larkWatcher),
		actions:        make(map[string]larkActionRecord),
		health:         make(map[string]larkDeliveryHealth),
		wake:           make(chan struct{}, 1),
	}
}

// Start enables the Feishu workflow when both wanctl-scoped credentials are
// present. Missing credentials and an untagged binary are deployment choices,
// not reasons for the portal's existing HTTP surface to fail.
func (s *Server) Start(parent context.Context) {
	s.larkMu.Lock()
	if s.larkStarted {
		s.larkMu.Unlock()
		return
	}
	s.larkStarted = true
	appID := os.Getenv("WANCTL_LARK_APP_ID")
	appSecret := os.Getenv("WANCTL_LARK_APP_SECRET")
	if appID == "" || appSecret == "" {
		s.larkMu.Unlock()
		log.Printf("lark approval disabled: WANCTL_LARK_APP_ID and WANCTL_LARK_APP_SECRET are not both configured")
		return
	}

	ctx, cancel := context.WithCancel(parent)
	sender := lark.NewClient(appID, appSecret)
	supervisor := newLarkSupervisor(s, sender, lark.NewGrants())
	runtime := &larkRuntime{cancel: cancel, supervisor: supervisor}
	consumer, consumerErr := lark.NewConsumer(appID, appSecret, supervisor.actionHandler)
	supervisor.start(ctx)
	if consumerErr == nil {
		runtime.consumerWG.Add(1)
	}
	s.larkRuntime = runtime
	s.larkMu.Unlock()

	if consumerErr != nil {
		log.Printf("lark callback consumer disabled: %v", consumerErr)
		return
	}
	go func() {
		defer runtime.consumerWG.Done()
		if err := consumer.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("lark callback consumer stopped: %v", err)
		}
	}()
}

// Close stops every Feishu goroutine and closes pooled console sessions. It is
// safe to call when Feishu was disabled or after the parent context was canceled.
func (s *Server) Close() {
	s.larkMu.Lock()
	runtime := s.larkRuntime
	s.larkRuntime = nil
	s.larkMu.Unlock()
	if runtime != nil {
		runtime.cancel()
		runtime.supervisor.stop()
		runtime.supervisor.wait()
		runtime.consumerWG.Wait()
	}

	s.mu.Lock()
	for key, conn := range s.conns {
		conn.close()
		delete(s.conns, key)
	}
	s.mu.Unlock()
}

func (s *Server) triggerLarkReconcile() {
	s.larkMu.Lock()
	runtime := s.larkRuntime
	s.larkMu.Unlock()
	if runtime != nil {
		runtime.supervisor.triggerReconcile()
	}
}

func (s *larkSupervisor) start(parent context.Context) {
	s.ctx, s.cancel = context.WithCancel(parent)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runReconciler()
	}()
}

func (s *larkSupervisor) stop() {
	s.actionMu.Lock()
	s.closing = true
	s.actionMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *larkSupervisor) wait() { s.wg.Wait() }

func (s *larkSupervisor) triggerReconcile() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *larkSupervisor) runReconciler() {
	ticker := time.NewTicker(s.reconcileEvery)
	defer ticker.Stop()
	defer s.cancelAllWatchers()

	if err := s.reconcile(); err != nil {
		s.logf("lark approval reconcile: %v", err)
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		if err := s.reconcile(); err != nil {
			s.logf("lark approval reconcile: %v", err)
		}
	}
}

func (s *larkSupervisor) reconcile() error {
	configs, err := s.loadConfigs()
	if err != nil {
		return err
	}
	s.applyConfigs(configs)
	return nil
}

// loadConfigs first enumerates namespaces because the relay's lark endpoint is
// intentionally namespace-scoped and omits the namespace from its JSON rows.
func (s *larkSupervisor) loadConfigs() ([]deviceLarkApproval, error) {
	resp, err := s.adminReq(http.MethodGet, "/admin/users", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("list namespaces", resp)
	}
	var users struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode namespaces: %w", err)
	}
	sort.Strings(users.Namespaces)

	configs := make([]deviceLarkApproval, 0)
	for _, ns := range users.Namespaces {
		resp, err := s.adminReq(http.MethodGet, "/admin/devices/lark", url.Values{"namespace": {ns}}, nil)
		if err != nil {
			return nil, fmt.Errorf("list lark devices for %q: %w", ns, err)
		}
		var out struct {
			Devices []deviceLarkApproval `json:"devices"`
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&out)
		} else {
			err = responseError("list lark devices for "+ns, resp)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for i := range out.Devices {
			out.Devices[i].Namespace = ns
			if out.Devices[i].ApprovalEnabled {
				configs = append(configs, out.Devices[i])
			}
		}
	}
	return configs, nil
}

func responseError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: relay returned %d: %s", op, resp.StatusCode, string(body))
}

func larkDeviceKey(ns, device string) string { return ns + "/" + device }

func (s *larkSupervisor) recordHealth(ns, device, kind string, err error) {
	key := larkDeviceKey(ns, device)
	s.healthMu.Lock()
	health := s.health[key]
	// A successful reconnect only resolves a dial failure. It must not make a
	// failed card delivery look healthy before another card is actually sent.
	if kind == "dial" && err == nil && health.Kind != "" && health.Kind != "dial" {
		s.healthMu.Unlock()
		return
	}
	health.AttemptedAt = time.Now().UTC()
	health.Kind = kind
	if err == nil {
		health.Result = "success"
		health.Error = ""
		health.ConsecutiveFailures = 0
	} else {
		health.Result = "failure"
		health.Error = truncateLarkHealthError(err.Error())
		health.ConsecutiveFailures++
	}
	if s.health == nil {
		s.health = make(map[string]larkDeliveryHealth)
	}
	s.health[key] = health
	s.healthMu.Unlock()
}

func (s *larkSupervisor) deliveryHealth(ns, device string) *larkDeliveryHealth {
	s.healthMu.RLock()
	health, ok := s.health[larkDeviceKey(ns, device)]
	s.healthMu.RUnlock()
	if !ok {
		return nil
	}
	return &health
}

func (s *larkSupervisor) clearHealth(key string) {
	s.healthMu.Lock()
	delete(s.health, key)
	s.healthMu.Unlock()
}

func truncateLarkHealthError(message string) string {
	runes := []rune(message)
	if len(runes) <= larkHealthErrorRunes {
		return message
	}
	return string(runes[:larkHealthErrorRunes]) + "..."
}

func (s *larkSupervisor) applyConfigs(configs []deviceLarkApproval) {
	desired := make(map[string]deviceLarkApproval, len(configs))
	for _, cfg := range configs {
		if cfg.Namespace != "" && cfg.Device != "" && cfg.NotifyEmail != "" && cfg.ApprovalEnabled {
			desired[larkDeviceKey(cfg.Namespace, cfg.Device)] = cfg
		}
	}

	s.mu.Lock()
	var stopping []*larkWatcher
	var stoppingKeys []string
	for key, watcher := range s.watchers {
		cfg, ok := desired[key]
		if !ok {
			delete(s.watchers, key)
			stopping = append(stopping, watcher)
			stoppingKeys = append(stoppingKeys, key)
			continue
		}
		watcher.setConfig(cfg)
		delete(desired, key)
	}
	for key, cfg := range desired {
		ctx, cancel := context.WithCancel(s.ctx)
		watcher := &larkWatcher{
			sup: s, ns: cfg.Namespace, device: cfg.Device,
			config: cfg, cancel: cancel, done: make(chan struct{}),
		}
		s.watchers[key] = watcher
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer close(watcher.done)
			watcher.run(ctx)
		}()
	}
	s.mu.Unlock()

	for i, watcher := range stopping {
		watcher.cancel()
		<-watcher.done
		s.clearHealth(stoppingKeys[i])
	}
	s.pruneActions(time.Now())
}

func (s *larkSupervisor) cancelAllWatchers() {
	s.mu.Lock()
	watchers := make([]*larkWatcher, 0, len(s.watchers))
	for key, watcher := range s.watchers {
		delete(s.watchers, key)
		watchers = append(watchers, watcher)
	}
	s.mu.Unlock()
	for _, watcher := range watchers {
		watcher.cancel()
	}
	for _, watcher := range watchers {
		<-watcher.done
	}
}

func (w *larkWatcher) setConfig(cfg deviceLarkApproval) {
	w.mu.Lock()
	w.config = cfg
	w.mu.Unlock()
}

func (w *larkWatcher) getConfig() deviceLarkApproval {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config
}

func (w *larkWatcher) run(ctx context.Context) {
	seenPending := make(map[string]seenPending)
	seenPairings := make(map[string]seenPairing)
	backoff := w.sup.retryMin
	dialFailures := 0
	var lastSession deviceSession
	defer func() {
		if lastSession != nil {
			if _, err := lastSession.setApprovalTimeout(0); err != nil {
				w.sup.logf("lark approval restore timeout for %s: %v", larkDeviceKey(w.ns, w.device), err)
			}
		}
	}()

	for {
		session, err := w.sup.sessionFor(ctx, w.ns, w.device)
		if err != nil {
			w.sup.recordHealth(w.ns, w.device, "dial", err)
			// Say something. An agent started without the portal's admin
			// fingerprint refuses the console dial, and this loop would then
			// retry forever in complete silence: the switch reads "on", no card
			// is ever sent, and nothing anywhere names the cause. Log the first
			// failure immediately — that is the one somebody is waiting for —
			// then throttle, so a device that stays unreachable does not drown
			// the log.
			dialFailures++
			if dialFailures == 1 || dialFailures%larkDialLogEvery == 0 {
				w.sup.logf("lark approval cannot reach %s (dial attempt %d): %v",
					larkDeviceKey(w.ns, w.device), dialFailures, err)
			}
			if !waitContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, w.sup.retryMax)
			continue
		}
		dialFailures = 0
		w.sup.recordHealth(w.ns, w.device, "dial", nil)
		lastSession = session
		// A device older than the timeout_set verb answers "unknown RPC kind".
		// That must not stop us watching it: carrying on with whatever wait the
		// device already uses degrades the feature to a shorter window, whereas
		// treating it as a dial failure would make Feishu approvals silently
		// never work on every agent that has not been upgraded yet.
		wait := larkApprovalWait
		if applied, err := session.setApprovalTimeout(int(larkApprovalWait / time.Second)); err != nil {
			wait = console.DefaultTimeout
			w.sup.logf("lark approval timeout for %s not set, using the device default %s: %v",
				larkDeviceKey(w.ns, w.device), wait, err)
		} else if applied > 0 {
			// The device clamps, so the card must state what it actually applied
			// rather than what we asked for.
			wait = time.Duration(applied) * time.Second
		}

		states, unsubscribe := session.subscribe()
		backoff = w.sup.retryMin
		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				unsubscribe()
				return
			case state, ok := <-states:
				if !ok {
					closed = true
					continue
				}
				w.reconcileState(ctx, state, seenPending, seenPairings, wait)
			}
		}
		unsubscribe()
		// A closed channel means visibility was lost, not that pending work
		// disappeared. Preserve both seen maps across the bounded re-dial loop.
		if !waitContext(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, w.sup.retryMax)
	}
}

func (w *larkWatcher) reconcileState(ctx context.Context, state console.State, pendingSeen map[string]seenPending, pairingSeen map[string]seenPairing, wait time.Duration) {
	cfg := w.getConfig()
	currentPending := make(map[string]console.Pending, len(state.Pending))
	for _, pending := range state.Pending {
		currentPending[pending.ID] = pending
		if _, ok := pendingSeen[pending.ID]; ok {
			continue
		}
		grant, err := w.sup.grants.Issue(w.ns, w.device, pending.ID, "", "", wait)
		if err != nil {
			w.sup.recordHealth(w.ns, w.device, "approval", err)
			w.sup.logf("lark approval issue grant for %s/%s: %v", larkDeviceKey(w.ns, w.device), pending.ID, err)
			continue
		}
		messageID, chatID, err := w.sup.sender.SendCard(ctx, cfg.NotifyEmail,
			lark.ApprovalCard(w.device, pending, grant.Nonce, w.sup.deviceURL(w.device), wait))
		if err != nil {
			w.sup.recordHealth(w.ns, w.device, "approval", err)
			w.sup.logf("lark approval send card for %s/%s: %v", larkDeviceKey(w.ns, w.device), pending.ID, err)
			continue
		}
		w.sup.recordHealth(w.ns, w.device, "approval", nil)
		pendingSeen[pending.ID] = seenPending{pending: pending, nonce: grant.Nonce, messageID: messageID}
		if _, err := w.sup.grants.BindChat(grant.Nonce, chatID); err != nil {
			w.sup.logf("lark approval bind card for %s/%s: %v", larkDeviceKey(w.ns, w.device), pending.ID, err)
			_ = w.sup.sender.UpdateCard(ctx, messageID, lark.ActionFailedCard("卡片授权绑定失败，请返回门户处理"))
			continue
		}
		copy := pending
		w.sup.storeAction(grant.Nonce, larkActionRecord{
			ns: w.ns, device: w.device, email: cfg.NotifyEmail, messageID: messageID,
			expiresAt: grant.ExpiresAt, pending: &copy,
		})
	}
	for id, seen := range pendingSeen {
		if _, ok := currentPending[id]; ok {
			continue
		}
		if w.sup.claimNaturalUpdate(seen.nonce) {
			err := w.sup.sender.UpdateCard(ctx, seen.messageID,
				lark.ResolvedCard(w.device, seen.pending, "已结束（已处理或超时）", "设备"))
			w.sup.finishNaturalUpdate(seen.nonce)
			if err != nil {
				w.sup.logf("lark approval resolve card for %s/%s: %v", larkDeviceKey(w.ns, w.device), id, err)
			}
		}
		delete(pendingSeen, id)
	}

	currentPairings := make(map[string]console.PendingPairing, len(state.PendingPairings))
	for _, pairing := range state.PendingPairings {
		currentPairings[pairing.FP] = pairing
		if _, ok := pairingSeen[pairing.FP]; ok {
			continue
		}
		grant, err := w.sup.grants.Issue(w.ns, w.device, "", pairing.FP, "", larkPairingWait)
		if err != nil {
			w.sup.recordHealth(w.ns, w.device, "pairing", err)
			w.sup.logf("lark pairing issue grant for %s/%s: %v", larkDeviceKey(w.ns, w.device), pairing.FP, err)
			continue
		}
		messageID, chatID, err := w.sup.sender.SendCard(ctx, cfg.NotifyEmail,
			lark.PairingCard(w.device, pairing, grant.Nonce, w.sup.deviceURL(w.device), cfg.PairingFromCard))
		if err != nil {
			w.sup.recordHealth(w.ns, w.device, "pairing", err)
			w.sup.logf("lark pairing send card for %s/%s: %v", larkDeviceKey(w.ns, w.device), pairing.FP, err)
			continue
		}
		w.sup.recordHealth(w.ns, w.device, "pairing", nil)
		pairingSeen[pairing.FP] = seenPairing{pairing: pairing, nonce: grant.Nonce, messageID: messageID}
		if _, err := w.sup.grants.BindChat(grant.Nonce, chatID); err != nil {
			w.sup.logf("lark pairing bind card for %s/%s: %v", larkDeviceKey(w.ns, w.device), pairing.FP, err)
			_ = w.sup.sender.UpdateCard(ctx, messageID, lark.ActionFailedCard("卡片授权绑定失败，请返回门户处理"))
			continue
		}
		copy := pairing
		w.sup.storeAction(grant.Nonce, larkActionRecord{
			ns: w.ns, device: w.device, email: cfg.NotifyEmail, messageID: messageID,
			expiresAt: grant.ExpiresAt, pairing: &copy,
		})
	}
	for fp, seen := range pairingSeen {
		if _, ok := currentPairings[fp]; ok {
			continue
		}
		if w.sup.claimNaturalUpdate(seen.nonce) {
			err := w.sup.sender.UpdateCard(ctx, seen.messageID,
				lark.ResolvedPairingCard(w.device, seen.pairing, "已结束（已处理或超时）", "设备"))
			w.sup.finishNaturalUpdate(seen.nonce)
			if err != nil {
				w.sup.logf("lark pairing resolve card for %s/%s: %v", larkDeviceKey(w.ns, w.device), fp, err)
			}
		}
		delete(pairingSeen, fp)
	}
	w.sup.pruneActions(time.Now())
}

// deviceURL builds the absolute link a Lark card's "open in portal" button
// carries. It must be absolute: the card is rendered inside someone's Lark
// client, which has no origin to resolve a path against, so a relative URL makes
// the button silently do nothing when tapped. PORTAL_PUBLIC_ORIGIN is optional
// (it only matters when TLS terminates upstream), so fall back to the compiled-in
// portal address rather than emitting a path that cannot work.
func (s *larkSupervisor) deviceURL(device string) string {
	origin := s.portalOrigin
	if origin == "" {
		origin = strings.TrimRight(config.DefaultPortal, "/")
	}
	return origin + "/#device/" + url.PathEscape(device)
}

func (s *larkSupervisor) storeAction(nonce string, record larkActionRecord) {
	s.actionMu.Lock()
	record.naturalDone = make(chan struct{})
	s.actions[nonce] = record
	s.actionMu.Unlock()
}

// claimNaturalUpdate arbitrates the only card-update race in this workflow.
// A callback already driving the decision owns the terminal update; otherwise
// the watcher claims it and a concurrent callback waits for that generic update
// before replacing it with the precise decision result.
func (s *larkSupervisor) claimNaturalUpdate(nonce string) bool {
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	record, ok := s.actions[nonce]
	if !ok {
		return true
	}
	if record.inFlight {
		return false
	}
	record.naturalClaimed = true
	s.actions[nonce] = record
	return true
}

func (s *larkSupervisor) finishNaturalUpdate(nonce string) {
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	record, ok := s.actions[nonce]
	if !ok || !record.naturalClaimed || record.naturalDone == nil {
		return
	}
	close(record.naturalDone)
	record.naturalDone = nil
	s.actions[nonce] = record
}

func (s *larkSupervisor) forgetAction(nonce string) {
	s.actionMu.Lock()
	delete(s.actions, nonce)
	s.actionMu.Unlock()
}

func (s *larkSupervisor) pruneActions(now time.Time) {
	s.actionMu.Lock()
	for nonce, record := range s.actions {
		if !now.Before(record.expiresAt) {
			delete(s.actions, nonce)
		}
	}
	s.actionMu.Unlock()
}

func (s *larkSupervisor) actionHandler(ctx context.Context, action lark.CardAction) lark.CardReply {
	grant, err := s.grants.Consume(action.Nonce, action.ChatID, action.EventID)
	if err != nil {
		// Toast only, deliberately no card update. The message ID would have to
		// come from the callback itself (we have no verified record for a nonce
		// that failed authorization), and the tenant token can edit *any*
		// message this Feishu app ever sent — including other systems' messages,
		// since this app is shared. Rewriting an arbitrary message on the
		// strength of an unauthorized callback is not a trade worth making for
		// cosmetics; the toast already tells the person what happened.
		return lark.CardReply{ToastText: grantFailureToast(err)}
	}

	s.actionMu.Lock()
	record, ok := s.actions[action.Nonce]
	if ok {
		record.inFlight = true
		if record.naturalClaimed && record.naturalDone != nil {
			record.waitNatural = record.naturalDone
		}
		s.actions[action.Nonce] = record
	}
	s.actionMu.Unlock()
	if !ok {
		// Same reasoning as above: no verified record, so no message ID we are
		// willing to write to.
		return lark.CardReply{ToastText: "该审批已失效，请返回门户查看当前状态"}
	}

	if !s.launch(func(ctx context.Context) { s.resolveAction(ctx, action, grant, record) }) {
		return lark.CardReply{ToastText: "服务正在关闭，请返回门户处理"}
	}
	if action.Verdict == "n" {
		return lark.CardReply{ToastText: "已收到，正在拒绝"}
	}
	return lark.CardReply{ToastText: "已收到，正在处理"}
}

func grantFailureToast(err error) string {
	switch {
	case errors.Is(err, lark.ErrChatMismatch):
		return "这张卡不属于当前会话，操作已拒绝"
	case errors.Is(err, lark.ErrNonceConsumed):
		return "该审批已处理，请勿重复操作"
	case errors.Is(err, lark.ErrGrantExpired):
		return "该审批已过期，请返回门户查看当前状态"
	case errors.Is(err, lark.ErrEventDuplicate):
		return "该操作已收到，请勿重复点击"
	case errors.Is(err, lark.ErrGrantUnbound):
		return "卡片尚未完成授权绑定，请返回门户处理"
	case errors.Is(err, lark.ErrGrantNotFound):
		return "该审批已失效或服务已重启"
	default:
		return "卡片授权无效，操作已拒绝"
	}
}

func (s *larkSupervisor) launch(fn func(context.Context)) bool {
	s.actionMu.Lock()
	if s.closing || s.ctx == nil {
		s.actionMu.Unlock()
		return false
	}
	s.wg.Add(1)
	ctx := s.ctx
	s.actionMu.Unlock()
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
	return true
}

func (s *larkSupervisor) resolveAction(ctx context.Context, action lark.CardAction, grant lark.Grant, record larkActionRecord) {
	session, err := s.sessionFor(ctx, grant.NS, grant.Device)
	actor := "lark:" + record.email
	result := "已处理"
	if err == nil {
		if grant.PendingID != "" {
			err = session.decide(grant.PendingID, action.Verdict, actor)
			if action.Verdict == "n" {
				result = "已拒绝"
			} else {
				result = "已允许"
			}
		} else if grant.PairingFP != "" {
			err = session.pairDecide(grant.PairingFP, action.Verdict)
			if action.Verdict == "y" {
				result = "已信任"
			} else {
				result = "已拒绝"
			}
		} else {
			err = errors.New("callback target is missing")
		}
	}
	if err != nil {
		result = "请求已失效（设备未接受决策）"
		actor = "系统"
		s.logf("lark callback decision for %s: %v", larkDeviceKey(grant.NS, grant.Device), err)
	}
	if record.waitNatural != nil {
		select {
		case <-ctx.Done():
			return
		case <-record.waitNatural:
		}
	}

	var card any
	if record.pending != nil {
		card = lark.ResolvedCard(grant.Device, *record.pending, result, actor)
	} else {
		card = lark.ResolvedPairingCard(grant.Device, *record.pairing, result, actor)
	}
	// record.messageID, not action.MessageID: the target of a write must come
	// from what we recorded when we sent the card, never from the callback.
	if err := s.sender.UpdateCard(ctx, record.messageID, card); err != nil {
		s.logf("lark callback result card update for %s: %v", larkDeviceKey(grant.NS, grant.Device), err)
	}
	s.forgetAction(action.Nonce)
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}
