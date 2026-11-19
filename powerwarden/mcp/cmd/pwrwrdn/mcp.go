package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/mcpserver"
)

func runMCP(ctx context.Context, e env) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	tokenFile := fs.String("token-file", "", "token file (default $POWERWARDEN_TOKEN_FILE or ~/.config/powerwarden/token)")
	addURLFlag(fs)
	if err := fs.Parse(e.Args); err != nil {
		return err
	}
	cfg, err := client.ResolveConfig(urlFlag(fs), e.Getenv, e.Home)
	if err != nil {
		return err
	}
	tok, err := client.LoadToken(client.TokenPath(*tokenFile, e.Getenv, e.Home))
	if err != nil {
		return err
	}
	api, err := client.New(cfg, tok, nil)
	if err != nil {
		return err
	}
	session := client.NewSessionID()
	k := keeper.New(api, keeper.Options{Prefix: "mcp", Session: session})
	srv := mcpserver.New(mcpserver.Config{API: api, Keeper: k})
	fmt.Fprintf(e.Stderr, "pwrwrdn mcp: session %s, %s\n", session, cfg.URL)
	// stdin EOF ends Run; so does ctx (SIGTERM). Serve then releases claims.
	t := &mcp.IOTransport{Reader: e.Stdin, Writer: e.Stdout}
	if err := mcpserver.Serve(ctx, srv, k, t); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
