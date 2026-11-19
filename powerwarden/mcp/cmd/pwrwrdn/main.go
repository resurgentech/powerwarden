// Command pwrwrdn is the powerwarden agent CLI: the `mcp` server plus the
// setup-token, run, claim, release, extend, power, hosts and claims commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
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
}

// exitError makes dispatch exit with code. A non-empty msg is printed first.
type exitError struct {
	code int
	msg  string
}

func (x *exitError) Error() string { return x.msg }

type command struct {
	summary string
	run     func(ctx context.Context, e env) error
}

// commands is the subcommand table; adding one is one entry.
var commands = map[string]command{
	"mcp":         {"serve MCP tools on stdio, one process per agent session", runMCP},
	"setup-token": {"fetch your user token from OpenBao into ~/.config/powerwarden/token", runSetupToken},
	"run":         {"claim a host, run a command on it, release on exit", runRun},
	"claim":       {"make a detached claim and print its id", runClaim},
	"release":     {"release a claim: release <id>", runRelease},
	"extend":      {"move a claim's hard cap: extend <id> <duration>", runExtend},
	"power":       {"power the claim's host: power [<id>] <action> [--wait-on D]", runPower},
	"hosts":       {"list hosts, tags, state and claims", runHosts},
	"claims":      {"list claims", runClaims},
}

func usage(w io.Writer) {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "usage: pwrwrdn <subcommand> [flags]")
	for _, n := range names {
		fmt.Fprintf(w, "  %-12s %s\n", n, commands[n].summary)
	}
}

func dispatch(ctx context.Context, e env) int {
	if len(e.Args) == 0 || strings.HasPrefix(e.Args[0], "-") {
		usage(e.Stderr)
		return 2
	}
	c, ok := commands[e.Args[0]]
	if !ok {
		fmt.Fprintf(e.Stderr, "pwrwrdn: unknown subcommand %q\n", e.Args[0])
		usage(e.Stderr)
		return 2
	}
	e.Args = e.Args[1:]
	if err := c.run(ctx, e); err != nil {
		var x *exitError
		if errors.As(err, &x) {
			if x.msg != "" {
				fmt.Fprintf(e.Stderr, "pwrwrdn: %s\n", x.msg)
			}
			return x.code
		}
		fmt.Fprintf(e.Stderr, "pwrwrdn: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	home, _ := os.UserHomeDir()
	os.Exit(dispatch(ctx, env{Args: os.Args[1:], Getenv: os.Getenv, Home: home,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
