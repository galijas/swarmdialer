package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"swarmdialer/internal/sipua"
)

// SessionConfig configures a local Session (single PBXware server, callers
// and callees are the same pool of extensions dialing each other directly
// by extension number). For a remote (cross-server) session, use
// NewRemoteSession instead.
type SessionConfig struct {
	DialDestination string
	SIPDomain       string
	LocalIP         string
	BaseLocalPort   int
	RegisterTimeout time.Duration
	DialTimeout     time.Duration
	// OnRegisterProgress, if non-nil, is called as each extension finishes
	// its initial registration (see RegisterProgressFunc).
	OnRegisterProgress RegisterProgressFunc
}

// RegisterProgressFunc reports a pool's initial registration progress:
// how many of total extensions have registered and how many failed so far.
// side is "" for a local session's single pool, or "caller"/"callee" for a
// remote session's two. Called from many goroutines at once; it must be
// safe for concurrent use and cheap.
type RegisterProgressFunc func(side string, registered, failed, total int)

// pool is one registered, auto-answering set of extensions on one PBXware
// server. A local Session has one pool shared as both caller and callee;
// a remote Session has two separate pools, one per server.
type pool struct {
	phones          map[string]*sipua.Phone // keyed by AOR
	available       map[string]bool         // AOR -> currently free for a new call
	dialDestination string
	sipDomain       string
}

func newPool(ctx context.Context, localIP string, basePort int, registerTimeout time.Duration, dialDestination, sipDomain string, endpoints []sipua.Endpoint, progress func(registered, failed, total int)) (*pool, error) {
	if sipDomain == "" {
		sipDomain = dialDestination
	}

	phones := make(map[string]*sipua.Phone, len(endpoints))
	order := make([]string, 0, len(endpoints))
	for i, ep := range endpoints {
		phone, err := sipua.NewPhone(ctx, localIP, basePort+i, ep)
		if err != nil {
			return nil, fmt.Errorf("setting up phone for %s: %w", ep.AOR, err)
		}
		phones[ep.AOR] = phone
		order = append(order, ep.AOR)
	}

	if err := registerPhonesStaggered(ctx, registerTimeout, dialDestination, sipDomain, phones, order, progress); err != nil {
		return nil, err
	}
	log.Printf("session: %d extensions registered against %s", len(phones), dialDestination)

	available := make(map[string]bool, len(phones))
	for aor, phone := range phones {
		phone.AutoAnswer(ctx)
		available[aor] = true
	}

	// A Session (unlike the old one-shot Run()) stays alive indefinitely —
	// that's the whole point of the additive +N buttons. Without a refresh,
	// every phone's REGISTER binding (300s below) silently expires and every
	// subsequent call to/from it starts failing with 603 Decline, even
	// though nothing else changed. Stagger the refreshes the same way the
	// initial registration was staggered, to avoid re-creating the exact
	// REGISTER-burst problem registerPhonesStaggered exists to avoid.
	const registerExpiry = 300
	const refreshStagger = 20 * time.Millisecond
	refreshInterval := time.Duration(float64(registerExpiry)*0.8) * time.Second
	for i, aor := range order {
		go keepPhoneRegistered(ctx, phones[aor], dialDestination, sipDomain, registerTimeout, registerExpiry, refreshInterval+time.Duration(i)*refreshStagger)
	}

	return &pool{phones: phones, available: available, dialDestination: dialDestination, sipDomain: sipDomain}, nil
}

