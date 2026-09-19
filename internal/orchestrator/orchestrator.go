// Package orchestrator drives a load-test run against one PBXware server:
// registers a pool of extensions, pairs them up, places calls between the
// pairs (staggered, to ramp concurrency rather than slam PBXware all at
// once), holds them, and hangs up.
//
// Scoped to a single server on purpose — see PROJECT_STATE.md's "Future
// scope: remote (inter-server) calling" note. Running two servers is meant
// to be "call Run twice, once per server," not a restructuring of this
// package.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"swarmdialer/internal/sipua"
)

// Config controls one run.
type Config struct {
	DialDestination string // PBXware host:port, e.g. "10.1.100.208:5060"
	SIPDomain       string // domain for SIP headers; defaults to DialDestination if empty
	LocalIP         string // our own IP, advertised in Contact headers
	BaseLocalPort   int    // first local SIP port to use; phone i binds BaseLocalPort+i

	RampInterval    time.Duration // delay between starting each successive pair's call
	CallDuration    time.Duration // how long to hold each call before hanging up
	RegisterTimeout time.Duration
	DialTimeout     time.Duration
}

// PairResult is the outcome of one caller/callee pair's call attempt.
type PairResult struct {
	CallerAOR, CalleeAOR string
	Answered             bool
	Err                  error
	RTPSent, RTPRecv     uint64
}

// Result aggregates one full run.
type Result struct {
	Pairs []PairResult
}

// Answered returns how many pairs completed a call successfully.
func (r *Result) Answered() int {
	n := 0
	for _, p := range r.Pairs {
		if p.Answered {
			n++
		}
	}
	return n
}

// Failed returns how many pairs failed to establish a call.
func (r *Result) Failed() int {
	return len(r.Pairs) - r.Answered()
}

// Run registers every endpoint as its own simulated extension, pairs them up
// (0↔1, 2↔3, ...; an odd one out is skipped), places a call within each
// pair, holds it for Config.CallDuration, then hangs up — all concurrently,
// with each pair's call start staggered by RampInterval.
//
// state receives live updates throughout the run (phase transitions, each
// pair's status and running RTP counts) — pass the result of NewRunState
// and read it concurrently (e.g. from an HTTP handler) to watch the run
// progress instead of only seeing the final Result once everything
// finishes. Pass nil if you only care about the final Result.
func Run(ctx context.Context, cfg Config, endpoints []sipua.Endpoint, state *RunState) (*Result, error) {
	if state == nil {
		state = NewRunState(len(endpoints) / 2)
	}

	sipDomain := cfg.SIPDomain
	if sipDomain == "" {
		sipDomain = cfg.DialDestination
	}

	phones := make([]*sipua.Phone, len(endpoints))
	for i, ep := range endpoints {
		phone, err := sipua.NewPhone(ctx, cfg.LocalIP, cfg.BaseLocalPort+i, ep)
		if err != nil {
			return nil, fmt.Errorf("setting up phone for %s: %w", ep.AOR, err)
		}
		phones[i] = phone
	}
	defer func() {
		for _, phone := range phones {
			phone.Close()
		}
	}()

	state.setPhase(PhaseRegistering)
	if err := registerAll(ctx, cfg, sipDomain, phones); err != nil {
		return nil, err
	}
	log.Printf("orchestrator: %d extensions registered", len(phones))

	for _, phone := range phones {
		phone.AutoAnswer(ctx)
	}

	pairs := makePairs(len(endpoints))
	log.Printf("orchestrator: %d pairs to call, ramping %s apart", len(pairs), cfg.RampInterval)

	for i, pair := range pairs {
		state.initPair(i, endpoints[pair[0]].AOR, endpoints[pair[1]].AOR)
	}
	state.setPhase(PhaseCalling)

	results := make([]PairResult, len(pairs))
	var wg sync.WaitGroup
	for i, pair := range pairs {
		wg.Add(1)
		go func(i int, pair [2]int) {
			defer wg.Done()
			select {
			case <-time.After(time.Duration(i) * cfg.RampInterval):
			case <-ctx.Done():
				err := ctx.Err()
				state.updatePair(i, func(p *PairState) {
					p.Status = StatusFailed
					p.Err = err.Error()
				})
				results[i] = PairResult{
					CallerAOR: endpoints[pair[0]].AOR,
					CalleeAOR: endpoints[pair[1]].AOR,
					Err:       err,
				}
				return
			}
			state.updatePair(i, func(p *PairState) { p.Status = StatusDialing })
			results[i] = runOnePair(ctx, cfg, sipDomain, phones[pair[0]], endpoints[pair[0]].AOR, endpoints[pair[1]].AOR, i, state)
		}(i, pair)
	}
	wg.Wait()

	state.setPhase(PhaseDone)
	return &Result{Pairs: results}, nil
}

