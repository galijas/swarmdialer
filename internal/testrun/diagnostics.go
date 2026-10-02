package testrun

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"swarmdialer/internal/orchestrator"
	"swarmdialer/internal/sipua"
)

// The diagnostics below explain why a test ended the way it did: the load
// when it stopped, how calls failed, PBXware's own view of the calls, the
// health of SwarmDialer itself, and a list of notable events. Everything
// is counts, codes and numbers: no names, addresses or free text.

// AtStop is the load in the sample that ended the test (the one that met a
// stop condition, or the last one before the test ended).
type AtStop struct {
	Calls             int                  `json:"calls"`
	HostCPUPct        float64              `json:"host_cpu_pct"`
	HostMemPct        float64              `json:"host_mem_pct"`
	HostIOWaitPct     float64              `json:"host_iowait_pct"`
	VPS               map[string]VPSAtStop `json:"vps"` // MT, CC
	SwarmDialerCPUPct float64              `json:"swarmdialer_cpu_pct"`
	// RAMDiskEstPct is the estimated recording RAM disk use, as % of its
	// size (recording tests only).
	RAMDiskEstPct *float64 `json:"ramdisk_est_pct,omitempty"`
}

// VPSAtStop is one PBXware VPS's load at the stop. The *_of_limit values
// are nil when the VPS has no such limit.
type VPSAtStop struct {
	CPUPctOfLimit      *float64 `json:"cpu_pct_of_limit,omitempty"`
	CPUPctOfHost       float64  `json:"cpu_pct_of_host"`
	MemPctOfLimit      *float64 `json:"mem_pct_of_limit,omitempty"`
	MemBytes           float64  `json:"mem_bytes"`
	AsteriskCPUPct     float64  `json:"asterisk_cpu_pct"` // % of one core
	PBXwareActiveCalls *float64 `json:"pbxware_active_calls,omitempty"`
}

// Failure is one cause of failed calls in a test.
type Failure struct {
	// Cause is rejected, no_response, no_answer or setup_error (the call
	// never connected), or dropped (PBXware hung up an answered call
	// early) or no_audio (an answered call received no RTP).
	Cause   string `json:"cause"`
	SIPCode int    `json:"sip_code,omitempty"` // rejected: PBXware's response code
	Count   int    `json:"count"`
	// AfterStop is how many of Count failed after the test stopped
	// placing calls: calls still being set up at the stop (included in
	// Count). A cause seen only after the stop has no FirstAt*.
	AfterStop int `json:"after_stop,omitempty"`
	// When this cause first appeared (to the 5-second sample): calls
	// running at once, and seconds since the test started.
	FirstAtCalls *int `json:"first_at_calls,omitempty"`
	FirstAtS     *int `json:"first_at_s,omitempty"`
}

// PBXwareView is the calls as PBXware saw them.
type PBXwareView struct {
	// CDRStatus counts the test's calls by the status in each instance's
	// CDRs (e.g. Answered, Not Answered, Busy, Failed).
	CDRStatus map[string]map[string]int `json:"cdr_status,omitempty"`
	// ActiveCallsPeak is the most calls PBXware itself reported active at
	// once on each VPS (SERVERware's SRW exporter), to compare with
	// SwarmDialer's own count.
	ActiveCallsPeak map[string]float64 `json:"active_calls_peak,omitempty"`
}

// ToolHealth shows whether SwarmDialer itself held up during the test.
type ToolHealth struct {
	SwarmDialerCPUPeakPct float64 `json:"swarmdialer_cpu_peak_pct"`
	// Datagrams SwarmDialer's VPS dropped during the test because a send
	// queue or receive buffer was full (RTP lost before leaving, or before
	// SwarmDialer read it).
	UDPSendErrors    uint64 `json:"udp_send_errors"`
	UDPReceiveErrors uint64 `json:"udp_receive_errors"`
	// UDPSendDropPct is UDPSendErrors as % of everything SwarmDialer's VPS
	// tried to send during the test.
	UDPSendDropPct float64 `json:"udp_send_drop_pct"`
	// The test's extensions: how many failed their last registration at
	// the start and end, and failed re-registrations during the test.
	Extensions             int    `json:"extensions"`
	NotRegisteredAtStart   int    `json:"not_registered_at_start"`
	NotRegisteredAtEnd     int    `json:"not_registered_at_end"`
	ReregistrationFailures uint64 `json:"reregistration_failures"`
	// MediaReceived is the audio SwarmDialer's phones received from each
	// instance (MT: by the calling phones, CC: by the answering phones).
	MediaReceived map[string]MediaQuality `json:"media_received,omitempty"`
}

