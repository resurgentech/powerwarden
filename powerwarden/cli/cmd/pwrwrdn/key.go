package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/cli"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/cli/internal/keyreq"
)

// newKeyFlags is newFlags plus the flags of a command that may ask for a key.
func newKeyFlags(name string, e env) (*flag.FlagSet, *string) {
	fs, tf := newFlags(name, e)
	addKeyRequestFlags(fs)
	return fs, tf
}

// addKeyRequestFlags registers --key-request-url, --role and --no-key-request.
func addKeyRequestFlags(fs *flag.FlagSet) {
	fs.String("key-request-url", "", "key request URL (default $"+client.EnvKeyRequestURL+"; when neither is set, discovered from the service)")
	fs.String("role", keyreq.DefaultRole, "role to ask for in a key request")
	fs.Bool("no-key-request", false, "do not wait for a person to approve a key request; print the notice and exit 75")
}

// keyOverride is the key request URL: the flag, else the env var.
func keyOverride(e env, fs *flag.FlagSet) string {
	if v := fs.Lookup("key-request-url").Value.String(); v != "" {
		return v
	}
	return e.Getenv(client.EnvKeyRequestURL)
}

func roleFlag(fs *flag.FlagSet) string { return fs.Lookup("role").Value.String() }

func keyParams(e env, fs *flag.FlagSet, cfg client.Config, path string) keyreq.Params {
	return keyreq.Params{ServiceURL: cfg.URL, Override: keyOverride(e, fs), TokenPath: path,
		Role: roleFlag(fs), NameHint: ownerGuess(e)}
}

// ensureKey runs request.Ensure for a command that found no token file. With a
// terminal (and no --no-key-request) it waits for the approval; otherwise it
// returns once the request is filed. The error is an *exitError carrying the
// SR-9 lines when SR-9 words the outcome, request.ErrNoProvider as it is (the
// caller keeps its own message), and any other error as it is.
func ensureKey(ctx context.Context, e env, fs *flag.FlagSet, cfg client.Config, path string) (request.Result, error) {
	wait := e.IsTerminal != nil && e.IsTerminal() && !fs.Lookup("no-key-request").Value.(flag.Getter).Get().(bool)
	hc, err := client.HTTPClient(cfg.CAFile)
	if err != nil {
		return request.Result{}, err
	}
	// The notice goes to stderr as it comes, except with --json and nobody
	// waiting: then it is part of the error object's message.
	show := wait || !e.jsonOut()
	var notified atomic.Bool
	p := keyParams(e, fs, cfg, path)
	p.Wait, p.HTTPClient = wait, hc
	p.Notify = func(n request.Notice) {
		if show {
			notified.Store(true)
			fmt.Fprintln(e.Stderr, n.String())
		}
	}
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	res, err := request.Ensure(ectx, keyreq.Options(p))
	if err == nil {
		return res, nil
	}
	var pending *request.ErrPending
	if errors.As(err, &pending) {
		// Stop the background poll and wait for it, so the lock is free when
		// this command ends. The pending file stays (STATES T-24).
		cancel()
		if d := pending.Done(); d != nil {
			<-d
		}
	}
	lines, code, ok := keyreq.Classify(err, path)
	if !ok {
		return res, err
	}
	if pending != nil && notified.Load() {
		lines = lines[1:] // the notice is shown exactly once
	}
	return res, &exitError{code: code, lines: lines, kind: keyErrorCode(err)}
}

// keyErrorCode is the JSON error code of a key request outcome that
// keyreq.Classify words.
func keyErrorCode(err error) string {
	var pending *request.ErrPending
	var perr *request.ProviderError
	switch {
	case errors.As(err, &pending):
		return "key_pending"
	case errors.Is(err, request.ErrBusy):
		return "key_busy"
	case errors.Is(err, request.ErrDenied):
		return "key_denied"
	case errors.Is(err, request.ErrExpired):
		return "key_expired"
	case errors.Is(err, request.ErrConsumed):
		return "key_consumed"
	case errors.Is(err, request.ErrUnauthorized):
		return "key_unauthorized"
	case errors.As(err, &perr):
		return "key_provider"
	}
	return "error"
}

// keyRequestResult is the JSON form of key request.
type keyRequestResult struct {
	TokenFile string `json:"token_file"`
	Result    string `json:"result"` // already_present or stored
	Name      string `json:"name,omitempty"`
	Role      string `json:"role,omitempty"`
}

