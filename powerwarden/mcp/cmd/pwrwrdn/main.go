// Command pwrwrdn is the powerwarden CLI for people, agents and scripts: the
// key, setup-token, run, claim, release, extend, power, hosts and claims commands.
//
// It follows the command-line convention in docs/CLI.md: --json on every
// command (none is interactive), help on stdout with exit 0, and the exit
// codes 0, 1, 2 and 75.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/cli"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
)

// env is everything a subcommand needs from the process, injectable in tests.
type env struct {
	Args   []string
	Getenv func(string) string
	Home   string
	Stdin  io.ReadCloser
	Stdout io.WriteCloser
	Stderr io.Writer
	// Signals, when set, replaces the process signal subscription of `run`
	// (tests inject signals here; main leaves it nil).
	Signals <-chan os.Signal
	// IsTerminal reports whether the notice of a key request would reach a
	// person (main: stderr is a terminal). Nil, as in tests, means no.
	IsTerminal func() bool

	// Set by dispatch: the command being run, its path ("key status"), and
	// whether --json is on (a scan of the arguments, then the parsed flag).
	cmd  *command
	path string
	json *bool
}

// jsonOut reports whether the command prints JSON.
func (e env) jsonOut() bool { return e.json != nil && *e.json }

// exitError makes dispatch exit with code. A non-empty msg is printed first,
// after "pwrwrdn: "; lines are printed bare, one per line (the SR-9 key
// request messages). With --json both become the message of the error
// object, whose code is kind (default "usage" for exit 2, else "error"). An
// exitError with neither msg nor lines prints nothing.
type exitError struct {
	code  int
	msg   string
	lines []string
	kind  string
}

func (x *exitError) Error() string { return x.msg }

// usageErr is a usage error: exit 2.
func usageErr(format string, a ...any) error {
	return &exitError{code: 2, kind: "usage", msg: fmt.Sprintf(format, a...)}
}

// errHelp ends a command after its help was printed: exit 0, nothing more.
var errHelp = &exitError{code: 0}

// codedError gives an error the code its JSON error object carries; the text
// is the wrapped error's.
type codedError struct {
	code string
	err  error
}

func (c *codedError) Error() string { return c.err.Error() }
func (c *codedError) Unwrap() error { return c.err }