// MediaQuality is RTP received over a test's calls.
type MediaQuality struct {
	Calls       uint64  `json:"calls"`
	LossPct     float64 `json:"loss_pct"`
	JitterMSAvg float64 `json:"jitter_ms_avg"`
	JitterMSMax float64 `json:"jitter_ms_max"`
}

// Event is one notable moment in a test.
type Event struct {
	TS    int    `json:"t_s"` // seconds since the test started
	Code  string `json:"code"`
	Calls int    `json:"calls"` // calls running at once then
	Role  string `json:"role,omitempty"`
	// Metric and Value: threshold_near (host_cpu, host_mem, vps_cpu,
	// vps_mem, swarmdialer_cpu; % of the stop threshold's base), and
	// quality_degraded (failed_pct, setup_p95_ms).
	Metric  string   `json:"metric,omitempty"`
	Value   *float64 `json:"value,omitempty"`
	Cause   string   `json:"cause,omitempty"`    // failures_started
	SIPCode int      `json:"sip_code,omitempty"` // failures_started
	Reason  string   `json:"reason,omitempty"`   // stop: the stop reason; api_error: the source
	Count   int      `json:"count,omitempty"`    // registration_lost
}

// Event codes.
const (
	EvThresholdNear    = "threshold_near"   // a stop metric passed 80% of its threshold (first time)
	EvQualityDegraded  = "quality_degraded" // see Result.QualityDegradedAtCalls
	EvFailuresStarted  = "failures_started" // a failure cause appeared
	EvRAMDiskFull      = "ramdisk_full_estimated"
	EvRegistrationLost = "registration_lost" // more extensions failed to re-register
	EvAPIError         = "api_error"         // reading Prometheus, CDRs or MOS failed (Reason: the source)
	EvStop             = "stop"              // the test stopped placing calls (Reason)
	EvSendDrops        = "udp_send_drops"    // SwarmDialer's VPS started dropping outgoing packets (Count: in that 5-second sample)
)

const maxEvents = 100

// nearFraction of a stop threshold counts as approaching it.
const nearFraction = 0.8

// diag is a test's diagnostic bookkeeping, alongside testState.
type diag struct {
	start        orchestrator.Health
	prev         orchestrator.Health
	atStop       *orchestrator.Health // set when the test stops placing calls
	udpStart     UDPCounters
	udpPrev      UDPCounters
	udpOK        bool
	dropsSeen    bool
	near         map[string]bool
	apiErrSeen   map[string]bool
	firstFailure map[string][2]int // failure key -> calls, seconds
}

func newDiag(h orchestrator.Health) *diag {
	d := &diag{start: h, prev: h, near: map[string]bool{}, apiErrSeen: map[string]bool{}, firstFailure: map[string][2]int{}}
	d.udpStart, d.udpOK = ReadUDPCounters()
	d.udpPrev = d.udpStart
	return d
}

func f64(v float64) *float64 { v = round2(v); return &v }

// event appends an event to the running test's result.
func (r *Runner) event(st *testState, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(st.res.Events) < maxEvents {
		st.res.Events = append(st.res.Events, e)
	}
}

func (st *testState) sinceStart(t time.Time) int {
	return int(t.Sub(st.res.StartedAt).Seconds())
}

// apiError records the first failure of an API source in a test.
func (r *Runner) apiError(st *testState, source string) {
	if st.d.apiErrSeen[source] {
		return
	}
	st.d.apiErrSeen[source] = true
	calls := 0
	if n := len(st.res.Samples); n > 0 {
		calls = st.res.Samples[n-1].ConcurrentCalls
	}
	r.event(st, Event{TS: st.sinceStart(time.Now()), Code: EvAPIError, Calls: calls, Reason: source})
}

