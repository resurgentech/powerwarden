package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keyreq"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/mcpserver"
)

func runMCP(ctx context.Context, e env) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	tokenFile := fs.String("token-file", "", "token file (default $POWERWARDEN_TOKEN_FILE or ~/.config/powerwarden/token)")
	addURLFlag(fs)
	addKeyRequestFlags(fs, false)
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	cfg, err := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if err != nil {
		return err
	}
	path := client.TokenPath(*tokenFile, e.Getenv, e.Home)
	session := client.NewSessionID()
	newKeeper := func(api client.API) *keeper.Keeper {
		return keeper.New(api, keeper.Options{Prefix: "mcp", Session: session})
	}
	var mc mcpserver.Config
	tok, lerr := client.LoadToken(path)
	switch {
	case lerr == nil:
		api, err := client.New(cfg, tok, nil)
		if err != nil {
			return err
		}
		mc = mcpserver.Config{API: api, Keeper: newKeeper(api)}
	case errors.Is(lerr, os.ErrNotExist):
		// Start without a key: the first tool call asks for one (D10).
		gate, err := keyreq.NewGate(ctx, cfg, keyParams(e, fs, cfg, path))
		if err != nil {
			return err
		}
		mc = mcpserver.Config{Connect: gate.Connect, NewKeeper: newKeeper}
	default:
		return lerr
	}
	srv := mcpserver.New(mc)
	fmt.Fprintf(e.Stderr, "pwrwrdn mcp: session %s, %s\n", session, cfg.URL)
	// stdin EOF ends Run; so does ctx (SIGTERM). Serve then releases claims.
	t := &mcp.IOTransport{Reader: e.Stdin, Writer: e.Stdout}
	if err := mcpserver.Serve(ctx, srv, t); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
