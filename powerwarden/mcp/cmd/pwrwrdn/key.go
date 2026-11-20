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
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keyreq"
)

// newKeyFlags is newFlags plus the flags of a command that may ask for a key.
func newKeyFlags(name string, e env) (*flag.FlagSet, *string) {
	fs, tf := newFlags(name, e)
	addKeyRequestFlags(fs, true)
	return fs, tf
}

// addKeyRequestFlags registers --key-request-url and --role, and with
// noKeyRequest also --no-key-request (the MCP server never waits, so it has
// no use for it).
func addKeyRequestFlags(fs *flag.FlagSet, noKeyRequest bool) {
	fs.String("key-request-url", "", "key request URL (default $"+client.EnvKeyRequestURL+"; when neither is set, discovered from the service)")
	fs.String("role", keyreq.DefaultRole, "role to ask for in a key request")
	if noKeyRequest {
		fs.Bool("no-key-request", false, "do not wait for a person to approve a key request; print the notice and exit 75")
	}
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
	var notified atomic.Bool
	p := keyParams(e, fs, cfg, path)
	p.Wait, p.HTTPClient = wait, hc
	p.Notify = func(n request.Notice) {
		notified.Store(true)
		fmt.Fprintln(e.Stderr, n.String())
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
	return res, &exitError{code: code, lines: lines}
}

func usageKey() error {
	return &exitError{code: 2, msg: "usage: pwrwrdn key request|status [flags]"}
}

// runKey is `pwrwrdn key request|status`.
func runKey(ctx context.Context, e env) error {
	if len(e.Args) == 0 {
		return usageKey()
	}
	sub := e.Args[0]
	e.Args = e.Args[1:]
	switch sub {
	case "request":
		return runKeyRequest(ctx, e)
	case "status":
		return runKeyStatus(e)
	}
	return usageKey()
}

func runKeyRequest(ctx context.Context, e env) error {
	fs, tf := newKeyFlags("key request", e)
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	cfg, err := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if err != nil {
		return err
	}
	path := client.TokenPath(*tf, e.Getenv, e.Home)
	if _, lerr := client.LoadToken(path); lerr == nil {
		fmt.Fprintf(e.Stdout, "a key is already at %s\n", path)
		return nil
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return lerr
	}
	res, err := ensureKey(ctx, e, fs, cfg, path)
	if err != nil {
		return err
	}
	if res.Name != "" {
		fmt.Fprintf(e.Stdout, "key for %s (role %s) stored at %s\n", res.Name, res.Role, path)
	} else {
		fmt.Fprintf(e.Stdout, "key stored at %s\n", path)
	}
	return nil
}

// runKeyStatus shows the token file and the pending request, read only: no
// network, no lock, never Ensure. The pending request is read by keyclient's
// request.Pending with the options a token-loading command would use, so it
// reports on the request this configuration would resume.
func runKeyStatus(e env) error {
	fs, tf := newFlags("key status", e)
	fs.String("key-request-url", "", "key request URL (default $"+client.EnvKeyRequestURL+")")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	path := client.TokenPath(*tf, e.Getenv, e.Home)
	state := "present"
	if _, err := os.Lstat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("token file: %w", err)
		}
		state = "absent"
	}
	fmt.Fprintf(e.Stdout, "token    %s: %s\n", path, state)
	pp := request.PendingPath(path)
	override := keyOverride(e, fs)
	cfg, cfgErr := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if cfgErr != nil && override == "" {
		// Nothing to read the request against. With no pending file there is
		// nothing to tell; with one, say plainly why it cannot be checked.
		if _, err := os.Lstat(pp); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(e.Stdout, "pending  %s: none\n", pp)
			return nil
		}
		return fmt.Errorf("cannot check the pending request without the powerwarden URL: %w", cfgErr)
	}
	n, ok, err := request.Pending(keyreq.Options(keyreq.Params{ServiceURL: cfg.URL, Override: override, TokenPath: path}))
	var line string
	switch {
	case errors.Is(err, request.ErrPendingExpired):
		line = "expired"
	case errors.Is(err, request.ErrPendingForeign):
		line = "filed for another service"
	case err != nil:
		return err // ErrPendingUnusable names the file and says to remove it
	case !ok:
		line = "none"
	default:
		line = fmt.Sprintf("service %s, code %s, expires %s", n.Service, n.UserCode, n.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(e.Stdout, "pending  %s: %s\n", pp, line)
	return nil
}
