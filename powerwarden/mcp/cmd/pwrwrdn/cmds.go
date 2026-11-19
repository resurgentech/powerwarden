package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/cli"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
)

// claimPrefix is the id prefix of every claim the CLI makes.
const claimPrefix = "pwrwrdn"

func newFlags(name string, e env) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	tf := fs.String("token-file", "", "token file (default $POWERWARDEN_TOKEN_FILE or ~/.config/powerwarden/token)")
	addURLFlag(fs)
	return fs, tf
}

// addURLFlag registers --url, which every subcommand takes.
func addURLFlag(fs *flag.FlagSet) {
	fs.String("url", "", "powerwarden base URL (default $POWERWARDEN_URL, else the file ~/.config/powerwarden/url)")
}

func urlFlag(fs *flag.FlagSet) string { return fs.Lookup("url").Value.String() }

// parseMixed parses flags that may come before or after positional arguments.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// connect loads the token (refusing a group/other readable file) and builds
// the client.
func connect(e env, fs *flag.FlagSet, tokenFile string) (client.API, client.Config, error) {
	cfg, err := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if err != nil {
		return nil, cfg, err
	}
	tok, err := client.LoadToken(client.TokenPath(tokenFile, e.Getenv, e.Home))
	if err != nil {
		return nil, cfg, err
	}
	api, err := client.New(cfg, tok, nil)
	return api, cfg, err
}

func splitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseMode(s string) (pw.ClaimMode, error) {
	switch s {
	case "", "hold":
		return pw.Hold, nil
	case "reserve":
		return pw.Reserve, nil
	case "dedicated":
		return pw.Dedicated, nil
	}
	return "", fmt.Errorf("--mode %q is not hold, reserve or dedicated", s)
}

func maxSeconds(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := cli.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--max: %w", err)
	}
	return int(d / time.Second), nil
}

func ownerGuess(e env) string {
	h, _ := os.Hostname()
	return e.Getenv("USER") + "@" + cli.ShortHost(h)
}

// claimFlags are the flags `run` and `claim` share.
type claimFlags struct {
	mode, tags, purpose, max, tokenFile *string
}

func addClaimFlags(fs *flag.FlagSet, tf *string) claimFlags {
	return claimFlags{
		mode:      fs.String("mode", "hold", "hold, reserve or dedicated"),
		tags:      fs.String("tags", "", "comma-separated tags a host must cover (a name tag names one host)"),
		purpose:   fs.String("purpose", "", "what the host is for"),
		max:       fs.String("max", "", "hard cap, e.g. 2h or 1d (server clamps to 15m..72h; default 8h)"),
		tokenFile: tf,
	}
}

func (c claimFlags) spec(defPurpose string) (keeper.Spec, error) {
	mode, err := parseMode(*c.mode)
	if err != nil {
		return keeper.Spec{}, err
	}
	tags := splitTags(*c.tags)
	if len(tags) == 0 {
		return keeper.Spec{}, errors.New("--tags: at least one tag is required")
	}
	ms, err := maxSeconds(*c.max)
	if err != nil {
		return keeper.Spec{}, err
	}
	p := *c.purpose
	if p == "" {
		p = defPurpose
	}
	return keeper.Spec{Tags: tags, Purpose: p, Mode: mode, MaxSeconds: ms}, nil
}

func hostOf(c pw.Claim) string {
	if c.Hostname != nil {
		return *c.Hostname
	}
	return "pending"
}

func printClaim(e env, c pw.Claim) {
	fmt.Fprintf(e.Stdout, "claim       %s\nhost        %s\nexpires_at  %s\n", c.Id, hostOf(c), c.ExpiresAt.Local().Format(time.RFC3339))
}

// ---- setup-token ----