func runKeyRequest(ctx context.Context, e env) error {
	fs, tf := newKeyFlags("key request", e)
	if _, err := parse(e, fs, e.Args, false); err != nil {
		return err
	}
	cfg, err := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if err != nil {
		return coded("config", err)
	}
	path := client.TokenPath(*tf, e.Getenv, e.Home)
	if _, lerr := client.LoadToken(path); lerr == nil {
		if e.jsonOut() {
			return cli.WriteJSON(e.Stdout, keyRequestResult{TokenFile: path, Result: "already_present"})
		}
		fmt.Fprintf(e.Stdout, "a key is already at %s\n", path)
		return nil
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return coded("token_file", lerr)
	}
	res, err := ensureKey(ctx, e, fs, cfg, path)
	if errors.Is(err, request.ErrNoProvider) {
		return coded("no_provider", err)
	}
	if err != nil {
		return err
	}
	if e.jsonOut() {
		return cli.WriteJSON(e.Stdout, keyRequestResult{TokenFile: path, Result: "stored", Name: res.Name, Role: res.Role})
	}
	if res.Name != "" {
		fmt.Fprintf(e.Stdout, "key for %s (role %s) stored at %s\n", res.Name, res.Role, path)
	} else {
		fmt.Fprintf(e.Stdout, "key stored at %s\n", path)
	}
	return nil
}

// keyStatusResult is the JSON form of key status.
type keyStatusResult struct {
	TokenFile   string        `json:"token_file"`
	Token       string        `json:"token"` // present or absent
	PendingFile string        `json:"pending_file"`
	Pending     keyStatusPend `json:"pending"`
}

type keyStatusPend struct {
	State     string `json:"state"` // none, pending, expired or foreign
	Service   string `json:"service,omitempty"`
	Code      string `json:"code,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// runKeyStatus shows the token file and the pending request, read only: no
// network, no lock, never Ensure. The pending request is read by keyclient's
// request.Pending with the options a token-loading command would use, so it
// reports on the request this configuration would resume.
func runKeyStatus(_ context.Context, e env) error {
	fs, tf := newFlags("key status", e)
	fs.String("key-request-url", "", "key request URL (default $"+client.EnvKeyRequestURL+")")
	if _, err := parse(e, fs, e.Args, false); err != nil {
		return err
	}
	path := client.TokenPath(*tf, e.Getenv, e.Home)
	out := keyStatusResult{TokenFile: path, Token: "present"}
	if _, err := os.Lstat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return coded("token_file", fmt.Errorf("token file: %w", err))
		}
		out.Token = "absent"
	}
	pp := request.PendingPath(path)
	out.PendingFile = pp
	// emit writes the result: two lines, or one JSON object at the end.
	emit := func(line string) error {
		if e.jsonOut() {
			return cli.WriteJSON(e.Stdout, out)
		}
		fmt.Fprintf(e.Stdout, "token    %s: %s\n", path, out.Token)
		fmt.Fprintf(e.Stdout, "pending  %s: %s\n", pp, line)
		return nil
	}
	override := keyOverride(e, fs)
	cfg, cfgErr := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if cfgErr != nil && override == "" {
		// Nothing to read the request against. With no pending file there is
		// nothing to tell; with one, say plainly why it cannot be checked.
		if _, err := os.Lstat(pp); errors.Is(err, os.ErrNotExist) {
			out.Pending.State = "none"
			return emit("none")
		}
		if !e.jsonOut() {
			fmt.Fprintf(e.Stdout, "token    %s: %s\n", path, out.Token)
		}
		return coded("config", fmt.Errorf("cannot check the pending request without the powerwarden URL: %w", cfgErr))
	}
	n, ok, err := request.Pending(keyreq.Options(keyreq.Params{ServiceURL: cfg.URL, Override: override, TokenPath: path}))
	var line string
	switch {
	case errors.Is(err, request.ErrPendingExpired):
		line, out.Pending.State = "expired", "expired"
	case errors.Is(err, request.ErrPendingForeign):
		line, out.Pending.State = "filed for another service", "foreign"
	case err != nil:
		if !e.jsonOut() {
			fmt.Fprintf(e.Stdout, "token    %s: %s\n", path, out.Token)
		}
		return coded("pending_file", err) // ErrPendingUnusable names the file and says to remove it
	case !ok:
		line, out.Pending.State = "none", "none"
	default:
		exp := n.ExpiresAt.UTC().Format(time.RFC3339)
		line = fmt.Sprintf("service %s, code %s, expires %s", n.Service, n.UserCode, exp)
		out.Pending = keyStatusPend{State: "pending", Service: n.Service, Code: n.UserCode, ExpiresAt: exp}
	}
	return emit(line)
}