func registerAll(ctx context.Context, cfg Config, sipDomain string, phones []*sipua.Phone) error {
	// Registering fully concurrently (all at once) has been observed to make
	// PBXware reject a chunk of otherwise-valid REGISTERs with 403 — the
	// same extensions register fine individually or staggered. Root cause
	// not pinned down yet; staggering slightly avoids it cheaply, and
	// registration isn't part of what needs to be simultaneous (only the
	// calls do), so this costs nothing for the actual load test.
	const registerStagger = 20 * time.Millisecond

	var wg sync.WaitGroup
	errs := make([]error, len(phones))
	for i, phone := range phones {
		wg.Add(1)
		go func(i int, phone *sipua.Phone) {
			defer wg.Done()
			select {
			case <-time.After(time.Duration(i) * registerStagger):
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			regCtx, cancel := context.WithTimeout(ctx, cfg.RegisterTimeout)
			defer cancel()
			if _, err := phone.Register(regCtx, cfg.DialDestination, sipDomain, 300); err != nil {
				errs[i] = fmt.Errorf("registering %s: %w", phone.Endpoint.AOR, err)
			}
		}(i, phone)
	}
	wg.Wait()

	var failed []error
	for _, err := range errs {
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d/%d registrations failed, first error: %w", len(failed), len(phones), failed[0])
	}
	return nil
}

func makePairs(n int) [][2]int {
	var pairs [][2]int
	for i := 0; i+1 < n; i += 2 {
		pairs = append(pairs, [2]int{i, i + 1})
	}
	return pairs
}

func runOnePair(ctx context.Context, cfg Config, sipDomain string, caller *sipua.Phone, callerAOR, calleeAOR string, i int, state *RunState) PairResult {
	call, err := caller.Dial(ctx, cfg.DialTimeout, cfg.DialDestination, sipDomain, calleeAOR, true)
	if err != nil {
		state.updatePair(i, func(p *PairState) {
			p.Status = StatusFailed
			p.Err = err.Error()
		})
		return PairResult{CallerAOR: callerAOR, CalleeAOR: calleeAOR, Err: fmt.Errorf("dialing: %w", err)}
	}
	state.updatePair(i, func(p *PairState) { p.Status = StatusAnswered })

	// Update live RTP counts once a second while the call is held, so a
	// status page shows media actually flowing rather than just a static
	// "answered" state until the very end.
	holdTicker := time.NewTicker(time.Second)
	defer holdTicker.Stop()
	deadline := time.After(cfg.CallDuration)
holdLoop:
	for {
		select {
		case <-ctx.Done():
			break holdLoop
		case <-deadline:
			break holdLoop
		case <-holdTicker.C:
			sent, recv := call.RTP.Stats()
			state.updatePair(i, func(p *PairState) { p.RTPSent = sent; p.RTPRecv = recv })
		}
	}

	sent, recv := call.RTP.Stats()

	// Hang up even if ctx was already canceled — cleanup shouldn't be
	// skipped just because the run is winding down.
	byeCtx, byeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer byeCancel()
	if err := call.Hangup(byeCtx); err != nil {
		log.Printf("orchestrator: %s->%s: hangup error: %v", callerAOR, calleeAOR, err)
	}

	state.updatePair(i, func(p *PairState) {
		p.Status = StatusEnded
		p.RTPSent = sent
		p.RTPRecv = recv
	})

	return PairResult{CallerAOR: callerAOR, CalleeAOR: calleeAOR, Answered: true, RTPSent: sent, RTPRecv: recv}
}