func runSetupToken(ctx context.Context, e env) error {
	fs, tf := newFlags("setup-token", e)
	name := fs.String("name", "", "token name in OpenBao (default $USER@<short hostname>)")
	force := fs.Bool("force", false, "overwrite an existing token file")
	baoPath := fs.String("bao-path", "", "OpenBao kv path holding the user tokens (default $PWRWRDN_BAO_PATH)")
	baoMount := fs.String("bao-mount", "", "OpenBao kv mount (default $PWRWRDN_BAO_MOUNT, else kv)")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	if *baoPath == "" {
		*baoPath = e.Getenv("PWRWRDN_BAO_PATH")
	}
	if *baoPath == "" {
		return errors.New("OpenBao path not set: pass --bao-path or set PWRWRDN_BAO_PATH")
	}
	if *baoMount == "" {
		*baoMount = e.Getenv("PWRWRDN_BAO_MOUNT")
	}
	if *baoMount == "" {
		*baoMount = cli.DefaultBaoMount
	}
	if *name == "" {
		if e.Getenv("USER") == "" {
			return errors.New("$USER is empty: pass --name")
		}
		*name = ownerGuess(e)
	}
	path := client.TokenPath(*tf, e.Getenv, e.Home)
	if _, err := os.Lstat(path); err == nil && !*force {
		return fmt.Errorf("%s exists: pass --force to overwrite it", path)
	}
	// The URL file sits beside the token file and is written only when --url
	// is given. Like the token, an existing one needs --force.
	urlFile := filepath.Join(filepath.Dir(path), "url")
	setURL := strings.TrimRight(strings.TrimSpace(urlFlag(fs)), "/")
	if setURL != "" {
		if _, err := os.Lstat(urlFile); err == nil && !*force {
			return fmt.Errorf("%s exists: pass --force to overwrite it", urlFile)
		}
	}
	tok, err := cli.FetchToken(ctx, *baoMount, *baoPath, *name)
	if errors.Is(err, cli.ErrNoSuchName) {
		return fmt.Errorf("OpenBao has no token for %q. Ask the admin to store a user token at %s/%s under field %s, "+
			"then run setup-token again", *name, *baoMount, *baoPath, *name)
	}
	if err != nil {
		return err
	}
	if err := cli.WriteToken(path, tok, *force); err != nil {
		if errors.Is(err, cli.ErrExists) {
			return fmt.Errorf("%s exists: pass --force to overwrite it", path)
		}
		return err
	}
	fmt.Fprintf(e.Stdout, "token for %s written to %s\n", *name, path)
	if setURL != "" {
		// WriteToken is the atomic 0700-dir/0600-file writer; the content is just a line.
		if err := cli.WriteToken(urlFile, setURL, *force); err != nil {
			if errors.Is(err, cli.ErrExists) {
				return fmt.Errorf("%s exists: pass --force to overwrite it", urlFile)
			}
			return err
		}
		fmt.Fprintf(e.Stdout, "url %s written to %s\n", setURL, urlFile)
	}
	return nil
}

// ---- run ----

func runRun(ctx context.Context, e env) error {
	fs, tf := newFlags("run", e)
	cf := addClaimFlags(fs, tf)
	on := fs.Bool("on", false, "send powerup and wait for host_state on (for dedicated claims, which powerwarden never boots)")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return errors.New("usage: pwrwrdn run [flags] -- <cmd> [args...]")
	}
	spec, err := cf.spec("pwrwrdn run: " + argv[0])
	if err != nil {
		return err
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}

	sigs := e.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer signal.Stop(ch)
		sigs = ch
	}

	k := keeper.New(api, keeper.Options{Prefix: claimPrefix, Session: client.NewSessionID()})
	// Release on every way out: normal exit, failure, signal.
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), keeper.ReleaseTimeout)
		defer cancel()
		k.ReleaseAll(rctx)
		k.Close()
	}()

	// A signal while waiting for the claim aborts the wait; the deferred
	// release then runs.
	wctx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	var gotSig atomic.Int32
	stopWatch, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case s := <-sigs:
			if ss, ok := s.(syscall.Signal); ok {
				gotSig.Store(int32(ss))
			} else {
				gotSig.Store(int32(syscall.SIGTERM))
			}
			cancelWait()
		case <-stopWatch:
		}
	}()
	host, id, err := acquire(wctx, e, api, k, spec, *on)
	close(stopWatch)
	<-watchDone
	if s := gotSig.Load(); s != 0 {
		return &exitError{code: 128 + int(s), msg: fmt.Sprintf("interrupted while waiting for the claim; releasing %s", id)}
	}
	if err != nil {
		return err
	}
	return runChild(e, k, sigs, argv, id, host)
}

