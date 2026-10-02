package sipua

import (
	"errors"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestClassifyAnswerError(t *testing.T) {
	resp := func(code int) *sip.Response {
		return sip.NewResponse(code, "x")
	}
	cases := []struct {
		name  string
		err   error
		last  *sip.Response
		cause string
		code  int
	}{
		{"rejected", &sipgo.ErrDialogResponse{Res: resp(503)}, resp(503), FailRejected, 503},
		{"rejected value type", sipgo.ErrDialogResponse{Res: resp(486)}, nil, FailRejected, 486},
		{"timeout after ringing", errors.New("context deadline exceeded"), resp(180), FailNoAnswer, 0},
		{"no response", errors.New("transaction terminated"), nil, FailNoResponse, 0},
		{"final error seen", errors.New("x"), resp(404), FailRejected, 404},
	}
	for _, c := range cases {
		de := classifyAnswerError(c.err, c.last)
		if de.Cause != c.cause || de.SIPCode != c.code {
			t.Errorf("%s: got %s/%d, want %s/%d", c.name, de.Cause, de.SIPCode, c.cause, c.code)
		}
	}
}