// registerPhonesStaggered registers every phone with a small stagger
// between each (see orchestrator.go's registerAll for why — PBXware
// rejects a chunk of otherwise-valid REGISTERs when hit fully
// concurrently).
func registerPhonesStaggered(ctx context.Context, timeout time.Duration, dialDestination, sipDomain string, phones map[string]*sipua.Phone, order []string, progress func(registered, failed, total int)) error {
	const registerStagger = 20 * time.Millisecond

	var registered, failedCount atomic.Int64
	total := len(order)
	report := func() {
		if progress != nil {
			progress(int(registered.Load()), int(failedCount.Load()), total)
		}
	}
	report() // 0/total, so the caller can show registration has started

	var wg sync.WaitGroup
	errs := make([]error, len(order))
	for i, aor := range order {
		wg.Add(1)
		go func(i int, phone *sipua.Phone) {
			defer wg.Done()
			select {
			case <-time.After(time.Duration(i) * registerStagger):
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			regCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if _, err := phone.Register(regCtx, dialDestination, sipDomain, 300); err != nil {
				errs[i] = fmt.Errorf("registering %s: %w", phone.Endpoint.AOR, err)
				failedCount.Add(1)
			} else {
				registered.Add(1)
			}
			report()
		}(i, phones[aor])
	}
	wg.Wait()

	var failed []error
	for _, err := range errs {
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d/%d registrations failed, first error: %w", len(failed), len(order), failed[0])
	}
	return nil
}

// keepPhoneRegistered re-registers phone every ~80% of expiry until ctx is
// canceled, first waiting firstDelay (see newPool — staggered so many
// phones don't all refresh in the same instant).
func keepPhoneRegistered(ctx context.Context, phone *sipua.Phone, dialDestination, sipDomain string, registerTimeout time.Duration, expiry int, firstDelay time.Duration) {
	delay := firstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		regCtx, cancel := context.WithTimeout(ctx, registerTimeout)
		_, err := phone.Register(regCtx, dialDestination, sipDomain, expiry)
		cancel()
		if err != nil {
			log.Printf("session: re-registering %s failed: %v", phone.Endpoint.AOR, err)
		}
		delay = time.Duration(float64(expiry)*0.8) * time.Second
	}
}

// activeCall is one ongoing call tracked internally by a Session.
type activeCall struct {
	id                   string
	callerAOR, calleeAOR string
	call                 *sipua.Call
	startedAt            time.Time
	codec                sipua.Codec
	setupLatency         time.Duration // INVITE-to-200-OK wall-clock time (Phone.Dial's own duration)
}

// CallRecord is one completed call's full detail — captured at the moment
// it ends, before its activeCall entry is discarded. This is what feeds
// both per-batch MOS/latency aggregation and the per-batch log file (see
// cmd/gui/handlers.go and the (future) logging package) — kept separate
// from the live-status Event type since a log line needs much more detail
// than the dashboard's log view does.
type CallRecord struct {
	CallerAOR    string
	CalleeAOR    string
	Codec        sipua.Codec
	StartedAt    time.Time
	EndedAt      time.Time
	Answered     bool
	FailReason   string
	SetupLatency time.Duration
	RTPSent      uint64
	RTPRecv      uint64
}

// Event is one point-in-time occurrence in a call's lifecycle (dialing,
// answered, failed, ended), for the dashboard's live-status log — plain
// aggregate counters don't say *which* extensions did what, which is what
// makes the log useful for spotting a pattern like a specific extension
// always failing, or a burst of near-simultaneous 603s.
type Event struct {
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"` // "dialing", "answered", "failed", "ended"
	CallerAOR string    `json:"caller_aor"`
	CalleeAOR string    `json:"callee_aor"`
	Detail    string    `json:"detail,omitempty"` // e.g. the dial error for "failed"
}

const maxRecentEvents = 200

// CallInfo is a point-in-time, serializable view of one active call.
type CallInfo struct {
	ID        string    `json:"id"`
	CallerAOR string    `json:"caller_aor"`
	CalleeAOR string    `json:"callee_aor"`
	StartedAt time.Time `json:"started_at"`
	RTPSent   uint64    `json:"rtp_sent"`
	RTPRecv   uint64    `json:"rtp_recv"`
}

// SessionSnapshot is a point-in-time, serializable view of a Session's
// full state — what the live-status API/WebSocket sends to the GUI.
type SessionSnapshot struct {
	TotalExtensions     int        `json:"total_extensions"`
	AvailableExtensions int        `json:"available_extensions"`
	ActiveCalls         []CallInfo `json:"active_calls"`
	TotalCallsStarted   uint64     `json:"total_calls_started"`
	TotalCallsAnswered  uint64     `json:"total_calls_answered"`
	TotalCallsFailed    uint64     `json:"total_calls_failed"`
	TotalRTPSent        uint64     `json:"total_rtp_sent"` // cumulative, includes ended calls
	TotalRTPRecv        uint64     `json:"total_rtp_recv"`
	RecentEvents        []Event    `json:"recent_events"` // last maxRecentEvents lifecycle events, oldest first
}