// diagnoseSample notes events from one load sample: stop metrics nearing
// their threshold, new failure causes, and lost registrations.
func (r *Runner) diagnoseSample(st *testState, s Sample) {
	at := st.sinceStart(s.T)
	near := func(key, metric, role string, pct, threshold float64) {
		if pct >= nearFraction*threshold && !st.d.near[key] {
			st.d.near[key] = true
			r.event(st, Event{TS: at, Code: EvThresholdNear, Calls: s.ConcurrentCalls, Role: role, Metric: metric, Value: f64(pct)})
		}
	}
	near("host_cpu", "host_cpu", "", s.Host.CPUPct, saturatedPct)
	near("host_mem", "host_mem", "", s.Host.MemPct, saturatedPct)
	for role, v := range s.VPS {
		lim := r.limits[role]
		if lim.CPULimit > 0 {
			near("vps_cpu:"+role, "vps_cpu", role, v.CPUPct/float64(lim.CPULimit), saturatedPct)
		}
		if lim.MemLimitMB > 0 {
			near("vps_mem:"+role, "vps_mem", role, 100*v.MemBytes/(float64(lim.MemLimitMB)*1024*1024), saturatedPct)
		}
	}
	near("swarmdialer_cpu", "swarmdialer_cpu", "", s.Local.CPUPct, swarmDialerMaxPct)
	if s.UDPSendDrops > 0 && !st.d.dropsSeen {
		st.d.dropsSeen = true
		r.event(st, Event{TS: at, Code: EvSendDrops, Calls: s.ConcurrentCalls, Count: int(s.UDPSendDrops), Metric: "host_cpu", Value: f64(s.Host.CPUPct)})
	}

	h := st.sess.Health()
	if st.d.atStop != nil {
		// After the stop: failures from calls still being set up are
		// counted as after_stop, not as where failures began.
		st.d.prev = h
		return
	}
	for key, n := range h.Failures {
		if n > st.d.start.Failures[key] && st.d.prev.Failures[key] <= st.d.start.Failures[key] {
			st.d.firstFailure[key] = [2]int{s.ConcurrentCalls, at}
			cause, code := splitFailureKey(key)
			r.event(st, Event{TS: at, Code: EvFailuresStarted, Calls: s.ConcurrentCalls, Cause: cause, SIPCode: code})
		}
	}
	for _, x := range []struct {
		key       string
		now, prev uint64
		start     uint64
	}{
		{"dropped", h.Dropped, st.d.prev.Dropped, st.d.start.Dropped},
		{"no_audio", h.NoAudio, st.d.prev.NoAudio, st.d.start.NoAudio},
	} {
		if x.now > x.start && x.prev <= x.start {
			st.d.firstFailure[x.key] = [2]int{s.ConcurrentCalls, at}
			r.event(st, Event{TS: at, Code: EvFailuresStarted, Calls: s.ConcurrentCalls, Cause: x.key})
		}
	}
	if h.NotRegistered > st.d.prev.NotRegistered {
		r.event(st, Event{TS: at, Code: EvRegistrationLost, Calls: s.ConcurrentCalls, Count: h.NotRegistered - st.d.prev.NotRegistered})
	}
	st.d.prev = h
}

// splitFailureKey turns "rejected_503" into ("rejected", 503).
func splitFailureKey(key string) (string, int) {
	if rest, ok := strings.CutPrefix(key, sipua.FailRejected+"_"); ok {
		var code int
		fmt.Sscanf(rest, "%d", &code)
		return sipua.FailRejected, code
	}
	return key, 0
}

// atStop builds the load snapshot from the sample that ended the test.
func (r *Runner) atStop(st *testState) *AtStop {
	var s *Sample
	for i := len(st.res.Samples) - 1; i >= 0; i-- {
		if p := st.res.Samples[i].Phase; p != "baseline" && p != "cooldown" && p != "stopping" {
			s = &st.res.Samples[i]
			break
		}
	}
	if s == nil {
		return nil
	}
	a := &AtStop{
		Calls: s.ConcurrentCalls, HostCPUPct: round2(s.Host.CPUPct), HostMemPct: round2(s.Host.MemPct),
		HostIOWaitPct: round2(s.Host.IOWaitPct), SwarmDialerCPUPct: round2(s.Local.CPUPct), VPS: map[string]VPSAtStop{},
	}
	cpus := float64(max(r.mon.HostCPUs, 1))
	for role, v := range s.VPS {
		lim := r.limits[role]
		x := VPSAtStop{CPUPctOfHost: round2(v.CPUPct / cpus), MemBytes: math.Round(v.MemBytes), AsteriskCPUPct: round2(v.AsteriskCPUPct), PBXwareActiveCalls: v.PBXCalls}
		if lim.CPULimit > 0 {
			x.CPUPctOfLimit = f64(v.CPUPct / float64(lim.CPULimit))
		}
		if lim.MemLimitMB > 0 {
			x.MemPctOfLimit = f64(100 * v.MemBytes / (float64(lim.MemLimitMB) * 1024 * 1024))
		}
		a.VPS[role] = x
	}
	if st.ramdiskMB > 0 && s.RAMDiskEstMB > 0 {
		a.RAMDiskEstPct = f64(100 * s.RAMDiskEstMB / st.ramdiskMB)
	}
	return a
}

