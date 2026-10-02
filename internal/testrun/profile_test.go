package testrun

import "testing"

func TestRollingTargets(t *testing.T) {
	for _, p := range []Profile{Standard, Smoke} {
		for _, tc := range p.Tests {
			if tc.Mode != ModeRolling {
				continue
			}
			peak := tc.RollingCPS[len(tc.RollingCPS)-1] * tc.CallDuration.Seconds()
			if float64(tc.Target) > peak {
				t.Errorf("%s/%s: target %d is above the %v calls the highest rate can run", p.Name, tc.ID, tc.Target, peak)
			}
		}
	}
	if got := Standard.Tests[6].Target; got != 510 {
		t.Errorf("standard rolling target = %d, want 510", got)
	}
}