// Session is a persistent pool of registered, auto-answering extensions
// that calls can be added to incrementally over time — the model behind
// the dashboard's additive +25/+50/+100/+250/+500 buttons. Unlike Run (a
// one-shot batch that registers, calls, holds, hangs up, and returns), a
// Session stays alive indefinitely, reusing extensions as their calls end.
//
// A local Session (NewSession) has one pool acting as both caller and
// callee, dialing each other directly by extension number. A remote
// Session (NewRemoteSession) has two separate pools (one per PBXware
// server) and dials via a DID number rather than a plain extension — see
// docs/gui_spec.md's local/remote dialer sections.
type Session struct {
	ctx         context.Context
	dialTimeout time.Duration

	mu         sync.Mutex
	callerPool *pool
	calleePool *pool // == callerPool for a local Session
	// dialNumberFor maps a chosen callee AOR to the number the caller
	// should actually dial. nil means dial the AOR directly (local mode);
	// for a remote Session it looks up that extension's DID.
	dialNumberFor func(calleeAOR string) string
	calls         map[string]*activeCall
	events        []Event
	// stopCh is closed by Stop to cancel every call currently in flight
	// (queued/ramping or already answered) without tearing down the
	// session itself; Stop immediately replaces it with a fresh channel so
	// a later AddCalls works normally. AddCalls snapshots this once per
	// batch (under mu) and threads it through to runCall, rather than
	// having goroutines read s.stopCh directly, so a Stop that happens
	// mid-batch can't race a goroutine reading the field.
	stopCh chan struct{}

	nextEventSeq       atomic.Uint64
	nextCallID         atomic.Uint64
	totalCallsStarted  atomic.Uint64
	totalCallsAnswered atomic.Uint64
	totalCallsFailed   atomic.Uint64
	totalRTPSentEnded  atomic.Uint64 // sum from calls that have already ended
	totalRTPRecvEnded  atomic.Uint64
}

// NewSession registers every endpoint as its own phone against one
// PBXware server, sets each to auto-answer, and returns a Session ready
// for AddCalls, dialing between each other directly by extension number.
func NewSession(ctx context.Context, cfg SessionConfig, endpoints []sipua.Endpoint) (*Session, error) {
	p, err := newPool(ctx, cfg.LocalIP, cfg.BaseLocalPort, cfg.RegisterTimeout, cfg.DialDestination, cfg.SIPDomain, endpoints, sideProgress(cfg.OnRegisterProgress, ""))
	if err != nil {
		return nil, err
	}
	return &Session{
		ctx:         ctx,
		dialTimeout: cfg.DialTimeout,
		callerPool:  p,
		calleePool:  p,
		calls:       make(map[string]*activeCall),
		stopCh:      make(chan struct{}),
	}, nil
}

// RemoteSessionConfig configures a remote (cross-server) Session.
type RemoteSessionConfig struct {
	CallerDialDestination, CallerSIPDomain string
	CalleeDialDestination, CalleeSIPDomain string
	LocalIP                                string
	CallerBasePort                         int
	CalleeBasePort                         int
	RegisterTimeout                        time.Duration
	DialTimeout                            time.Duration
	OnRegisterProgress                     RegisterProgressFunc // see SessionConfig
}

// sideProgress adapts a RegisterProgressFunc to one pool's side, or
// returns nil if there's nothing to report to.
func sideProgress(f RegisterProgressFunc, side string) func(registered, failed, total int) {
	if f == nil {
		return nil
	}
	return func(registered, failed, total int) { f(side, registered, failed, total) }
}

