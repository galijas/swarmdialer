package testrun

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/sipua"
)

// Env is what the runner needs from the rest of SwarmDialer.
type Env interface {
	// RemoteSession returns the MT -> CC dialing session (registering its
	// extensions first if needed).
	RemoteSession() (*orchestrator.Session, error)
	// SetRecording sets recording on both instances.
	SetRecording(mode, format string) error
	// PrepareTrunk orders both trunks for codec (see cmd/gui's
	// prepareRemoteBatch) and returns a release to call when the test's
	// calls have ended.
	PrepareTrunk(codec string) (release func(), err error)
	// RecordingCDRs returns recording CDRs of calls started since since, per
	// instance role ("MT", "CC").
	RecordingCDRs(since time.Time) (map[string][]pbxware.RecordingCDR, error)
	// CallMOS looks up PBXware's MOS for answered calls (by CDR).
	CallMOS(records []orchestrator.CallRecord) (MOS, error)
}

// MOS is call quality from PBXware's RTCP data (1.0-5.0, 4.40 is the most
// PBXware reports), over N calls.
type MOS struct {
	N   int     `json:"n"`
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
}

// mosSampleSize bounds how many calls' MOS is looked up per test: each is
// one PBXware API call, and a rolling test places thousands of calls.
const mosSampleSize = 200

// VPSLimits are a PBXware VPS's SERVERware resource limits.
type VPSLimits struct {
	CPULimit     int `json:"cpu_limit"` // cores; 0 = unlimited
	MemLimitMB   int `json:"mem_limit_mb"`
	CallrecRAMMB int `json:"callrec_ram_mb"`
}

// Sample is one 5-second sample during a test.
type Sample struct {
	T     time.Time             `json:"t"`
	Phase string                `json:"phase"` // baseline, ramp, hold, rolling, stopping, cooldown
	Host  HostMetrics           `json:"host"`
	VPS   map[string]VPSMetrics `json:"vps"`
	Local LocalMetrics          `json:"swarmdialer"`

	ConcurrentCalls int     `json:"concurrent_calls"`
	Started         uint64  `json:"started"`
	Answered        uint64  `json:"answered"`
	Failed          uint64  `json:"failed"`
	SetupP95MS      int64   `json:"setup_p95_ms"` // calls answered in the last 30s
	RAMDiskEstMB    float64 `json:"ramdisk_est_mb,omitempty"`
	CPS             float64 `json:"cps,omitempty"` // rolling: current dial rate
}

