package orchestrator

import (
	"sync"
	"time"
)

// PairStatus is the lifecycle state of one caller/callee pair's call attempt.
type PairStatus string

const (
	StatusPending  PairStatus = "pending"
	StatusDialing  PairStatus = "dialing"
	StatusAnswered PairStatus = "answered"
	StatusFailed   PairStatus = "failed"
	StatusEnded    PairStatus = "ended"
)

// Phase is the overall run's current stage.
type Phase string

const (
	PhaseSettingUp   Phase = "setting_up"
	PhaseRegistering Phase = "registering"
	PhaseCalling     Phase = "calling"
	PhaseDone        Phase = "done"
)

// PairState is one pair's live status — safe to copy and serialize.
type PairState struct {
	CallerAOR, CalleeAOR string
	Status               PairStatus
	Err                  string
	RTPSent, RTPRecv     uint64
}

// Snapshot is a point-in-time, serializable copy of a run's full state.
type Snapshot struct {
	Phase     Phase
	StartedAt time.Time
	Pairs     []PairState
}

// RunState is one run's live status, safe for concurrent reads (via
// Snapshot) while Run is still updating it in the background. Create with
// NewRunState and pass it to Run; read with Snapshot at any time — including
// from an HTTP handler polled while the run is still in progress, which is
// the whole point: a live-status page reads this while Run() is still
// executing in a goroutine elsewhere.
type RunState struct {
	mu        sync.RWMutex
	phase     Phase
	startedAt time.Time
	pairs     []PairState
}

// NewRunState creates live state for a run with n pairs, ready to pass to
// Run.
func NewRunState(n int) *RunState {
	return &RunState{
		phase:     PhaseSettingUp,
		startedAt: time.Now(),
		pairs:     make([]PairState, n),
	}
}

func (s *RunState) setPhase(p Phase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = p
}

func (s *RunState) initPair(i int, callerAOR, calleeAOR string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairs[i] = PairState{CallerAOR: callerAOR, CalleeAOR: calleeAOR, Status: StatusPending}
}

func (s *RunState) updatePair(i int, mutate func(*PairState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.pairs[i])
}

// Snapshot returns a point-in-time copy of the run's state, safe to read or
// serialize without further locking.
func (s *RunState) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pairsCopy := make([]PairState, len(s.pairs))
	copy(pairsCopy, s.pairs)
	return Snapshot{Phase: s.phase, StartedAt: s.startedAt, Pairs: pairsCopy}
}
