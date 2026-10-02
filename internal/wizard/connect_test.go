package wizard

import "testing"

func TestTrunkName(t *testing.T) {
	for in, want := range map[string]string{
		"CC":              "SwarmDialer-to-CC",
		"MT Test":         "SwarmDialer-to-MT-Test",
		"DT_CC.v2":        "SwarmDialer-to-DT_CC.v2",
		"Call Centre (2)": "SwarmDialer-to-Call-Centre-2",
		"Zagreb – čćž":    "SwarmDialer-to-Zagreb",
		"!!!":             "SwarmDialer-to-peer",
	} {
		if got := trunkName(in); got != want {
			t.Errorf("trunkName(%q) = %q, want %q", in, got, want)
		}
	}
}