// acquire makes the leased claim and waits until it is ready (and, with on,
// the host is on). It returns the bound hostname and the claim id.
func acquire(ctx context.Context, e env, api client.API, k *keeper.Keeper, spec keeper.Spec, on bool) (host, id string, err error) {
	c, err := k.Create(ctx, spec)
	if err != nil {
		return "", "", err
	}
	id = c.Id
	fmt.Fprintf(e.Stderr, "pwrwrdn: claim %s (%s), waiting for a host\n", id, spec.Mode)
	got, err := cli.WaitClaim(ctx, api, id, cli.ReadyOrFailed)
	if err != nil {
		return "", id, err
	}
	if got.State == pw.Failed {
		return "", id, fmt.Errorf("claim %s failed: %s", id, got.Detail)
	}
	if got.Hostname == nil {
		return "", id, fmt.Errorf("claim %s is ready without a host", id)
	}
	host = *got.Hostname
	if on {
		if _, _, err := cli.Power(ctx, api, id, string(pw.ClaimPowerActionPowerup)); err != nil {
			return host, id, err
		}
		fmt.Fprintf(e.Stderr, "pwrwrdn: powerup sent to %s, waiting for it to come on\n", host)
		got, err = cli.WaitClaim(ctx, api, id, cli.HostOn)
		if err != nil {
			return host, id, err
		}
		if got.State == pw.Failed {
			return host, id, fmt.Errorf("claim %s failed: %s", id, got.Detail)
		}
	}
	return host, id, nil
}

// runChild runs argv with the claim in its environment, forwards signals to
// it, warns on stderr when the claim is lost, and returns its exit code as an
// exitError.
func runChild(e env, k *keeper.Keeper, sigs <-chan os.Signal, argv []string, id, host string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "POWERWARDEN_CLAIM_ID="+id, "POWERWARDEN_HOST="+host)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return &exitError{code: code, msg: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	warned := map[string]bool{}
	warn := func() {
		for _, l := range k.Lost() {
			if !warned[l.ID] {
				warned[l.ID] = true
				if l.Failed {
					fmt.Fprintf(e.Stderr, "pwrwrdn: claim %s failed: %s\n", l.ID, l.Reason)
					continue
				}
				fmt.Fprintf(e.Stderr, "pwrwrdn: warning: claim %s was lost (%s); the host is no longer held\n", l.ID, l.Reason)
			}
		}
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		case <-tick.C:
			warn()
		case err := <-done:
			warn()
			return exitCode(err)
		}
	}
}

func exitCode(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return &exitError{code: 128 + int(ws.Signal())}
		}
		return &exitError{code: ee.ExitCode()}
	}
	return err
}

// ---- claim ----