// NewRemoteSession registers callerEndpoints against the caller server and
// calleeEndpoints against the callee server (two separate PBXware
// instances/tenants), and returns a Session that dials between them via
// didForExt — a lookup from a callee extension's AOR to the DID number
// that routes to it over the trunk (see docs/pbxware_api_reference.md's
// Trunks/DIDs sections: dialing the DID's exact number just works once the
// trunk+DID exist, no extra config needed).
func NewRemoteSession(ctx context.Context, cfg RemoteSessionConfig, callerEndpoints, calleeEndpoints []sipua.Endpoint, didForExt map[string]string) (*Session, error) {
	callerPool, err := newPool(ctx, cfg.LocalIP, cfg.CallerBasePort, cfg.RegisterTimeout, cfg.CallerDialDestination, cfg.CallerSIPDomain, callerEndpoints, sideProgress(cfg.OnRegisterProgress, "caller"))
	if err != nil {
		return nil, fmt.Errorf("setting up caller pool: %w", err)
	}
	calleePool, err := newPool(ctx, cfg.LocalIP, cfg.CalleeBasePort, cfg.RegisterTimeout, cfg.CalleeDialDestination, cfg.CalleeSIPDomain, calleeEndpoints, sideProgress(cfg.OnRegisterProgress, "callee"))
	if err != nil {
		return nil, fmt.Errorf("setting up callee pool: %w", err)
	}
	return &Session{
		ctx:         ctx,
		dialTimeout: cfg.DialTimeout,
		callerPool:  callerPool,
		calleePool:  calleePool,
		dialNumberFor: func(calleeAOR string) string {
			if did, ok := didForExt[calleeAOR]; ok {
				return did
			}
			return calleeAOR
		},
		calls:  make(map[string]*activeCall),
		stopCh: make(chan struct{}),
	}, nil
}

// AddCalls starts up to n new calls, drawing caller extensions from the
// caller pool and callee extensions from the callee pool (the same pool,
// for a local Session), staggered by rampInterval. If fewer than n pairs
// are available, it starts as many as it can and returns that count (not
// an error — a partial add is a normal, expected outcome the caller/GUI
// should just report). Each call automatically hangs up and returns its
// extensions to the pool after callDuration, or immediately if Stop is
// called first.
//
// onBatchDone, if non-nil, is called once every call in this batch has
// ended, with one CallRecord per call that actually got as far as
// dialing (a call canceled during its ramp-delay wait — before Dial was
// ever invoked — has nothing to record and is left out). This package
// has no knowledge of PBXware's CDR/MOS API or of log files — it's the
// caller's job (see cmd/gui/handlers.go) to turn these records into
// per-batch MOS/latency aggregates and a log file, then report a summary
// back via LogBatchQuality.
func (s *Session) AddCalls(n int, callDuration, rampInterval time.Duration, sendMedia bool, codec sipua.Codec, onBatchDone func([]CallRecord)) (started int) {
	pairs := s.reservePairs(n)
	if len(pairs) == 0 {
		return 0
	}

	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()

	var wg sync.WaitGroup
	var batchAnswered, batchFailed atomic.Uint64
	var recordsMu sync.Mutex
	records := make([]CallRecord, 0, len(pairs))
	wg.Add(len(pairs))
	for i, pair := range pairs {
		s.totalCallsStarted.Add(1)
		go func(i int, caller, callee string) {
			defer wg.Done()
			select {
			case <-time.After(time.Duration(i) * rampInterval):
			case <-s.ctx.Done():
				s.release(caller, callee)
				s.totalCallsFailed.Add(1)
				batchFailed.Add(1)
				return
			case <-stopCh:
				s.release(caller, callee)
				s.totalCallsFailed.Add(1)
				batchFailed.Add(1)
				return
			}
			rec := s.runCall(caller, callee, callDuration, sendMedia, stopCh, codec)
			recordsMu.Lock()
			records = append(records, rec)
			recordsMu.Unlock()
			if rec.Answered {
				batchAnswered.Add(1)
			} else {
				batchFailed.Add(1)
			}
		}(i, pair[0], pair[1])
	}

	go func() {
		wg.Wait()
		s.logEvent("batch_done", "", "", fmt.Sprintf(
			"+%d batch: %d/%d calls answered, %d failed",
			n, batchAnswered.Load(), len(pairs), batchFailed.Load(),
		))
		if onBatchDone != nil {
			onBatchDone(records)
		}
	}()

	return len(pairs)
}

// LogBatchQuality posts a follow-up event to the session's log/event
// stream — used by the caller (which owns the pbxware.Client needed for
// MOS lookups, not available to this package) to report per-batch
// MOS/latency aggregates shortly after AddCalls' own immediate
// "batch_done" summary, once it's finished computing them.
func (s *Session) LogBatchQuality(detail string) {
	s.logEvent("batch_quality", "", "", detail)
}

