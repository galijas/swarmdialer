// Package testrun runs SwarmDialer's standardized SERVERware host test
// script: a fixed sequence of call-load tests on remote (MT to CC) calls,
// while sampling the host's and the PBXware VPSs' load from SERVERware's
// Prometheus, and stopping each test when the hardware is saturated. The
// results feed the report uploaded to DT Collector (see
// ~/claude/DTcollector_project.md for the agreed test script).
package testrun

import "time"

// Recording modes.
const (
	RecordingOff    = "off"
	RecordingMono   = "mono"
	RecordingStereo = "stereo"
)

// Test modes.
const (
	// ModeRamp adds long calls at DialInterval until Target run at once,
	// then holds for Hold.
	ModeRamp = "ramp"
	// ModeRolling places CallDuration-long calls continuously, replacing
	// them as they end, at each rate in RollingCPS for StepDuration.
	ModeRolling = "rolling"
)

// Test is one test in a profile.
type Test struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	CallerCodec string `json:"caller_codec"`
	CalleeCodec string `json:"callee_codec"` // differs from CallerCodec for a transcoding test
	Recording   string `json:"recording"`
	// RecordingFormat is the mono recording format (call_recordings.format);
	// stereo recordings use PBXware's separate stereo format (wav).
	RecordingFormat string `json:"recording_format"`

	CallDuration time.Duration `json:"call_duration_ns"`
	Target       int           `json:"target_calls"`

	// Ramp mode.
	DialInterval time.Duration `json:"dial_interval_ns"`
	Hold         time.Duration `json:"hold_ns"`

	// Rolling mode.
	RollingCPS   []float64     `json:"rolling_cps"`
	StepDuration time.Duration `json:"step_duration_ns"`
}

// Profile is a versioned, fixed test script. Reports are only comparable
// between runs of the same Name and Version, so never change a published
// profile: add a new version instead.
type Profile struct {
	Name     string        `json:"name"`
	Version  int           `json:"version"`
	Baseline time.Duration `json:"baseline_ns"` // idle measurement before each test
	Cooldown time.Duration `json:"cooldown_ns"` // idle wait/measurement after each test
	Tests    []Test        `json:"tests"`
}

// The PBXware license on the test instances allows 512 concurrent calls.
const standardTarget = 512

func rampTest(id, caller, callee, recording string) Test {
	return Test{
		ID: id, Mode: ModeRamp, CallerCodec: caller, CalleeCodec: callee,
		Recording: recording, RecordingFormat: "wav49",
		// 512 calls at 0.8s spacing take ~7 minutes to start, so 10-minute
		// calls are all still up during the 1-minute hold.
		CallDuration: 10 * time.Minute, Target: standardTarget,
		DialInterval: 800 * time.Millisecond, Hold: time.Minute,
	}
}

// Standard is profile "standard", version 1: the agreed script.
// Low-cost codec: ulaw end to end (PBXware passes the audio through).
// High-cost codec: opus from the caller, ulaw on the answering side, so
// PBXware transcodes every call (codec choice alone costs PBXware nothing
// when it only relays the audio).
var Standard = Profile{
	Name: "standard", Version: 1,
	Baseline: time.Minute, Cooldown: 2 * time.Minute,
	Tests: []Test{
		rampTest("ramp_norec_low", "ulaw", "ulaw", RecordingOff),
		rampTest("ramp_norec_high", "opus", "ulaw", RecordingOff),
		rampTest("ramp_mono_low", "ulaw", "ulaw", RecordingMono),
		rampTest("ramp_mono_high", "opus", "ulaw", RecordingMono),
		rampTest("ramp_stereo_low", "ulaw", "ulaw", RecordingStereo),
		rampTest("ramp_stereo_high", "opus", "ulaw", RecordingStereo),
		{
			ID: "rolling_stereo", Mode: ModeRolling, CallerCodec: "ulaw", CalleeCodec: "ulaw",
			Recording: RecordingStereo, RecordingFormat: "wav49",
			CallDuration: time.Minute, Target: standardTarget,
			// 8.5 calls/s of 1-minute calls is ~510 at once.
			RollingCPS: []float64{2, 4, 6, 8.5}, StepDuration: 90 * time.Second,
		},
	},
}

// Smoke is a short profile for checking the whole pipeline works (a few
// minutes, a handful of calls). Its results are not comparable to
// anything and are never uploaded.
var Smoke = Profile{
	Name: "smoke", Version: 1,
	Baseline: 10 * time.Second, Cooldown: 15 * time.Second,
	Tests: []Test{
		{ID: "ramp_norec_low", Mode: ModeRamp, CallerCodec: "ulaw", CalleeCodec: "ulaw", Recording: RecordingOff, RecordingFormat: "wav49",
			CallDuration: 2 * time.Minute, Target: 10, DialInterval: 800 * time.Millisecond, Hold: 20 * time.Second},
		{ID: "ramp_stereo_high", Mode: ModeRamp, CallerCodec: "opus", CalleeCodec: "ulaw", Recording: RecordingStereo, RecordingFormat: "wav49",
			CallDuration: 2 * time.Minute, Target: 10, DialInterval: 800 * time.Millisecond, Hold: 20 * time.Second},
		{ID: "rolling_stereo", Mode: ModeRolling, CallerCodec: "ulaw", CalleeCodec: "ulaw", Recording: RecordingStereo, RecordingFormat: "wav49",
			CallDuration: 20 * time.Second, Target: 20, RollingCPS: []float64{0.5, 1}, StepDuration: 30 * time.Second},
	},
}

// Profiles by name.
var Profiles = map[string]Profile{Standard.Name: Standard, Smoke.Name: Smoke}

// recordingBytesPerSecond estimates how fast one recorded call fills the
// recording RAM disk. Stereo wav was measured live (2026-09-28: 60 calls
// filled 512 MB in ~5.5 minutes, ~29 KB/s per call); mono rates follow
// from the formats' bitrates (wav49/gsm ~13 kbit/s, wav 128 kbit/s).
func recordingBytesPerSecond(recording, format string) float64 {
	switch recording {
	case RecordingStereo:
		return 29 * 1024
	case RecordingMono:
		switch format {
		case "wav":
			return 16 * 1024
		case "ogg":
			return 2 * 1024
		default: // wav49, gsm
			return 1.65 * 1024
		}
	}
	return 0
}
