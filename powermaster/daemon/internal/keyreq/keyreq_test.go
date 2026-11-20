package keyreq

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/request"
)

// TestSR9Table pins every SR-9 string and exit code, character for character.
func TestSR9Table(t *testing.T) {
	notice := request.Notice{
		Service: "pm.example", ApproveURL: "https://keys.example/approve/abc", UserCode: "ABCD-EFGH",
		Role: "full", Tool: "powermaster/0.4.0", ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	const tok = "/home/u/.config/powermaster/token"
	cases := []struct {
		name  string
		err   error
		shown bool
		lines []string
		code  int
	}{
		{"pending, notice not yet shown", &request.ErrPending{Notice: notice}, false,
			[]string{notice.String(), "key request pending; run this again after approving"}, 75},
		{"pending, notice already shown", &request.ErrPending{Notice: notice}, true,
			[]string{"key request pending; run this again after approving"}, 75},
		{"busy", request.ErrBusy, false,
			[]string{"another process is handling the key request for " + tok + "; try again shortly"}, 75},
		{"denied", request.ErrDenied, false, []string{"key request denied"}, 1},
		{"expired", request.ErrExpired, false,
			[]string{request.ErrExpired.Error(), "run this again to file a new request"}, 1},
		{"consumed", request.ErrConsumed, false,
			[]string{request.ErrConsumed.Error(), "run this again to file a new request"}, 1},
		{"unauthorized", request.ErrUnauthorized, false, []string{request.ErrUnauthorized.Error()}, 1},
		{"provider error", &request.ProviderError{Status: 503, Message: "down"}, false,
			[]string{"key provider refused the request (status 503): down"}, 1},
		{"provider error with Retry-After rounds up", &request.ProviderError{Status: 429, Message: "slow", RetryAfter: 1500 * time.Millisecond}, false,
			[]string{"key provider refused the request (status 429): slow (retry after 1.5s)", "try again in 2s"}, 1},
		{"provider error with whole seconds", &request.ProviderError{Status: 429, RetryAfter: 30 * time.Second}, false,
			[]string{"key provider refused the request (status 429) (retry after 30s)", "try again in 30s"}, 1},
		{"wrapped denied", fmt.Errorf("x: %w", request.ErrDenied), false, []string{"key request denied"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.err, tok, c.shown)
			if got == nil {
				t.Fatalf("Classify(%v) = nil", c.err)
			}
			if !reflect.DeepEqual(got.Lines, c.lines) || got.Code != c.code {
				t.Fatalf("got %q exit %d, want %q exit %d", got.Lines, got.Code, c.lines, c.code)
			}
		})
	}
}

// ErrNoProvider and unknown errors are not SR-9 outcomes: the caller reports
// them as it does today.
func TestClassifyLeavesOthers(t *testing.T) {
	for _, err := range []error{request.ErrNoProvider, errors.New("boom"), nil} {
		if got := Classify(err, "/x", false); got != nil {
			t.Errorf("Classify(%v) = %v, want nil", err, got)
		}
	}
}