// reservePairs atomically picks up to n pairs of available extensions
// (one from the caller pool, one from the callee pool) and marks them
// unavailable, so concurrent AddCalls calls never double-book the same
// extension. For a local Session (same pool both sides), it's careful not
// to pick the same extension as both caller and callee.
func (s *Session) reservePairs(n int) [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	sameSided := s.callerPool == s.calleePool

	freeCallers := freeList(s.callerPool.available)
	var freeCallees []string
	if sameSided {
		freeCallees = freeCallers
	} else {
		freeCallees = freeList(s.calleePool.available)
	}

	maxPairs := n
	if sameSided {
		if m := len(freeCallers) / 2; m < maxPairs {
			maxPairs = m
		}
	} else {
		if len(freeCallers) < maxPairs {
			maxPairs = len(freeCallers)
		}
		if len(freeCallees) < maxPairs {
			maxPairs = len(freeCallees)
		}
	}

	pairs := make([][2]string, 0, maxPairs)
	for i := 0; i < maxPairs; i++ {
		var caller, callee string
		if sameSided {
			caller, callee = freeCallers[i*2], freeCallers[i*2+1]
		} else {
			caller, callee = freeCallers[i], freeCallees[i]
		}
		s.callerPool.available[caller] = false
		s.calleePool.available[callee] = false
		pairs = append(pairs, [2]string{caller, callee})
	}
	return pairs
}

func freeList(available map[string]bool) []string {
	var free []string
	for aor, ok := range available {
		if ok {
			free = append(free, aor)
		}
	}
	return free
}

// logEvent records one call-lifecycle event for the live-status log,
// keeping only the last maxRecentEvents.
func (s *Session) logEvent(typ, callerAOR, calleeAOR, detail string) {
	ev := Event{
		Seq: s.nextEventSeq.Add(1), Time: time.Now(), Type: typ,
		CallerAOR: callerAOR, CalleeAOR: calleeAOR, Detail: detail,
	}
	s.mu.Lock()
	s.events = append(s.events, ev)
	if len(s.events) > maxRecentEvents {
		s.events = s.events[len(s.events)-maxRecentEvents:]
	}
	s.mu.Unlock()
}

func (s *Session) release(caller, callee string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callerPool.available[caller] = true
	s.calleePool.available[callee] = true
}

// runCall places one call using codec and blocks until it ends (naturally,
// via ctx cancellation, or via stopCh being closed by Stop), returning a
// full record of what happened (for the caller's batch-completion tally
// and MOS/latency/log reporting — see AddCalls).
func (s *Session) runCall(callerAOR, calleeAOR string, callDuration time.Duration, sendMedia bool, stopCh <-chan struct{}, codec sipua.Codec) CallRecord {
	s.mu.Lock()
	caller := s.callerPool.phones[callerAOR]
	dialDestination, sipDomain := s.callerPool.dialDestination, s.callerPool.sipDomain
	dialNumber := calleeAOR
	if s.dialNumberFor != nil {
		dialNumber = s.dialNumberFor(calleeAOR)
	}
	s.mu.Unlock()

	startedAt := time.Now()
	s.logEvent("dialing", callerAOR, calleeAOR, "")
	dialStart := time.Now()
	// The callee answers with this call's codec if PBXware offers it (see
	// sipua.Phone.SetPreferredCodec); the pool reserved it for this call only.
	if callee := s.calleePool.phones[calleeAOR]; callee != nil {
		callee.SetPreferredCodec(codec)
	}
	call, err := caller.Dial(s.ctx, s.dialTimeout, dialDestination, sipDomain, dialNumber, sendMedia, codec)
	setupLatency := time.Since(dialStart) // INVITE-to-200-OK wall-clock time, win or lose — see CallRecord's doc comment
	if err != nil {
		log.Printf("session: %s -> %s (dialing %s): dial failed: %v", callerAOR, calleeAOR, dialNumber, err)
		s.logEvent("failed", callerAOR, calleeAOR, err.Error())
		s.totalCallsFailed.Add(1)
		s.release(callerAOR, calleeAOR)
		return CallRecord{
			CallerAOR: callerAOR, CalleeAOR: calleeAOR, Codec: codec,
			StartedAt: startedAt, EndedAt: time.Now(),
			Answered: false, FailReason: err.Error(), SetupLatency: setupLatency,
		}
	}
	s.totalCallsAnswered.Add(1)
	s.logEvent("answered", callerAOR, calleeAOR, "")

	id := fmt.Sprintf("%d", s.nextCallID.Add(1))
	ac := &activeCall{id: id, callerAOR: callerAOR, calleeAOR: calleeAOR, call: call, startedAt: startedAt, codec: codec, setupLatency: setupLatency}
	s.mu.Lock()
	s.calls[id] = ac
	s.mu.Unlock()

	select {
	case <-s.ctx.Done():
	case <-stopCh:
	case <-time.After(callDuration):
	}

	sent, recv := call.RTP.Stats()

	byeCtx, byeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := call.Hangup(byeCtx); err != nil {
		log.Printf("session: %s -> %s: hangup error: %v", callerAOR, calleeAOR, err)
	}
	byeCancel()

	s.totalRTPSentEnded.Add(sent)
	s.totalRTPRecvEnded.Add(recv)
	s.logEvent("ended", callerAOR, calleeAOR, fmt.Sprintf("rtp sent=%d recv=%d", sent, recv))

	s.mu.Lock()
	delete(s.calls, id)
	s.mu.Unlock()
	s.release(callerAOR, calleeAOR)
	return CallRecord{
		CallerAOR: callerAOR, CalleeAOR: calleeAOR, Codec: codec,
		StartedAt: startedAt, EndedAt: time.Now(),
		Answered: true, SetupLatency: setupLatency, RTPSent: sent, RTPRecv: recv,
	}
}

