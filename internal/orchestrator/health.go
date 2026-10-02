package orchestrator

import (
	"errors"
	"fmt"
	"maps"
	"sync"

	"swarmdialer/internal/sipua"
)

// Health is a Session's running totals of what went wrong (or right) with
// its calls and phones, since it was created. A test takes it at its start
// and end; the difference is the test's own.
type Health struct {
	// Failures counts failed calls by cause: a sipua.Fail* constant, with
	// the SIP code for rejections (e.g. "rejected_503").
	Failures map[string]uint64
	// Dropped counts answered calls that PBXware hung up before their
	// planned end.
	Dropped uint64
	// NoAudio counts answered calls (with media) that received no RTP.
	NoAudio uint64
	// Media received by the calling phones (audio from the calling
	// PBXware) and by the answering phones (audio from the answering one).
	CallerMedia, CalleeMedia MediaTotals
	// Registration of the session's extensions.
	Extensions      int
	NotRegistered   int    // extensions whose last registration attempt failed
	RefreshFailures uint64 // failed re-registrations
}

// MediaTotals sums the receive side of many calls' RTP streams.
type MediaTotals struct {
	Calls       uint64
	Expected    uint64
	Received    uint64
	JitterSumMS float64
	JitterMaxMS float64
}

func (m *MediaTotals) add(s sipua.MediaStats) {
	if s.Expected == 0 {
		return
	}
	m.Calls++
	m.Expected += s.Expected
	m.Received += min(s.Received, s.Expected)
	m.JitterSumMS += s.JitterMS
	m.JitterMaxMS = max(m.JitterMaxMS, s.JitterMS)
}

// failureKey names a failure cause, with the SIP code for rejections.
func failureKey(err error) (cause string, code int, key string) {
	var de *sipua.DialError
	if errors.As(err, &de) {
		cause, code = de.Cause, de.SIPCode
	} else {
		cause = sipua.FailSetupError
	}
	key = cause
	if cause == sipua.FailRejected {
		key = fmt.Sprintf("%s_%d", cause, code)
	}
	return
}

// health holds a Session's Health under its own lock (calls update it
// from many goroutines).
type health struct {
	mu sync.Mutex
	h  Health
}

func (h *health) update(f func(*Health)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.h.Failures == nil {
		h.h.Failures = map[string]uint64{}
	}
	f(&h.h)
}

// Health returns a copy of the session's running totals.
func (s *Session) Health() Health {
	s.health.mu.Lock()
	out := s.health.h
	out.Failures = maps.Clone(s.health.h.Failures)
	s.health.mu.Unlock()
	if out.Failures == nil {
		out.Failures = map[string]uint64{}
	}
	s.mu.Lock()
	pools := []*pool{s.callerPool}
	if s.calleePool != s.callerPool {
		pools = append(pools, s.calleePool)
	}
	s.mu.Unlock()
	for _, p := range pools {
		total, notReg, refreshFailed := p.registration()
		out.Extensions += total
		out.NotRegistered += notReg
		out.RefreshFailures += refreshFailed
	}
	return out
}

// ResetMediaPeaks starts a new window for the media jitter maximums (the
// other totals are differences between two Health readings).
func (s *Session) ResetMediaPeaks() {
	s.health.update(func(h *Health) { h.CallerMedia.JitterMaxMS, h.CalleeMedia.JitterMaxMS = 0, 0 })
}

// watchAnsweredMedia sends the media statistics of every call the pool's
// phones answer into the session's totals.
func (s *Session) watchAnsweredMedia(p *pool) {
	for _, ph := range p.phones {
		ph.SetOnAnsweredCallEnd(func(ms sipua.MediaStats) {
			s.health.update(func(h *Health) { h.CalleeMedia.add(ms) })
		})
	}
}
