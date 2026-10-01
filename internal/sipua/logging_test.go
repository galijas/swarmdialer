package sipua

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNonSIPFilter(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(nonSIPFilter{slog.NewTextHandler(&buf, nil)}).With("caller", "Transport<UDP>")
	l.Error("failed to parse", "data", "\x01\x00\x00\x8b\x02\x00EdgeRouter.ER-e300", "error", "line has no CRLF")
	l.Error("failed to parse", "data", "INVITE sip:100@x SIP/2.0\r\nbroken", "error", "bad header")
	l.Error("failed to parse", "data", "SIP/2.0 200 OK\r\nbroken", "error", "bad header")
	l.Warn("something else", "data", "\x01")
	out := buf.String()
	if strings.Contains(out, "EdgeRouter") {
		t.Error("non-SIP datagram was logged")
	}
	for _, want := range []string{"INVITE sip:100", "SIP/2.0 200 OK", "something else"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log:\n%s", want, out)
		}
	}
}
