// Command powerwarden is the new demand-driven powerwarden
// (docs/DESIGN-powerwarden.md). It is renamed to cmd/powerwarden at cutover.
//
//	powerwarden serve [--config /etc/powerwarden/config.yaml]
//	powerwarden check-config [--config path] [--tokens-file path]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/httpapi"
)

const defaultConfig = "/etc/powerwarden/config.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// quoted matches a backtick-quoted span, which yaml.v3 decode errors use to
// echo a scalar value ("cannot unmarshal !!str `v` into ...").
var quoted = regexp.MustCompile("`[^`]*`")

// checkTokens loads path with the loader serve uses and returns the number of
// full and agent tokens. Errors never carry a token value: the loader's own
// messages name entries only, and yaml's quoted scalars are redacted.
func checkTokens(path string) (full, agent int, err error) {
	if _, err := httpapi.LoadTokens(path); err != nil {
		return 0, 0, errors.New(quoted.ReplaceAllString(err.Error(), "`<redacted>`"))
	}
	// LoadTokens exposes no role counts; the file already passed it, so decode
	// the same shape again and count.
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, errors.New("tokens: cannot reread file")
	}
	var m map[string]httpapi.TokenEntry
	if err := yaml.Unmarshal(b, &m); err != nil {
		return 0, 0, errors.New("tokens: cannot count roles")
	}
	for _, e := range m {
		if e.Role == httpapi.RoleAgent {
			agent++
		} else {
			full++
		}
	}
	return full, agent, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: powerwarden serve|check-config [--config path] [--tokens-file path (check-config)]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfig, "config file")
	var tokensFile *string
	if args[0] == "check-config" {
		tokensFile = fs.String("tokens-file", "", "tokens file (overrides tokens_file in the config)")
	}
	switch args[0] {
	case "check-config":
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := config.Load(*path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tf := cfg.TokensFile
		if *tokensFile != "" {
			tf = *tokensFile
		}
		full, agent, err := checkTokens(tf)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "ok: listen=%s powermaster=%s tokens=%d (full=%d, agent=%d)\n",
			cfg.Listen, cfg.Powermaster.URL, full+agent, full, agent)
		return 0
	case "serve":
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := serve(ctx, *path); err != nil {
			fmt.Fprintln(stderr, "powerwarden:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}