// Snapshot returns a point-in-time, serializable view of the session.
func (s *Session) Snapshot() SessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	totalExt := len(s.callerPool.phones)
	avail := countTrue(s.callerPool.available)
	if s.calleePool != s.callerPool {
		totalExt += len(s.calleePool.phones)
		avail += countTrue(s.calleePool.available)
	}

	var liveSent, liveRecv uint64
	calls := make([]CallInfo, 0, len(s.calls))
	for _, ac := range s.calls {
		sent, recv := ac.call.RTP.Stats()
		liveSent += sent
		liveRecv += recv
		calls = append(calls, CallInfo{
			ID:        ac.id,
			CallerAOR: ac.callerAOR,
			CalleeAOR: ac.calleeAOR,
			StartedAt: ac.startedAt,
			RTPSent:   sent,
			RTPRecv:   recv,
		})
	}

	events := make([]Event, len(s.events))
	copy(events, s.events)

	return SessionSnapshot{
		TotalExtensions:     totalExt,
		AvailableExtensions: avail,
		ActiveCalls:         calls,
		TotalCallsStarted:   s.totalCallsStarted.Load(),
		TotalCallsAnswered:  s.totalCallsAnswered.Load(),
		TotalCallsFailed:    s.totalCallsFailed.Load(),
		TotalRTPSent:        s.totalRTPSentEnded.Load() + liveSent,
		TotalRTPRecv:        s.totalRTPRecvEnded.Load() + liveRecv,
		RecentEvents:        events,
	}
}

func countTrue(m map[string]bool) int {
	n := 0
	for _, ok := range m {
		if ok {
			n++
		}
	}
	return n
}

// Stop cancels every call this session currently has in flight — both
// ones still waiting out their ramp delay (never dialed at all) and ones
// already answered (hung up immediately, via the exact same code path a
// naturally-expiring call takes — see runCall's select) — without
// tearing down the session itself: phones stay registered, and a later
// AddCalls works normally, using a freshly-made stop channel so it isn't
// immediately canceled too.
func (s *Session) Stop() {
	s.mu.Lock()
	oldStop := s.stopCh
	s.stopCh = make(chan struct{})
	s.mu.Unlock()
	close(oldStop)
}

// Close hangs up every active call and shuts down every phone.
func (s *Session) Close() {
	s.mu.Lock()
	calls := make([]*activeCall, 0, len(s.calls))
	for _, ac := range s.calls {
		calls = append(calls, ac)
	}
	phones := make([]*sipua.Phone, 0, len(s.callerPool.phones)+len(s.calleePool.phones))
	for _, p := range s.callerPool.phones {
		phones = append(phones, p)
	}
	if s.calleePool != s.callerPool {
		for _, p := range s.calleePool.phones {
			phones = append(phones, p)
		}
	}
	s.mu.Unlock()

	for _, ac := range calls {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ac.call.Hangup(ctx)
		cancel()
	}
	for _, p := range phones {
		p.Close()
	}
}
