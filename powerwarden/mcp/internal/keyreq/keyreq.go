// Package keyreq is what pwrwrdn adds to keyclient's request flow (SR-9 of the
// plan; keyclient's docs/CONTRACT.md and docs/STATES.md define the flow
// itself): the request options, the table that turns each outcome into the
// words and exit code SR-9 gives it, the MCP texts and gate, the retry-once
// transport for a just-issued key. `pwrwrdn key status` reads the pending
// request through keyclient's request.Pending; nothing here knows the pending
// file's format.
//
// Nothing here opens an approval URL, and nothing here prints or returns a
// poll secret or a key.
package keyreq

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
)

const (
	// DefaultRole is the role pwrwrdn asks for unless --role says otherwise.
	DefaultRole = "agent"
	// ExitTempFail is the exit code (EX_TEMPFAIL) for "run this again": a
	// pending request, or another process handling it.
	ExitTempFail = 75

	// PendingLine follows the notice when the command does not wait.
	PendingLine = "key request pending; run this again after approving"
	// NewRequestLine follows keyclient's message for an expired or consumed request.
	NewRequestLine = "run this again to file a new request"
	// MCPBusyText is the tool result when another pwrwrdn files the request.
	MCPBusyText = "another pwrwrdn is handling the key request; retry shortly"
	// DeniedLine is the message for a denied request.
	DeniedLine = "key request denied"

	mcpNoKey = "No powerwarden key on this machine yet. "
	mcpRetry = " Then retry."
)

// Tool is the Tool of every key request: pwrwrdn/<version>.
func Tool() string { return "pwrwrdn/" + client.Version }

// Params are what a pwrwrdn command knows; Options fills in the rest.
type Params struct {
	ServiceURL string // the resolved service URL
	Override   string // --key-request-url or $POWERWARDEN_KEY_REQUEST_URL
	TokenPath  string // the existing token path
	Role       string // "" means DefaultRole
	NameHint   string // $USER@<short host>
	Wait       bool
	Notify     func(request.Notice)
	HTTPClient *http.Client
}

// Options builds the request.Options of SR-9. Host stays at its default and
// AllowInsecureHTTP is never set.
func Options(p Params) request.Options {
	role := p.Role
	if role == "" {
		role = DefaultRole
	}
	return request.Options{
		ServiceURL: p.ServiceURL, Override: p.Override, TokenPath: p.TokenPath,
		Role: role, NameHint: nameHint(p.NameHint), Tool: Tool(),
		HTTPClient: p.HTTPClient, Notify: p.Notify, Wait: p.Wait,
	}
}

// nameHint turns the owner guess ($USER@<short host>) into the NameHint. With
// no user the guess is "@<host>" (or empty): the hint is then the short host
// alone, never "@host", and never empty (keyclient requires one).
func nameHint(guess string) string {
	g := strings.TrimPrefix(guess, "@")
	if g != "" {
		return g
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
}

// Classify maps an Ensure error to the SR-9 lines (printed bare, one per
// line) and exit code. ok is false for what SR-9 leaves to the normal error
// path: ErrNoProvider (the caller keeps today's "no token" error), option
// errors, discovery failures, an unsupported platform, a cancelled context.
func Classify(err error, tokenPath string) (lines []string, code int, ok bool) {
	if err == nil {
		return nil, 0, false
	}
	var pending *request.ErrPending
	var perr *request.ProviderError
	switch {
	case errors.As(err, &pending):
		return []string{pending.Notice.String(), PendingLine}, ExitTempFail, true
	case errors.Is(err, request.ErrBusy):
		return []string{"another process is handling the key request for " + tokenPath + "; try again shortly"}, ExitTempFail, true
	case errors.Is(err, request.ErrDenied):
		return []string{DeniedLine}, 1, true
	case errors.Is(err, request.ErrExpired), errors.Is(err, request.ErrConsumed):
		return []string{err.Error(), NewRequestLine}, 1, true
	case errors.Is(err, request.ErrUnauthorized):
		return []string{err.Error()}, 1, true
	case errors.As(err, &perr):
		lines = []string{err.Error()}
		if perr.RetryAfter > 0 {
			secs := int((perr.RetryAfter + time.Second - 1) / time.Second)
			lines = append(lines, fmt.Sprintf("try again in %ds", secs))
		}
		return lines, 1, true
	}
	return nil, 0, false
}

// PendingMCPText is the tool result while no key has landed: the notice, which
// holds the approval address and the code, never a poll secret.
func PendingMCPText(n request.Notice) string { return mcpNoKey + n.String() + mcpRetry }