// finishDiagnostics fills in the test's failures, PBXware view and tool
// health once its calls have ended.
func (r *Runner) finishDiagnostics(st *testState, callsFrom, stoppedAt time.Time) {
	res := st.res
	end := st.sess.Health()

	// Failures by cause, largest first.
	stopH := end
	if st.d.atStop != nil {
		stopH = *st.d.atStop
	}
	add := func(key string, n, afterStop uint64) {
		if n == 0 {
			return
		}
		cause, code := splitFailureKey(key)
		f := Failure{Cause: cause, SIPCode: code, Count: int(n), AfterStop: int(afterStop)}
		if ff, ok := st.d.firstFailure[key]; ok {
			calls, at := ff[0], ff[1]
			f.FirstAtCalls, f.FirstAtS = &calls, &at
		}
		res.Failures = append(res.Failures, f)
	}
	for key, n := range end.Failures {
		add(key, n-st.d.start.Failures[key], n-stopH.Failures[key])
	}
	add("dropped", end.Dropped-st.d.start.Dropped, end.Dropped-stopH.Dropped)
	add("no_audio", end.NoAudio-st.d.start.NoAudio, end.NoAudio-stopH.NoAudio)
	sort.Slice(res.Failures, func(i, j int) bool { return res.Failures[i].Count > res.Failures[j].Count })

	// Tool health.
	th := &ToolHealth{
		Extensions: end.Extensions, NotRegisteredAtStart: st.d.start.NotRegistered, NotRegisteredAtEnd: end.NotRegistered,
		ReregistrationFailures: end.RefreshFailures - st.d.start.RefreshFailures,
		MediaReceived:          map[string]MediaQuality{},
	}
	for _, s := range res.Samples {
		if s.Phase == "ramp" || s.Phase == "hold" || s.Phase == "rolling" {
			th.SwarmDialerCPUPeakPct = math.Max(th.SwarmDialerCPUPeakPct, round2(s.Local.CPUPct))
		}
	}
	if st.d.udpOK {
		if u, ok := ReadUDPCounters(); ok {
			th.UDPSendErrors = u.SndbufErrors - st.d.udpStart.SndbufErrors
			th.UDPReceiveErrors = (u.RcvbufErrors + u.InErrors) - (st.d.udpStart.RcvbufErrors + st.d.udpStart.InErrors)
			// The kernel counts a datagram dropped at the send queue as a
			// send error and not in OutDatagrams, so all that was attempted
			// is the two together.
			if tried := u.OutDatagrams - st.d.udpStart.OutDatagrams + th.UDPSendErrors; tried > 0 {
				th.UDPSendDropPct = round2(100 * float64(th.UDPSendErrors) / float64(tried))
			}
		}
	}
	for role, pair := range map[string][2]orchestrator.MediaTotals{
		"MT": {st.d.start.CallerMedia, end.CallerMedia},
		"CC": {st.d.start.CalleeMedia, end.CalleeMedia},
	} {
		a, b := pair[0], pair[1]
		calls, expected, received := b.Calls-a.Calls, b.Expected-a.Expected, b.Received-a.Received
		if calls == 0 || expected == 0 {
			continue
		}
		// The peaks were reset when the test started (ResetMediaPeaks), so
		// the end's max is this test's own.
		th.MediaReceived[role] = MediaQuality{Calls: calls, LossPct: round2(100 * float64(expected-received) / float64(expected)),
			JitterMSAvg: round2((b.JitterSumMS - a.JitterSumMS) / float64(calls)), JitterMSMax: round2(b.JitterMaxMS)}
	}
	res.Tool = th

	// PBXware's view: its own active call count, and the CDR outcomes.
	pv := &PBXwareView{ActiveCallsPeak: map[string]float64{}}
	for _, s := range res.Samples {
		for role, v := range s.VPS {
			if v.PBXCalls != nil {
				pv.ActiveCallsPeak[role] = math.Max(pv.ActiveCallsPeak[role], *v.PBXCalls)
			}
		}
	}
	if cdrs, err := r.env.RecordingCDRs(callsFrom.Add(-time.Minute)); err != nil {
		r.logf("%s: reading CDRs: %v", res.Test.ID, err)
		r.apiError(st, "pbxware_cdr")
	} else {
		pv.CDRStatus = map[string]map[string]int{}
		for role, list := range cdrs {
			counts := map[string]int{}
			for _, c := range list {
				if c.StartUnix < callsFrom.Unix() || c.StartUnix > stoppedAt.Unix() {
					continue
				}
				counts[cleanStatus(c.Status)]++
			}
			pv.CDRStatus[role] = counts
		}
	}
	res.PBXware = pv
}

// cleanStatus keeps a CDR status to a short word or phrase (it goes into
// the report).
func cleanStatus(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r == ' ' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 30 {
		out = out[:30]
	}
	if out == "" {
		return "unknown"
	}
	return out
}
