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
}

// pool is one registered, auto-answering set of extensions on one PBXware
// server. A local Session has one pool shared as both caller and callee;
// a remote Session has two separate pools, one per server.
type pool struct {
	phones          map[string]*sipua.Phone // keyed by AOR
	available       map[string]bool         // AOR -> currently free for a new call
	dialDestination string
	sipDomain       string
}

func newPool(ctx context.Context, localIP string, basePort int, registerTimeout time.Duration, dialDestination, sipDomain string, endpoints []sipua.Endpoint) (*pool, error) {
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

	if err := registerPhonesStaggered(ctx, registerTimeout, dialDestination, sipDomain, phones, order); err != nil {
		return nil, err
	}
	log.Printf("session: %d extensions registered against %s", len(phones), dialDestination)

	available := make(map[string]bool, len(phones))
	for aor, phone := range phones {
		phone.AutoAnswer(ctx)
		available[aor] = true
	}

	return &pool{phones: phones, available: available, dialDestination: dialDestination, sipDomain: sipDomain}, nil
}

// registerPhonesStaggered registers every phone with a small stagger
// between each (see orchestrator.go's registerAll for why — PBXware
// rejects a chunk of otherwise-valid REGISTERs when hit fully
// concurrently).
func registerPhonesStaggered(ctx context.Context, timeout time.Duration, dialDestination, sipDomain string, phones map[string]*sipua.Phone, order []string) error {
	const registerStagger = 20 * time.Millisecond

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
			}
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

// activeCall is one ongoing call tracked internally by a Session.
type activeCall struct {
	id                   string
	callerAOR, calleeAOR string
	call                 *sipua.Call
	startedAt            time.Time
}

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
	p, err := newPool(ctx, cfg.LocalIP, cfg.BaseLocalPort, cfg.RegisterTimeout, cfg.DialDestination, cfg.SIPDomain, endpoints)
	if err != nil {
		return nil, err
	}
	return &Session{
		ctx:         ctx,
		dialTimeout: cfg.DialTimeout,
		callerPool:  p,
		calleePool:  p,
		calls:       make(map[string]*activeCall),
	}, nil
}

// RemoteSessionConfig configures a remote (cross-server) Session.
type RemoteSessionConfig struct {
	CallerDialDestination, CallerSIPDomain string
	CalleeDialDestination, CalleeSIPDomain string
	LocalIP         string
	CallerBasePort  int
	CalleeBasePort  int
	RegisterTimeout time.Duration
	DialTimeout     time.Duration
}

// NewRemoteSession registers callerEndpoints against the caller server and
// calleeEndpoints against the callee server (two separate PBXware
// instances/tenants), and returns a Session that dials between them via
// didForExt — a lookup from a callee extension's AOR to the DID number
// that routes to it over the trunk (see docs/pbxware_api_reference.md's
// Trunks/DIDs sections: dialing the DID's exact number just works once the
// trunk+DID exist, no extra config needed).
func NewRemoteSession(ctx context.Context, cfg RemoteSessionConfig, callerEndpoints, calleeEndpoints []sipua.Endpoint, didForExt map[string]string) (*Session, error) {
	callerPool, err := newPool(ctx, cfg.LocalIP, cfg.CallerBasePort, cfg.RegisterTimeout, cfg.CallerDialDestination, cfg.CallerSIPDomain, callerEndpoints)
	if err != nil {
		return nil, fmt.Errorf("setting up caller pool: %w", err)
	}
	calleePool, err := newPool(ctx, cfg.LocalIP, cfg.CalleeBasePort, cfg.RegisterTimeout, cfg.CalleeDialDestination, cfg.CalleeSIPDomain, calleeEndpoints)
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
		calls: make(map[string]*activeCall),
	}, nil
}

// AddCalls starts up to n new calls, drawing caller extensions from the
// caller pool and callee extensions from the callee pool (the same pool,
// for a local Session), staggered by rampInterval. If fewer than n pairs
// are available, it starts as many as it can and returns that count (not
// an error — a partial add is a normal, expected outcome the caller/GUI
// should just report). Each call automatically hangs up and returns its
// extensions to the pool after callDuration.
func (s *Session) AddCalls(n int, callDuration, rampInterval time.Duration, sendMedia bool) (started int) {
	pairs := s.reservePairs(n)
	for i, pair := range pairs {
		s.totalCallsStarted.Add(1)
		go func(i int, caller, callee string) {
			select {
			case <-time.After(time.Duration(i) * rampInterval):
			case <-s.ctx.Done():
				s.release(caller, callee)
				s.totalCallsFailed.Add(1)
				return
			}
			s.runCall(caller, callee, callDuration, sendMedia)
		}(i, pair[0], pair[1])
	}
	return len(pairs)
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

func (s *Session) release(caller, callee string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callerPool.available[caller] = true
	s.calleePool.available[callee] = true
}

func (s *Session) runCall(callerAOR, calleeAOR string, callDuration time.Duration, sendMedia bool) {
	s.mu.Lock()
	caller := s.callerPool.phones[callerAOR]
	dialDestination, sipDomain := s.callerPool.dialDestination, s.callerPool.sipDomain
	dialNumber := calleeAOR
	if s.dialNumberFor != nil {
		dialNumber = s.dialNumberFor(calleeAOR)
	}
	s.mu.Unlock()

	call, err := caller.Dial(s.ctx, s.dialTimeout, dialDestination, sipDomain, dialNumber, sendMedia)
	if err != nil {
		log.Printf("session: %s -> %s (dialing %s): dial failed: %v", callerAOR, calleeAOR, dialNumber, err)
		s.totalCallsFailed.Add(1)
		s.release(callerAOR, calleeAOR)
		return
	}
	s.totalCallsAnswered.Add(1)

	id := fmt.Sprintf("%d", s.nextCallID.Add(1))
	ac := &activeCall{id: id, callerAOR: callerAOR, calleeAOR: calleeAOR, call: call, startedAt: time.Now()}
	s.mu.Lock()
	s.calls[id] = ac
	s.mu.Unlock()

	select {
	case <-s.ctx.Done():
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

	s.mu.Lock()
	delete(s.calls, id)
	s.mu.Unlock()
	s.release(callerAOR, calleeAOR)
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

	return SessionSnapshot{
		TotalExtensions:     totalExt,
		AvailableExtensions: avail,
		ActiveCalls:         calls,
		TotalCallsStarted:   s.totalCallsStarted.Load(),
		TotalCallsAnswered:  s.totalCallsAnswered.Load(),
		TotalCallsFailed:    s.totalCallsFailed.Load(),
		TotalRTPSent:        s.totalRTPSentEnded.Load() + liveSent,
		TotalRTPRecv:        s.totalRTPRecvEnded.Load() + liveRecv,
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
