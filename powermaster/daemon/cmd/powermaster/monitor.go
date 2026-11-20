package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/request"
	api "github.com/resurgentech/powerwarden/powermaster/api"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/keyreq"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/tui"
)

// retryDelay is the wait before the one retry of a just-issued key whose first
// service call got 401 (SR-9). Tests shorten it.
var retryDelay = 5 * time.Second

// monitorEnv is what `monitor` takes from its surroundings, so tests can
// replace it.
type monitorEnv struct {
	getenv     func(string) string
	home       string
	stderr     io.Writer
	isTerminal func() bool // is the notice going to a person
	hostname   func() (string, error)
	runTUI     func(context.Context, tui.API, tui.Config) error
}

func expandHomeIn(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home != "" {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// nameHint is $USER@<short host>, the hint a key request carries. With no user
// name it is the short host alone, never "@host" (keyclient refuses an empty
// hint, so the host is the fallback).
func (e monitorEnv) nameHint() string {
	host, _ := e.hostname()
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	user := e.getenv("USER")
	if user == "" {
		return host
	}
	return user + "@" + host
}

func monitorWith(ctx context.Context, args []string, env monitorEnv) error {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (optional; tui section)")
	apiURL := fs.String("api-url", "", "powermaster API base URL (overrides tui.api_url, $POWERMASTER_URL and ~/.config/powermaster/url)")
	tokenFile := fs.String("token-file", "", "file holding the bearer token (overrides tui.token_file)")
	keyURL := fs.String("key-request-url", "", "where to request a key when the token file is missing (default $"+powermaster.EnvKeyRequestURL+", else discovered from the service)")
	role := fs.String("role", "full", "role to ask for in a key request")
	noReq := fs.Bool("no-key-request", false, "file a key request but do not wait for approval: print the notice and exit 75")
	_ = fs.Parse(args)

	cfg := powermaster.DefaultConfig()
	if *cfgPath != "" {
		c, err := powermaster.LoadConfig(*cfgPath)
		if err != nil {
			return err
		}
		cfg = c
	}
	if *tokenFile != "" {
		cfg.TUI.TokenFile = *tokenFile
	}
	baseURL, err := powermaster.ResolveMonitorURL(*apiURL, cfg.TUI.APIURL, env.getenv, env.home)
	if err != nil {
		return err
	}
	tokenPath := expandHomeIn(cfg.TUI.TokenFile, env.home)
	raw, readErr := os.ReadFile(tokenPath)
	fresh := false
	switch {
	case readErr == nil:
	case errors.Is(readErr, os.ErrNotExist):
		var res request.Result
		res, err = ensureKey(ctx, env, baseURL, powermaster.ResolveKeyRequestURL(*keyURL, env.getenv), tokenPath, *role, *noReq)
		if errors.Is(err, request.ErrNoProvider) {
			return fmt.Errorf("token file: %w", readErr) // today's error, unchanged
		}
		if err != nil {
			return err
		}
		raw, fresh = []byte(res.Token), res.Name != ""
	default:
		return fmt.Errorf("token file: %w", readErr)
	}
	token := strings.TrimSpace(string(raw))
	if fresh {
		if err := retryFreshKey(ctx, baseURL, token); err != nil {
			return err
		}
	}
	client, err := tui.NewHTTPClient(baseURL, token)
	if err != nil {
		return err
	}
	return env.runTUI(ctx, client, tui.Config{
		LockedTaps: cfg.TUI.LockedTaps, LockedTapGap: cfg.TUI.LockedTapGap.D(), LockedForceDelay: cfg.TUI.LockedForceDelay.D(),
	})
}

// ensureKey requests a key (SR-9). It returns the keyclient Result, or an
// error: a *keyreq.ExitError for an SR-9 outcome; request.ErrNoProvider and any
// other error as they are.
func ensureKey(ctx context.Context, env monitorEnv, baseURL, override, tokenPath, role string, noRequest bool) (request.Result, error) {
	shown := false
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	res, err := request.Ensure(ectx, request.Options{
		ServiceURL: baseURL,
		Override:   override,
		TokenPath:  tokenPath,
		Role:       role,
		NameHint:   env.nameHint(),
		Tool:       "powermaster/" + version,
		Wait:       env.isTerminal() && !noRequest,
		Notify: func(n request.Notice) {
			shown = true
			fmt.Fprintln(env.stderr, n.String())
		},
	})
	if err == nil {
		return res, nil
	}
	var pending *request.ErrPending
	if errors.As(err, &pending) {
		// End the background poll and release the lock; the pending file stays.
		cancel()
		<-pending.Done()
	}
	if ee := keyreq.Classify(err, tokenPath, shown); ee != nil {
		return res, ee
	}
	return res, err
}

// retryFreshKey makes the first service call with a key just issued. A 401 is
// retried once after retryDelay (the service may not have loaded the key yet);
// a second 401 is an error that holds no token. Any other answer, or no
// answer, lets the monitor start: it reports its own errors.
func retryFreshKey(ctx context.Context, baseURL, token string) error {
	probe := func() (int, error) {
		c, err := api.NewClientWithResponses(baseURL,
			api.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
			api.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
				r.Header.Set("Authorization", "Bearer "+token)
				return nil
			}))
		if err != nil {
			return 0, err
		}
		r, err := c.ListHostsWithResponse(ctx)
		if err != nil {
			return 0, err
		}
		return r.StatusCode(), nil
	}
	if code, err := probe(); err != nil || code != http.StatusUnauthorized {
		return nil
	}
	select {
	case <-time.After(retryDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if code, err := probe(); err == nil && code == http.StatusUnauthorized {
		return errors.New("the service refused the new key (401); remove the token file to ask for another")
	}
	return nil
}
