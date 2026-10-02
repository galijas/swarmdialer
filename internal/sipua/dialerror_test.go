package sipua

import (
	"errors"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestClassifyAnswerError(t *testing.T) {
	rejected := func(code int) error { return &sipgo.ErrDialogResponse{Res: sip.NewResponse(code, "x")} }
	cases := []struct {
		name               string
		err                error
		timedOut, response bool
		cause              string
		code               int
	}{
		{"rejected before the timeout", rejected(503), false, true, FailRejected, 503},
		{"rejected, value type", sipgo.ErrDialogResponse{Res: sip.NewResponse(486, "x")}, false, true, FailRejected, 486},
		{"timeout after ringing", errors.New("context deadline exceeded"), true, true, FailNoAnswer, 0},
		// Seen live: PBXware's auth challenge arrived after the timeout,
		// during the CANCEL; it isn't a rejection.
		{"late 401 during cancel", errors.New("non provisional response received during CANCEL. resp=SIP/2.0 401"), true, true, FailNoAnswer, 0},
		{"cancel failed 481", errors.New("cancel failed with non 200. code=481"), true, true, FailNoAnswer, 0},
		{"timeout, nothing heard", errors.New("context deadline exceeded"), true, false, FailNoResponse, 0},
		{"transaction terminated", errors.New("transaction terminated"), false, false, FailNoResponse, 0},
	}
	for _, c := range cases {
		de := classifyAnswerError(c.err, c.timedOut, c.response)
		if de.Cause != c.cause || de.SIPCode != c.code {
			t.Errorf("%s: got %s/%d, want %s/%d", c.name, de.Cause, de.SIPCode, c.cause, c.code)
		}
	}
}