func coded(code string, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

type command struct {
	summary     string
	usage       string   // after "pwrwrdn <path> "
	description string   // lines, printed indented
	examples    []string // full command lines
	tip         string   // the command's own agent tip, if any
	run         func(ctx context.Context, e env) error
	subs        map[string]*command
}

// commands is the subcommand table; adding one is one entry, with its help.
var commands = map[string]*command{
	"setup-token": {
		summary: "deprecated: use pwrwrdn key request. Fetch your user token from OpenBao into ~/.config/powerwarden/token",
		usage:   "[--name NAME] [--force] [flags]",
		description: "Deprecated: use pwrwrdn key request. Fetches your user token with the bao CLI (your own\n" +
			"OpenBao login) and writes it to the token file, mode 0600. With --url it also writes the URL file.",
		examples: []string{"pwrwrdn setup-token --bao-path path/to/user_tokens", "pwrwrdn setup-token --json"},
		run:      runSetupToken,
	},
	"key": {
		summary: "request a key (key request) or show key state (key status)",
		usage:   "request|status [flags]",
		description: "Keys for the powerwarden API. `key request` asks for one; `key status` shows the token file\n" +
			"and any pending request.",
		examples: []string{"pwrwrdn key request", "pwrwrdn key status --json"},
		subs: map[string]*command{
			"request": {
				summary: "ask for a key now, without another command",
				usage:   "[--role R] [--no-key-request] [flags]",
				description: "Files a key request when there is no token file. In a terminal it waits for a person to\n" +
					"approve it at the address shown, then stores the key (mode 0600). Without a terminal, or with\n" +
					"--no-key-request, it prints the notice and exits 75; approve, then run it again.",
				examples: []string{"pwrwrdn key request", "pwrwrdn key request --no-key-request --json"},
				tip:      "run it in a terminal; it blocks until a person approves (exit 75 means the request is pending).",
				run:      runKeyRequest,
			},
			"status": {
				summary: "show the token file and the pending request",
				usage:   "[flags]",
				description: "Shows whether the token file is present and the pending key request (service, code,\n" +
					"expiry). Reads files only: no network, and never the poll secret.",
				examples: []string{"pwrwrdn key status", "pwrwrdn key status --json"},
				run:      runKeyStatus,
			},
		},
	},
	"run": {
		summary: "claim a host, run a command on it, release on exit",
		usage:   "[--mode M] --tags a,b [--purpose P] [--max D] [--on] [flags] -- <cmd> [args...]",
		description: "Claims a host (leased, renewed while the command runs), waits until the claim is ready, runs\n" +
			"<cmd> with POWERWARDEN_CLAIM_ID and POWERWARDEN_HOST set, and releases the claim when it ends.\n" +
			"pwrwrdn run exits with the command's exit code (128+N if it died of signal N).\n" +
			"With --json it prints one object on stderr with the claim id and host before the command\n" +
			"starts, and one with exit_code after it ends; stdout is the command's.",
		examples: []string{"pwrwrdn run --tags gpu -- make test", "pwrwrdn run --json --mode dedicated --on --tags host01 -- ./bench.sh"},
		tip:      "blocks until the claim is ready (as long as the host takes to free and boot), then for the command.",
		run:      runRun,
	},
	"claim": {
		summary: "make a detached claim and print its id",
		usage:   "[--mode M] --tags a,b [--purpose P] [--max D] [--wait] [flags]",
		description: "Makes a detached claim (no lease: it lives until its cap or pwrwrdn release) and prints it.\n" +
			"--wait blocks until the claim is ready and exits non-zero if it failed.",
		examples: []string{"pwrwrdn claim --tags gpu", "pwrwrdn claim --tags host01 --wait --json"},
		tip:      "release what you claim (pwrwrdn release <id>); prefer run when one command needs the host.",
		run:      runClaim,
	},
	"release": {
		summary:     "release a claim: release <id>",
		usage:       "<id> [flags]",
		description: "Releases (deletes) a claim.",
		examples:    []string{"pwrwrdn release pwrwrdn-0123456789ab", "pwrwrdn release pwrwrdn-0123456789ab --json"},
		run:         runRelease,
	},
	"extend": {
		summary:     "move a claim's hard cap: extend <id> <duration>",
		usage:       "<id> <duration> [flags]",
		description: "Sets the claim's hard cap to now plus the duration (90m, 2h, 1d); the server clamps it.",
		examples:    []string{"pwrwrdn extend pwrwrdn-0123456789ab 2h", "pwrwrdn extend pwrwrdn-0123456789ab 1d --json"},
		run:         runExtend,
	},
	"power": {
		summary: "power the claim's host: power [<id>] <action> [--wait-on D]",
		usage:   "[<id>] <action> [--wait-on D] [flags]",
		description: "Sends powerup, powerdown, powercut or reboot for the claim's host. The id defaults to\n" +
			"$POWERWARDEN_CLAIM_ID (set by pwrwrdn run). --wait-on D then waits up to D for the host to be on.",
		examples: []string{"pwrwrdn power pwrwrdn-0123456789ab powerup --wait-on 10m", "pwrwrdn power reboot --json"},
		tip:      "--wait-on blocks up to its duration.",
		run:      runPower,
	},
	"hosts": {
		summary:     "list hosts, tags, state and claims",
		usage:       "[--tags a,b] [flags]",
		description: "Lists hosts: explicit tags, name tags, power state, whether jobs are blocked, and claims.",
		examples:    []string{"pwrwrdn hosts", "pwrwrdn hosts --tags gpu --json"},
		run:         runHosts,
	},
	"claims": {
		summary:     "list claims",
		usage:       "[--mine] [flags]",
		description: "Lists claims: id, owner, mode, host, state and detail, lease left (or detached), cap left.",
		examples:    []string{"pwrwrdn claims", "pwrwrdn claims --mine --json"},
		run:         runClaims,
	},
}

// commandPaths lists every leaf command of the tree, sorted.
func commandPaths() [][]string {
	var out [][]string
	for _, n := range sortedKeys(commands) {
		c := commands[n]
		if c.subs == nil {
			out = append(out, []string{n})
			continue
		}
		for _, s := range sortedKeys(c.subs) {
			out = append(out, []string{n, s})
		}
	}
	return out
}

// lookup finds the command at path, or nil.
func lookup(path []string) *command {
	m := commands
	var c *command
	for _, p := range path {
		if m == nil {
			return nil
		}
		if c = m[p]; c == nil {
			return nil
		}
		m = c.subs
	}
	return c
}

func sortedKeys(m map[string]*command) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const topHelpText = `Usage:
  pwrwrdn <command> [flags]
  pwrwrdn <command> --help

Description:
  pwrwrdn claims powerwarden hosts, powers them and gives them back, for people, agents and
  scripts. It talks to the powerwarden server's claim API.

Commands:
%s
Flags (every command):
  --json              print one JSON object on stdout; an error is one object on stderr,
                      {"error":{"code":"...","message":"..."}}
  --url URL           powerwarden base URL (default $POWERWARDEN_URL, else ~/.config/powerwarden/url)
  --token-file PATH   token file (default $POWERWARDEN_TOKEN_FILE, else ~/.config/powerwarden/token)
  -h, --help          help for pwrwrdn or a command, on stdout

Exit codes:
  0 success; 1 error; 2 usage; 75 try again later (a key request is pending).
  pwrwrdn run exits with its command's exit code.

Examples:
  pwrwrdn hosts --json
  pwrwrdn claim --tags gpu --wait --json
  pwrwrdn run --tags gpu -- make test
  pwrwrdn claims --mine --json

Agent tips:
  - Use --json. Lists are under a named key ({"hosts":[...]}, {"claims":[...]}); claim and extend
    print the API's Claim object.
  - Blocking: claim --wait blocks until the claim is ready, as long as the host takes to free and
    boot (minutes; no limit), and leaves a detached claim you must pwrwrdn release. pwrwrdn run blocks
    the same way, then runs its command, renews the claim meanwhile and releases it at the end:
    prefer run when one command needs the host. power --wait-on D blocks up to D. key request in a
    terminal blocks until a person approves. Every other command returns after one request.
  - Keys: with no key on this machine, run pwrwrdn key request in a terminal and have a person
    approve it at the address shown. Without a terminal a command files the request and exits 75
    (error code key_pending): approve it, then run the command again. pwrwrdn key status shows the state.
  - Interactive: no pwrwrdn command is interactive; every one takes --json.
  - Durations are 90m, 2h, 1d, 1d12h. A claim id made by pwrwrdn looks like pwrwrdn-<12 hex>.
`

func topHelp(w io.Writer) {
	var b strings.Builder
	for _, p := range commandPaths() {
		name := strings.Join(p, " ")
		fmt.Fprintf(&b, "  %-13s %s\n", name, lookup(p).summary)
	}
	fmt.Fprintf(w, topHelpText, b.String())
}

func indent(w io.Writer, s string) {
	for _, l := range strings.Split(s, "\n") {
		fmt.Fprintf(w, "  %s\n", l)
	}
}

// printHelp prints a command's help: usage, description, flags (fs's, or
// only -h for a group), examples and the agent tip.
func printHelp(w io.Writer, path string, c *command, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage:\n  pwrwrdn %s %s\n\nDescription:\n", path, c.usage)
	indent(w, c.description)
	fmt.Fprintln(w, "\nFlags:")
	if fs != nil {
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	fmt.Fprintln(w, "  -h, --help\n    \tthis help")
	if c.subs != nil {
		fmt.Fprintln(w, "\nCommands:")
		for _, s := range sortedKeys(c.subs) {
			fmt.Fprintf(w, "  %-8s %s\n", s, c.subs[s].summary)
		}
	}
	fmt.Fprintln(w, "\nExamples:")
	for _, ex := range c.examples {
		fmt.Fprintf(w, "  %s\n", ex)
	}
	if c.tip != "" {
		fmt.Fprintf(w, "\nAgent tip:\n  %s\n", c.tip)
	}
}

func isHelpFlag(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// wantsJSON scans the arguments before any `--` for a true --json, so errors
// found before or while parsing flags can still be JSON.
func wantsJSON(args []string) bool {
	on := false
	for _, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "json" {
			continue
		}
		on = true
		if hasVal {
			on, _ = strconv.ParseBool(val)
		}
	}
	return on
}

// parse parses args into fs (with mixed, flags may sit between positional
// arguments). -h or --help prints the command's help on stdout and returns
// errHelp; a bad flag is a usage error. It returns the positional arguments.
func parse(e env, fs *flag.FlagSet, args []string, mixed bool) ([]string, error) {
	var pos []string
	var err error
	if mixed {
		pos, err = parseMixed(fs, args)
	} else if err = fs.Parse(args); err == nil {
		pos = fs.Args()
	}
	if errors.Is(err, flag.ErrHelp) {
		if e.cmd != nil {
			printHelp(e.Stdout, e.path, e.cmd, fs)
		}
		return nil, errHelp
	}
	if err != nil {
		return nil, usageErr("%v (see pwrwrdn %s --help)", err, e.path)
	}
	if f := fs.Lookup("json"); f != nil && e.json != nil {
		*e.json = f.Value.(flag.Getter).Get().(bool)
	}
	return pos, nil
}

func dispatch(ctx context.Context, e env) int {
	js := wantsJSON(e.Args)
	e.json = &js
	if len(e.Args) == 0 {
		topHelp(e.Stderr)
		return 2
	}
	if isHelpFlag(e.Args[0]) {
		topHelp(e.Stdout)
		return 0
	}
	if strings.HasPrefix(e.Args[0], "-") {
		return report(e, usageErr("a command comes first, not %q (see pwrwrdn --help)", e.Args[0]))
	}
	path := e.Args[0]
	c, ok := commands[path]
	if !ok {
		return report(e, usageErr("unknown subcommand %q (see pwrwrdn --help)", path))
	}
	e.Args = e.Args[1:]
	if c.subs != nil {
		groupUsage := usageErr("usage: pwrwrdn %s %s", path, c.usage)
		if len(e.Args) == 0 {
			return report(e, groupUsage)
		}
		if isHelpFlag(e.Args[0]) {
			printHelp(e.Stdout, path, c, nil)
			return 0
		}
		s, ok := c.subs[e.Args[0]]
		if !ok {
			return report(e, groupUsage)
		}
		path += " " + e.Args[0]
		e.Args = e.Args[1:]
		c = s
	}
	e.cmd, e.path = c, path
	if err := c.run(ctx, e); err != nil {
		return report(e, err)
	}
	return 0
}

// report prints err (as text, or with --json as the error object on stderr)
// and returns the exit code.
func report(e env, err error) int {
	code, kind, msg, lines := describe(err)
	if e.jsonOut() {
		if msg == "" && len(lines) == 0 {
			return code
		}
		text := lines
		if msg != "" {
			text = append([]string{msg}, lines...)
		}
		_ = cli.WriteError(e.Stderr, kind, strings.Join(text, "\n"))
		return code
	}
	if msg != "" {
		fmt.Fprintf(e.Stderr, "pwrwrdn: %s\n", msg)
	}
	for _, l := range lines {
		fmt.Fprintln(e.Stderr, l)
	}
	return code
}

// describe maps an error to its exit code, its JSON error code, and its text.
func describe(err error) (code int, kind, msg string, lines []string) {
	var x *exitError
	if errors.As(err, &x) {
		kind = x.kind
		if kind == "" {
			kind = "error"
			if x.code == 2 {
				kind = "usage"
			}
		}
		return x.code, kind, x.msg, x.lines
	}
	var ce *codedError
	var ref *client.Refusal
	switch {
	case errors.As(err, &ce):
		kind = ce.code
	case errors.As(err, &ref):
		kind = ref.Code
		if kind == "" {
			kind = fmt.Sprintf("http_%d", ref.Status)
		}
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	default:
		kind = "error"
	}
	return 1, kind, err.Error(), nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	home, _ := os.UserHomeDir()
	os.Exit(dispatch(ctx, env{Args: os.Args[1:], Getenv: os.Getenv, Home: home,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTerminal: func() bool { return request.IsTerminal(os.Stderr) }}))
}
