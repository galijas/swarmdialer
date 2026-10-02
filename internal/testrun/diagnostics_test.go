package testrun

import "testing"

func TestSplitFailureKey(t *testing.T) {
	for key, want := range map[string]struct {
		cause string
		code  int
	}{
		"rejected_503": {"rejected", 503},
		"no_response":  {"no_response", 0},
		"dropped":      {"dropped", 0},
	} {
		c, n := splitFailureKey(key)
		if c != want.cause || n != want.code {
			t.Errorf("%s: got %s/%d", key, c, n)
		}
	}
}

func TestCleanStatus(t *testing.T) {
	for in, want := range map[string]string{
		"Answered":          "Answered",
		" Not Answered ":    "Not Answered",
		"Failed <10.1.1.1>": "Failed ",
		"":                  "unknown",
	} {
		if got := cleanStatus(in); got != want {
			t.Errorf("cleanStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
