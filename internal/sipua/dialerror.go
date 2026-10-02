package sipua

import (
	"errors"
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Why a call failed, as a fixed set of causes (reported per test; the
// free-text error can contain addresses, so it isn't).
const (
	FailRejected   = "rejected"    // PBXware answered the INVITE with an error response (DialError.SIPCode)
	FailNoResponse = "no_response" // no response at all to the INVITE before the dial timeout
	FailNoAnswer   = "no_answer"   // PBXware responded (100/180) but never answered before the dial timeout
	FailSetupError = "setup_error" // a local or protocol error setting the call up (sending, ACK, SDP)
)

// DialError is why Dial failed: Cause is one of the Fail* constants, and
// SIPCode the final response code when Cause is FailRejected.
type DialError struct {
	Cause   string
	SIPCode int
	Err     error
}

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// classifyAnswerError sorts a WaitAnswer failure: an error response from
// PBXware, or a timeout with (no_answer) or without (no_response) any
// provisional response first.
func classifyAnswerError(err error, last *sip.Response) *DialError {
	wrapped := fmt.Errorf("waiting for answer: %w", err)
	var resp *sipgo.ErrDialogResponse
	if errors.As(err, &resp) && resp.Res != nil {
		return &DialError{Cause: FailRejected, SIPCode: resp.Res.StatusCode, Err: wrapped}
	}
	var respV sipgo.ErrDialogResponse
	if errors.As(err, &respV) && respV.Res != nil {
		return &DialError{Cause: FailRejected, SIPCode: respV.Res.StatusCode, Err: wrapped}
	}
	if last != nil && !last.IsProvisional() && !last.IsSuccess() {
		return &DialError{Cause: FailRejected, SIPCode: last.StatusCode, Err: wrapped}
	}
	if last != nil {
		return &DialError{Cause: FailNoAnswer, Err: wrapped}
	}
	return &DialError{Cause: FailNoResponse, Err: wrapped}
}
