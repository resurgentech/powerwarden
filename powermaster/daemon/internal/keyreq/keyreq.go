// Package keyreq maps the outcomes of a first-use key request (keyclient's
// request.Ensure) to what a command line tool prints and the exit code it
// returns. The table follows SR-9 of the key-request plan; pwrwrdn keeps its
// own copy of the same strings, so the two stay independent.
package keyreq

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/request"
)

// Exit codes.
const (
	// ExitPending (EX_TEMPFAIL): nothing is wrong; run again after approving.
	ExitPending = 75
	// ExitFailed is the exit code of every other failure.
	ExitFailed = 1
)

// PendingLine follows the notice when the request is still open.
const PendingLine = "key request pending; run this again after approving"

// DeniedLine is printed when a person denied the request.
const DeniedLine = "key request denied"

// RetryLine follows keyclient's message for an expired or consumed request.
const RetryLine = "run this again to file a new request"

// BusyLine is the message for ErrBusy; %s is the token path.
const BusyLine = "another process is handling the key request for %s; try again shortly"

// ExitError carries the lines to print (bare, one per line, on stderr) and the
// exit code. It never holds a token, a poll secret or a key: its lines come
// from keyclient's sanitised messages and fixed text.
type ExitError struct {
	Lines []string
	Code  int
}

func (e *ExitError) Error() string { return strings.Join(e.Lines, "; ") }

// Classify maps an error from request.Ensure to an ExitError. It returns nil
// for errors that are not SR-9 outcomes (including request.ErrNoProvider, which
// the caller keeps as its usual "no token" error); the caller then reports the
// error as it does today. noticeShown says Notify already printed the notice,
// so an *ErrPending does not print it a second time.
func Classify(err error, tokenPath string, noticeShown bool) *ExitError {
	var pending *request.ErrPending
	var perr *request.ProviderError
	switch {
	case errors.As(err, &pending):
		var lines []string
		if !noticeShown {
			lines = append(lines, pending.Notice.String())
		}
		return &ExitError{Lines: append(lines, PendingLine), Code: ExitPending}
	case errors.Is(err, request.ErrBusy):
		return &ExitError{Lines: []string{fmt.Sprintf(BusyLine, tokenPath)}, Code: ExitPending}
	case errors.Is(err, request.ErrDenied):
		return &ExitError{Lines: []string{DeniedLine}, Code: ExitFailed}
	case errors.Is(err, request.ErrExpired), errors.Is(err, request.ErrConsumed):
		return &ExitError{Lines: []string{err.Error(), RetryLine}, Code: ExitFailed}
	case errors.Is(err, request.ErrUnauthorized):
		return &ExitError{Lines: []string{err.Error()}, Code: ExitFailed}
	case errors.As(err, &perr):
		lines := []string{err.Error()}
		if perr.RetryAfter > 0 {
			secs := int((perr.RetryAfter + time.Second - 1) / time.Second)
			lines = append(lines, fmt.Sprintf("try again in %ds", secs))
		}
		return &ExitError{Lines: lines, Code: ExitFailed}
	}
	return nil
}