// Stats is a small summary of a set of values.
type Stats struct {
	N   int     `json:"n"`
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

func statsOf(v []float64) Stats {
	if len(v) == 0 {
		return Stats{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	return Stats{N: len(s), Avg: round2(sum / float64(len(s))), P95: round2(s[int(math.Ceil(0.95*float64(len(s))))-1]), Max: round2(s[len(s)-1])}
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// AtLoad is the averaged load while at the test's peak (the hold, or the
// last minute before a stop).
type AtLoad struct {
	Calls          int                `json:"calls"`
	HostCPUPct     float64            `json:"host_cpu_pct"`
	HostMemPct     float64            `json:"host_mem_pct"`
	VPSCPUPct      map[string]float64 `json:"vps_cpu_pct"`
	AsteriskCPUPct map[string]float64 `json:"asterisk_cpu_pct"`
	SwarmDialerCPU float64            `json:"swarmdialer_cpu_pct"`
}

// RecordingResult covers the recording-specific measurements.
type RecordingResult struct {
	RAMDiskFullEstAtCalls *int       `json:"ramdisk_full_estimated_at_calls,omitempty"`
	RAMDiskFullEstAt      *time.Time `json:"ramdisk_full_estimated_at,omitempty"`
	// MP3ConversionDelayS is the time from a call ending to its recording's
	// MP3 being available, per instance role.
	MP3ConversionDelayS map[string]Stats  `json:"mp3_conversion_delay_s"`
	MP3DelayTrend       map[string]string `json:"mp3_delay_trend"` // stable | growing
	MissingRecordings   map[string]int    `json:"missing_recordings"`
}

// Result is one test's outcome.
type Result struct {
	Test       Test      `json:"test"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	StopReason string    `json:"stop_reason"`
	StopDetail string    `json:"stop_detail,omitempty"`

	MaxConcurrent int `json:"max_concurrent_calls"`
	Calls         struct {
		Started  int `json:"started"`
		Answered int `json:"answered"`
		Failed   int `json:"failed"`
	} `json:"calls"`
	SetupMS                Stats            `json:"setup_ms"`
	MOS                    MOS              `json:"mos"`
	RTPReceivedRatio       float64          `json:"rtp_received_ratio"`
	QualityDegradedAtCalls *int             `json:"quality_degraded_at_calls,omitempty"`
	QualityDegradedReason  string           `json:"quality_degraded_reason,omitempty"`
	AtLoad                 AtLoad           `json:"at_load"`
	Recording              *RecordingResult `json:"recording,omitempty"`
	Samples                []Sample         `json:"samples"`
	Error                  string           `json:"error,omitempty"`
}

// Stop reasons.
const (
	StopTargetReached     = "target_reached"
	StopTargetNotReached  = "target_not_reached" // every call placed, but fewer ran at once
	StopHostCPU           = "host_cpu_100"
	StopHostRAM           = "host_ram_100"
	StopVPSCPU            = "vps_cpu_limit"
	StopVPSRAM            = "vps_ram_limit"
	StopSwarmDialerLoaded = "swarmdialer_overloaded"
	StopCancelled         = "cancelled"
	StopError             = "error"
)

// Thresholds.
const (
	sampleInterval    = 5 * time.Second
	saturatedPct      = 95.0 // "100%" allowing for sampling granularity
	saturatedSamples  = 3    // sustained this many samples (15s) before stopping
	swarmDialerMaxPct = 80.0
	degradeFailRatio  = 0.01
	degradeSetupP95MS = 500
)

// Status is the runner's live state, for the GUI.
type Status struct {
	Running   bool      `json:"running"`
	Profile   string    `json:"profile"`
	StartedAt time.Time `json:"started_at"`
	TestIndex int       `json:"test_index"`
	TestCount int       `json:"test_count"`
	TestID    string    `json:"test_id"`
	Phase     string    `json:"phase"`
	Message   string    `json:"message"`
	Latest    *Sample   `json:"latest,omitempty"`
	// CurrentSamples are the running test's samples so far (for live
	// charts); empty between tests.
	CurrentSamples []Sample `json:"current_samples,omitempty"`
	Results        []Result `json:"results"`
	Done           bool     `json:"done"`
	Error          string   `json:"error,omitempty"`
	Log            []string `json:"log"`
}

// Runner executes one profile run.
type Runner struct {
	env     Env
	mon     *Monitor
	limits  map[string]VPSLimits
	profile Profile

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	status  Status
	current *Result // the running test's result, guarded by mu
}

// NewRunner prepares a run; call Run to start it.
func NewRunner(env Env, mon *Monitor, limits map[string]VPSLimits, profile Profile) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{env: env, mon: mon, limits: limits, profile: profile, ctx: ctx, cancel: cancel,
		status: Status{Profile: fmt.Sprintf("%s v%d", profile.Name, profile.Version), TestCount: len(profile.Tests)}}
}

// Status returns a copy of the live state (without the per-test samples,
// which can be large; use Results for those).
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.Results = make([]Result, len(r.status.Results))
	for i, res := range r.status.Results {
		res.Samples = nil
		st.Results[i] = res
	}
	st.Log = append([]string(nil), r.status.Log...)
	if r.current != nil {
		st.CurrentSamples = append([]Sample(nil), r.current.Samples...)
	}
	return st
}

// Results returns the full results, including samples.
func (r *Runner) Results() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Result(nil), r.status.Results...)
}

// Cancel stops the run: the current test's calls are hung up and no
// further tests run.
func (r *Runner) Cancel() { r.cancel() }

func (r *Runner) logf(format string, a ...any) {
	line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, a...)
	r.mu.Lock()
	r.status.Log = append(r.status.Log, line)
	if len(r.status.Log) > 500 {
		r.status.Log = r.status.Log[len(r.status.Log)-500:]
	}
	r.status.Message = fmt.Sprintf(format, a...)
	r.mu.Unlock()
}

func (r *Runner) setPhase(phase string) {
	r.mu.Lock()
	r.status.Phase = phase
	r.mu.Unlock()
}

// Run executes every test in order. It blocks until done or cancelled.
func (r *Runner) Run() {
	r.mu.Lock()
	r.status.Running, r.status.StartedAt = true, time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.status.Running, r.status.Done = false, true
		r.status.Phase = ""
		r.mu.Unlock()
		if err := r.env.SetRecording(RecordingOff, "wav49"); err != nil {
			r.logf("turning recording off after the run: %v", err)
		}
	}()

	r.logf("monitoring host node %s", r.mon.Node())
	sess, err := r.env.RemoteSession()
	if err != nil {
		r.fail(fmt.Errorf("preparing the MT to CC session: %w", err))
		return
	}
	for i, t := range r.profile.Tests {
		if r.ctx.Err() != nil {
			break
		}
		r.mu.Lock()
		r.status.TestIndex, r.status.TestID = i+1, t.ID
		r.mu.Unlock()
		res := r.runTest(sess, t)
		r.mu.Lock()
		r.status.Results = append(r.status.Results, res)
		r.current = nil
		r.mu.Unlock()
		r.logf("%s finished: %s, max %d calls at once", t.ID, res.StopReason, res.MaxConcurrent)
	}
	if r.ctx.Err() != nil {
		r.logf("run cancelled")
	} else {
		r.logf("all tests finished")
	}
}

func (r *Runner) fail(err error) {
	r.logf("error: %v", err)
	r.mu.Lock()
	r.status.Error = err.Error()
	r.mu.Unlock()
}

// sleepCtx waits d or until the run is cancelled; false if cancelled.
func (r *Runner) sleepCtx(d time.Duration) bool {
	select {
	case <-r.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// testState is the per-test bookkeeping for sampling.
type testState struct {
	res          *Result
	t            Test
	sess         *orchestrator.Session
	ramdiskMB    float64
	lastStarted  time.Time
	saturated    map[string]int // stop reason -> consecutive samples over threshold
	lastFailed   uint64
	lastAnswered uint64
	cps          float64
}

// sample takes one sample, stores it, and returns a stop reason if a
// saturation condition is met ("" otherwise).
func (r *Runner) sample(st *testState, phase string) string {
	s := Sample{T: time.Now(), Phase: phase, Local: r.mon.SampleLocal(), CPS: st.cps}
	var err error
	if s.Host, err = r.mon.SampleHost(); err != nil {
		r.logf("sampling host metrics: %v", err)
	}
	if s.VPS, err = r.mon.SampleVPS(); err != nil {
		r.logf("sampling VPS metrics: %v", err)
	}
	snap := st.sess.Snapshot()
	s.ConcurrentCalls = len(snap.ActiveCalls)
	s.Started, s.Answered, s.Failed = snap.TotalCallsStarted, snap.TotalCallsAnswered, snap.TotalCallsFailed
	var recent []float64
	rate := recordingBytesPerSecond(st.t.Recording, st.t.RecordingFormat)
	var ramdiskBytes float64
	for _, c := range snap.ActiveCalls {
		if time.Since(c.StartedAt) < 30*time.Second {
			recent = append(recent, float64(c.SetupMS))
		}
		ramdiskBytes += rate * time.Since(c.StartedAt).Seconds()
	}
	s.SetupP95MS = int64(statsOf(recent).P95)
	if rate > 0 {
		s.RAMDiskEstMB = round2(ramdiskBytes / (1024 * 1024))
	}

	res := st.res
	r.mu.Lock()
	res.Samples = append(res.Samples, s)
	if s.ConcurrentCalls > res.MaxConcurrent {
		res.MaxConcurrent = s.ConcurrentCalls
	}
	r.status.Latest = &s
	r.mu.Unlock()

	if phase == "baseline" || phase == "cooldown" || phase == "stopping" {
		return ""
	}

	// Recording RAM disk: estimated only (it isn't visible in any API);
	// noted, not a reason to stop.
	if rate > 0 && st.ramdiskMB > 0 && res.Recording.RAMDiskFullEstAtCalls == nil && s.RAMDiskEstMB >= 0.95*st.ramdiskMB {
		n, at := s.ConcurrentCalls, s.T
		res.Recording.RAMDiskFullEstAtCalls, res.Recording.RAMDiskFullEstAt = &n, &at
		r.logf("recording RAM disk estimated full (%.0f of %.0f MB) at %d calls; recordings are cut off from here", s.RAMDiskEstMB, st.ramdiskMB, n)
	}

	// Quality: first point where calls start failing or setup slows down.
	newFailed, newAnswered := s.Failed-st.lastFailed, s.Answered-st.lastAnswered
	st.lastFailed, st.lastAnswered = s.Failed, s.Answered
	if res.QualityDegradedAtCalls == nil {
		reason := ""
		if total := newFailed + newAnswered; total > 0 && float64(newFailed)/float64(total) > degradeFailRatio {
			reason = fmt.Sprintf("%d of %d new calls failed", newFailed, total)
		} else if s.SetupP95MS > degradeSetupP95MS {
			reason = fmt.Sprintf("p95 call setup %d ms", s.SetupP95MS)
		}
		if reason != "" {
			n := s.ConcurrentCalls
			res.QualityDegradedAtCalls, res.QualityDegradedReason = &n, reason
			r.logf("call quality degraded at %d calls: %s", n, reason)
		}
	}

	check := func(reason string, over bool, detail string) string {
		if over {
			st.saturated[reason]++
		} else {
			st.saturated[reason] = 0
		}
		if st.saturated[reason] >= saturatedSamples {
			res.StopDetail = detail
			return reason
		}
		return ""
	}
	if why := check(StopHostCPU, s.Host.CPUPct >= saturatedPct, fmt.Sprintf("host CPU %.1f%%", s.Host.CPUPct)); why != "" {
		return why
	}
	if why := check(StopHostRAM, s.Host.MemPct >= saturatedPct, fmt.Sprintf("host memory %.1f%%", s.Host.MemPct)); why != "" {
		return why
	}
	for role, v := range s.VPS {
		lim := r.limits[role]
		if lim.CPULimit > 0 {
			if why := check(StopVPSCPU+":"+role, v.CPUPct >= saturatedPct*float64(lim.CPULimit), fmt.Sprintf("%s VPS CPU %.0f%% of its %d-core limit", role, v.CPUPct, lim.CPULimit)); why != "" {
				return StopVPSCPU
			}
		}
		if lim.MemLimitMB > 0 {
			memPct := 100 * v.MemBytes / (float64(lim.MemLimitMB) * 1024 * 1024)
			if why := check(StopVPSRAM+":"+role, memPct >= saturatedPct, fmt.Sprintf("%s VPS memory %.1f%% of its limit", role, memPct)); why != "" {
				return StopVPSRAM
			}
		}
	}
	if why := check(StopSwarmDialerLoaded, s.Local.CPUPct >= swarmDialerMaxPct, fmt.Sprintf("SwarmDialer CPU %.1f%%", s.Local.CPUPct)); why != "" {
		return why
	}
	return ""
}

// idle samples for d in the given phase (baseline or cooldown).
func (r *Runner) idle(st *testState, phase string, d time.Duration) bool {
	r.setPhase(phase)
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		r.sample(st, phase)
		if !r.sleepCtx(sampleInterval) {
			return false
		}
	}
	return true
}

func (r *Runner) runTest(sess *orchestrator.Session, t Test) Result {
	res := Result{Test: t, StartedAt: time.Now()}
	st := &testState{res: &res, t: t, sess: sess, saturated: map[string]int{}}
	r.mu.Lock()
	r.current = &res
	r.mu.Unlock()
	if t.Recording != RecordingOff {
		res.Recording = &RecordingResult{}
		st.ramdiskMB = float64(r.limits["MT"].CallrecRAMMB)
	}
	finish := func(reason string) Result {
		if res.StopReason == "" {
			res.StopReason = reason
		}
		res.FinishedAt = time.Now()
		return res
	}

	r.logf("%s: setting recording %s", t.ID, t.Recording)
	if err := r.env.SetRecording(t.Recording, t.RecordingFormat); err != nil {
		res.Error = err.Error()
		return finish(StopError)
	}
	// The trunk carries the answering side's codec: for a transcoding test
	// (opus caller, ulaw callee) the trunk leg is ulaw and PBXware converts
	// opus <-> ulaw on the calling instance.
	r.logf("%s: preparing trunks for %s", t.ID, t.CalleeCodec)
	release, err := r.env.PrepareTrunk(t.CalleeCodec)
	if err != nil {
		res.Error = err.Error()
		return finish(StopError)
	}

	st.lastStarted = time.Now()
	snap := sess.Snapshot()
	st.lastFailed, st.lastAnswered = snap.TotalCallsFailed, snap.TotalCallsAnswered
	startCounters := snap

	r.logf("%s: %s idle baseline", t.ID, r.profile.Baseline)
	if !r.idle(st, "baseline", r.profile.Baseline) {
		release()
		return finish(StopCancelled)
	}

	var recMu sync.Mutex
	var records []orchestrator.CallRecord
	var batches sync.WaitGroup
	onDone := func(rs []orchestrator.CallRecord) {
		recMu.Lock()
		records = append(records, rs...)
		recMu.Unlock()
		batches.Done()
	}
	callsFrom := time.Now()
	var poller *mp3Poller
	if res.Recording != nil {
		poller = startMP3Poller(r.env, callsFrom, r.logf)
	}

	var reason string
	switch t.Mode {
	case ModeRamp:
		reason = r.runRamp(st, onDone, &batches)
	case ModeRolling:
		reason = r.runRolling(st, onDone, &batches)
	}

	r.setPhase("stopping")
	r.logf("%s: stopping calls (%s)", t.ID, reason)
	sess.Stop()
	waitDone := make(chan struct{})
	go func() { batches.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Minute):
		r.logf("%s: some calls took over 2 minutes to hang up", t.ID)
	}
	release()

	// Cooldown: the host settles (and PBXware converts the last
	// recordings) before the next test; still sampled, for the report.
	r.idle(st, "cooldown", r.profile.Cooldown)
	if poller != nil {
		minDur := 0
		if t.Mode == ModeRolling {
			minDur = int(t.CallDuration.Seconds()) - 2
		}
		poller.finish(res.Recording, time.Minute, minDur)
	}

	recMu.Lock()
	defer recMu.Unlock()
	r.summarize(&res, records, startCounters)
	var answered []orchestrator.CallRecord
	for _, c := range records {
		if c.Answered {
			answered = append(answered, c)
		}
	}
	if len(answered) > 0 {
		sample := answered
		if len(sample) > mosSampleSize { // evenly spread over the test
			step := float64(len(answered)) / mosSampleSize
			sample = make([]orchestrator.CallRecord, mosSampleSize)
			for i := range sample {
				sample[i] = answered[int(float64(i)*step)]
			}
		}
		r.logf("%s: looking up MOS for %d calls", t.ID, len(sample))
		if mos, err := r.env.CallMOS(sample); err != nil {
			r.logf("%s: MOS lookup: %v", t.ID, err)
		} else {
			res.MOS = mos
		}
	}
	return finish(reason)
}

// runRamp places t.Target long calls at t.DialInterval and waits until they
// all run at once (then holds for t.Hold), or a stop condition hits.
func (r *Runner) runRamp(st *testState, onDone func([]orchestrator.CallRecord), batches *sync.WaitGroup) string {
	t := st.t
	r.setPhase("ramp")
	r.logf("%s: ramping to %d calls (%s %s->%s)", t.ID, t.Target, t.Recording, t.CallerCodec, t.CalleeCodec)
	batches.Add(1)
	started := st.sess.AddCallsWithCallee(t.Target, t.CallDuration, t.DialInterval, true, sipua.Codec(t.CallerCodec), sipua.Codec(t.CalleeCodec), onDone)
	if started == 0 {
		batches.Done()
		st.res.StopDetail = "no free extensions to place calls from"
		return StopError
	}
	if started < t.Target {
		r.logf("%s: only %d free extension pairs for %d calls", t.ID, started, t.Target)
	}
	last := st.res.Samples[len(st.res.Samples)-1] // from the baseline, before these calls
	doneBefore := last.Answered + last.Failed
	var holdUntil time.Time
	for {
		if !r.sleepCtx(sampleInterval) {
			return StopCancelled
		}
		phase := "ramp"
		if !holdUntil.IsZero() {
			phase = "hold"
		}
		if why := r.sample(st, phase); why != "" {
			return why
		}
		s := st.res.Samples[len(st.res.Samples)-1]
		dispatched := s.Answered+s.Failed >= doneBefore+uint64(started) // every call has been dialed
		if holdUntil.IsZero() && (s.ConcurrentCalls >= t.Target || (dispatched && s.ConcurrentCalls > 0)) {
			holdUntil = time.Now().Add(t.Hold)
			r.setPhase("hold")
			r.logf("%s: %d calls at once, holding for %s", t.ID, s.ConcurrentCalls, t.Hold)
		}
		if !holdUntil.IsZero() && time.Now().After(holdUntil) {
			if s.ConcurrentCalls >= t.Target {
				return StopTargetReached
			}
			st.res.StopDetail = fmt.Sprintf("%d of %d calls ran at once", st.res.MaxConcurrent, t.Target)
			return StopTargetNotReached
		}
		if dispatched && s.ConcurrentCalls == 0 {
			st.res.StopDetail = "every call ended or failed"
			return StopTargetNotReached
		}
	}
}

// runRolling places t.CallDuration calls continuously at each rate in
// t.RollingCPS for t.StepDuration, so finished calls are replaced as they
// end, until the last step ends or a stop condition hits. Calls are
// started in 10-second chunks so only a few extension pairs are reserved
// ahead of time.
func (r *Runner) runRolling(st *testState, onDone func([]orchestrator.CallRecord), batches *sync.WaitGroup) string {
	t := st.t
	const chunk = 10 * time.Second
	for _, cps := range t.RollingCPS {
		st.cps = cps
		r.setPhase("rolling")
		r.logf("%s: %.1f calls/s (~%.0f at once)", t.ID, cps, cps*t.CallDuration.Seconds())
		stepEnd := time.Now().Add(t.StepDuration)
		nextSample := time.Now().Add(sampleInterval)
		for time.Now().Before(stepEnd) {
			n := int(math.Round(cps * chunk.Seconds()))
			interval := time.Duration(float64(time.Second) / cps)
			batches.Add(1)
			if st.sess.AddCallsWithCallee(n, t.CallDuration, interval, true, sipua.Codec(t.CallerCodec), sipua.Codec(t.CalleeCodec), onDone) == 0 {
				batches.Done()
			}
			chunkEnd := time.Now().Add(chunk)
			for time.Now().Before(chunkEnd) {
				wait := time.Until(nextSample)
				if wait > time.Until(chunkEnd) {
					wait = time.Until(chunkEnd)
				}
				if wait > 0 && !r.sleepCtx(wait) {
					return StopCancelled
				}
				if !time.Now().Before(nextSample) {
					nextSample = nextSample.Add(sampleInterval)
					if why := r.sample(st, "rolling"); why != "" {
						return why
					}
				}
			}
		}
	}
	if st.res.MaxConcurrent >= t.Target {
		return StopTargetReached
	}
	st.res.StopDetail = fmt.Sprintf("%d of %d calls ran at once at the highest rate", st.res.MaxConcurrent, t.Target)
	return StopTargetNotReached
}

// summarize fills in call counts, setup time, RTP and the at-load average.
func (r *Runner) summarize(res *Result, records []orchestrator.CallRecord, _ orchestrator.SessionSnapshot) {
	var setups []float64
	var sent, recv uint64
	for _, rec := range records {
		res.Calls.Started++
		if rec.Answered {
			res.Calls.Answered++
			setups = append(setups, float64(rec.SetupLatency.Milliseconds()))
			sent += rec.RTPSent
			recv += rec.RTPRecv
		} else {
			res.Calls.Failed++
		}
	}
	res.SetupMS = statsOf(setups)
	if sent > 0 {
		res.RTPReceivedRatio = round2(float64(recv) / float64(sent))
	}

	// At load: average of the samples in the hold (ramp), or the last
	// minute of placing calls (rolling, or a test stopped early).
	var pick []Sample
	for _, s := range res.Samples {
		if s.Phase == "hold" {
			pick = append(pick, s)
		}
	}
	if len(pick) == 0 {
		var active []Sample
		for _, s := range res.Samples {
			if s.Phase == "ramp" || s.Phase == "rolling" {
				active = append(active, s)
			}
		}
		if n := len(active); n > 12 {
			active = active[n-12:]
		}
		pick = active
	}
	al := AtLoad{VPSCPUPct: map[string]float64{}, AsteriskCPUPct: map[string]float64{}}
	if len(pick) > 0 {
		n := float64(len(pick))
		for _, s := range pick {
			al.Calls += s.ConcurrentCalls
			al.HostCPUPct += s.Host.CPUPct / n
			al.HostMemPct += s.Host.MemPct / n
			al.SwarmDialerCPU += s.Local.CPUPct / n
			for role, v := range s.VPS {
				al.VPSCPUPct[role] += v.CPUPct / n
				al.AsteriskCPUPct[role] += v.AsteriskCPUPct / n
			}
		}
		al.Calls /= len(pick)
		al.HostCPUPct, al.HostMemPct, al.SwarmDialerCPU = round2(al.HostCPUPct), round2(al.HostMemPct), round2(al.SwarmDialerCPU)
		for k := range al.VPSCPUPct {
			al.VPSCPUPct[k], al.AsteriskCPUPct[k] = round2(al.VPSCPUPct[k]), round2(al.AsteriskCPUPct[k])
		}
	}
	res.AtLoad = al
}

// mp3Poller polls the recording CDRs every few seconds while a recording
// test runs, noting when each call's recording first shows as available
// (MP3 ready). Polling during the test, not afterwards, is what makes the
// delay measurable: accurate to the poll interval.
type mp3Poller struct {
	env  Env
	from time.Time
	stop chan struct{}
	done chan struct{}

	logf func(string, ...any)

	mu         sync.Mutex
	cdrs       map[string]map[string]pbxware.RecordingCDR // role -> uniqueid -> CDR
	firstAvail map[string]map[string]time.Time
	lastErr    error
}

const mp3PollInterval = 3 * time.Second

func startMP3Poller(env Env, from time.Time, logf func(string, ...any)) *mp3Poller {
	p := &mp3Poller{env: env, from: from, logf: logf, stop: make(chan struct{}), done: make(chan struct{}),
		cdrs: map[string]map[string]pbxware.RecordingCDR{}, firstAvail: map[string]map[string]time.Time{}}
	go func() {
		defer close(p.done)
		for {
			p.poll()
			select {
			case <-p.stop:
				return
			case <-time.After(mp3PollInterval):
			}
		}
	}()
	return p
}

func (p *mp3Poller) poll() {
	byRole, err := p.env.RecordingCDRs(p.from.Add(-time.Minute))
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil && (p.lastErr == nil || err.Error() != p.lastErr.Error()) {
		p.logf("reading recording CDRs: %v", err)
	}
	p.lastErr = err
	for role, list := range byRole {
		if p.cdrs[role] == nil {
			p.cdrs[role], p.firstAvail[role] = map[string]pbxware.RecordingCDR{}, map[string]time.Time{}
		}
		for _, c := range list {
			if time.Unix(c.StartUnix, 0).Before(p.from) {
				continue
			}
			p.cdrs[role][c.UniqueID] = c
			if c.Available {
				if _, ok := p.firstAvail[role][c.UniqueID]; !ok {
					p.firstAvail[role][c.UniqueID] = now
				}
			}
		}
	}
}

// allAvailable reports whether every CDR seen so far has its recording.
func (p *mp3Poller) allAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for role, m := range p.cdrs {
		for id, c := range m {
			if _, ok := p.firstAvail[role][id]; !ok && c.Duration >= minRecordedCall {
				return false
			}
		}
	}
	return true
}

// minRecordedCall is the shortest call expected to produce a recording:
// calls cut off within a second or two of starting when a test stops get
// no MP3 from PBXware, which isn't a recording failure.
const minRecordedCall = 5

// finish stops polling, after waiting up to wait for the remaining
// recordings, and fills in rec. Only calls at least minDurS seconds long
// count: for the rolling test that's calls which ran their full length,
// since the calls cut short by the test stopping all hang up at once and
// would make conversion look like it fell behind.
func (p *mp3Poller) finish(rec *RecordingResult, wait time.Duration, minDurS int) {
	if minDurS < minRecordedCall {
		minDurS = minRecordedCall
	}
	deadline := time.Now().Add(wait)
	for !p.allAvailable() && time.Now().Before(deadline) {
		time.Sleep(mp3PollInterval)
	}
	close(p.stop)
	<-p.done

	p.mu.Lock()
	defer p.mu.Unlock()
	rec.MP3ConversionDelayS = map[string]Stats{}
	rec.MP3DelayTrend = map[string]string{}
	rec.MissingRecordings = map[string]int{}
	for role, m := range p.cdrs {
		type ev struct {
			end   time.Time
			delay float64
		}
		var evs []ev
		missing := 0
		for id, c := range m {
			if c.Duration < minDurS {
				continue
			}
			at, ok := p.firstAvail[role][id]
			if !ok {
				missing++
				continue
			}
			end := time.Unix(c.StartUnix+int64(c.Duration), 0)
			evs = append(evs, ev{end, math.Max(0, at.Sub(end).Seconds())})
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].end.Before(evs[j].end) })
		delays := make([]float64, len(evs))
		for i, e := range evs {
			delays[i] = e.delay
		}
		rec.MP3ConversionDelayS[role] = statsOf(delays)
		rec.MissingRecordings[role] = missing
		rec.MP3DelayTrend[role] = trendOf(delays)
	}
}

// trendOf compares the average delay of the first and last quarter of
// calls (ordered by when they ended): "growing" if conversion fell behind
// as the test went on.
func trendOf(delays []float64) string {
	n := len(delays) / 4
	if n < 3 {
		return "stable"
	}
	avg := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}
	first, last := avg(delays[:n]), avg(delays[len(delays)-n:])
	if last > first*1.5 && last-first > 2 {
		return "growing"
	}
	return "stable"
}

// EstimatedDuration is roughly how long a profile takes if no test stops
// early (plus about a minute to register extensions, and ~65s for each
// trunk codec change).
func (p Profile) EstimatedDuration() time.Duration {
	total := time.Minute // registering the extensions
	prevCodec := ""
	for _, t := range p.Tests {
		total += p.Baseline + p.Cooldown + 15*time.Second // stopping
		if t.CalleeCodec != prevCodec && prevCodec != "" {
			total += 65 * time.Second
		}
		prevCodec = t.CalleeCodec
		switch t.Mode {
		case ModeRamp:
			total += time.Duration(t.Target)*t.DialInterval + t.Hold
		case ModeRolling:
			total += time.Duration(len(t.RollingCPS)) * t.StepDuration
		}
	}
	return total
}