func runClaim(ctx context.Context, e env) error {
	fs, tf := newFlags("claim", e)
	cf := addClaimFlags(fs, tf)
	wait := fs.Bool("wait", false, "block until the claim is ready")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	spec, err := cf.spec("pwrwrdn claim by " + ownerGuess(e))
	if err != nil {
		return err
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	c, err := cli.CreateDetached(ctx, api, claimPrefix, client.NewSessionID(), spec)
	if err != nil {
		return err
	}
	if *wait {
		got, err := cli.WaitClaim(ctx, api, c.Id, cli.ReadyOrFailed)
		if err != nil {
			return fmt.Errorf("%w; detached claim %s still exists: pwrwrdn release %s", err, c.Id, c.Id)
		}
		c = &got
		if c.State == pw.Failed {
			return fmt.Errorf("claim %s failed: %s", c.Id, c.Detail)
		}
	}
	printClaim(e, *c)
	return nil
}

// ---- release / extend / power ----

func runRelease(ctx context.Context, e env) error {
	fs, tf := newFlags("release", e)
	pos, err := parseMixed(fs, e.Args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: pwrwrdn release <id>")
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	if err := cli.Release(ctx, api, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "released %s\n", pos[0])
	return nil
}

func runExtend(ctx context.Context, e env) error {
	fs, tf := newFlags("extend", e)
	pos, err := parseMixed(fs, e.Args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: pwrwrdn extend <id> <duration>")
	}
	d, err := cli.ParseDuration(pos[1])
	if err != nil {
		return err
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	resp, err := api.ExtendClaimWithResponse(ctx, pos[0], pw.ClaimExtend{MaxSeconds: int(d / time.Second)})
	if err != nil {
		return err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return errors.New("powerwarden answered 200 without a claim")
	}
	printClaim(e, *resp.JSON200)
	return nil
}

func runPower(ctx context.Context, e env) error {
	fs, tf := newFlags("power", e)
	waitOn := fs.String("wait-on", "", "after the command, wait up to this long for host_state on")
	pos, err := parseMixed(fs, e.Args)
	if err != nil {
		return err
	}
	var id, action string
	switch len(pos) {
	case 1:
		if id = e.Getenv("POWERWARDEN_CLAIM_ID"); id == "" {
			return errors.New("no claim id: pass one or set POWERWARDEN_CLAIM_ID (usage: pwrwrdn power [<id>] <action>)")
		}
		action = pos[0]
	case 2:
		id, action = pos[0], pos[1]
	default:
		return errors.New("usage: pwrwrdn power [<id>] <action> [--wait-on D]")
	}
	var wait time.Duration
	if *waitOn != "" {
		if wait, err = cli.ParseDuration(*waitOn); err != nil {
			return fmt.Errorf("--wait-on: %w", err)
		}
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	cmd, already, err := cli.Power(ctx, api, id, action)
	if err != nil {
		return err
	}
	prefix := ""
	if already {
		prefix = "already there: "
	}
	if cmd != nil {
		fmt.Fprintf(e.Stdout, "%s%s %s on %s (command %d)\n", prefix, cmd.Action, cmd.State, cmd.Hostname, cmd.Id)
	} else {
		fmt.Fprintf(e.Stdout, "%s%s sent\n", prefix, action)
	}
	if wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		got, err := cli.WaitClaim(wctx, api, id, cli.HostOn)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("host did not reach state on within %s", *waitOn)
			}
			return err
		}
		if got.State == pw.Failed {
			return fmt.Errorf("claim %s failed: %s", id, got.Detail)
		}
		fmt.Fprintf(e.Stdout, "%s is on\n", hostOf(got))
	}
	return nil
}

// ---- hosts / claims ----

func runHosts(ctx context.Context, e env) error {
	fs, tf := newFlags("hosts", e)
	tags := fs.String("tags", "", "only hosts carrying all of these tags (explicit or name tags)")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	var p pw.ListHostsParams
	if t := strings.Join(splitTags(*tags), ","); t != "" {
		p.Tags = &t
	}
	resp, err := api.ListHostsWithResponse(ctx, &p)
	if err != nil {
		return err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return err
	}
	var hosts []pw.HostView
	if resp.JSON200 != nil {
		hosts = *resp.JSON200
	}
	cli.HostsTable(e.Stdout, hosts, time.Now())
	return nil
}

func runClaims(ctx context.Context, e env) error {
	fs, tf := newFlags("claims", e)
	mine := fs.Bool("mine", false, "only claims owned by this token")
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	api, _, err := connect(e, fs, *tf)
	if err != nil {
		return err
	}
	var p pw.ListClaimsParams
	if *mine {
		p.Mine = mine
	}
	resp, err := api.ListClaimsWithResponse(ctx, &p)
	if err != nil {
		return err
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return err
	}
	var claims []pw.Claim
	if resp.JSON200 != nil {
		claims = *resp.JSON200
	}
	cli.ClaimsTable(e.Stdout, claims, time.Now())
	return nil
}
