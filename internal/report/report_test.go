package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmdialer/internal/testrun"
)

func TestBuildTestDiagnostics(t *testing.T) {
	calls, at := 430, 312
	pct := 97.5
	n := 431.0
	res := testrun.Result{
		Test:       testrun.Standard.Tests[1],
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		StopReason: testrun.StopVPSCPU, StopDetail: "MT VPS CPU 390% of its 4-core limit",
		QualityDegradedReason: "p95 call setup 781 ms",
		MOS:                   testrun.MOS{N: 200, Avg: 4.2, Min: 3.1},
		AtStop:                &testrun.AtStop{Calls: 430, VPS: map[string]testrun.VPSAtStop{"MT": {CPUPctOfLimit: &pct, PBXwareActiveCalls: &n}}},
		Failures:              []testrun.Failure{{Cause: "rejected", SIPCode: 503, Count: 37, FirstAtCalls: &calls, FirstAtS: &at}},
		PBXware:               &testrun.PBXwareView{CDRStatus: map[string]map[string]int{"MT": {"Answered": 430}}},
		Tool:                  &testrun.ToolHealth{SwarmDialerCPUPeakPct: 22},
		Events:                []testrun.Event{{TS: 312, Code: testrun.EvFailuresStarted, Calls: 430, Cause: "rejected", SIPCode: 503}},
		Recording: &testrun.RecordingResult{
			MP3ConversionDelayS: map[string]testrun.Stats{"MT": {N: 9, Avg: 3}},
			MP3DelayTrend:       map[string]string{"MT": "stable"},
			MissingRecordings:   map[string]int{"MT": 1},
		},
		Samples: []testrun.Sample{{Phase: "ramp", UDPSendDrops: 1200, VPS: map[string]testrun.VPSMetrics{"MT": {PBXCalls: &n}, "CC": {}}}},
	}
	raw, err := json.Marshal(buildTest(res))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		`"stop_detail":"MT VPS CPU 390% of its 4-core limit"`,
		`"quality_degraded_reason":"p95 call setup 781 ms"`,
		`"n":200`,
		`"at_stop":{"calls":430`,
		`"cpu_pct_of_limit":97.5`,
		`"failures":[{"cause":"rejected","sip_code":503,"count":37,"first_at_calls":430,"first_at_s":312}]`,
		`"cdr_status":{"MT":{"Answered":430}}`,
		`"tool":{"swarmdialer_cpu_peak_pct":22`,
		`"events":[{"t_s":312,"code":"failures_started"`,
		`"mp3_conversion_delay_s_by_instance":{"MT":{"avg":3,"p95":0,"max":0,"n":9,"trend":"stable"}}`,
		`"missing_recordings":{"MT":1}`,
		`"pbxware_active_calls":{"CC":[null],"MT":[431]}`,
		`"swarmdialer_udp_send_drops":[1200]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}
