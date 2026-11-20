package mcpserver_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/mcpserver"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/pwfake"
)

// With no API at start, every tool call goes through Connect first: an error
// becomes an MCP error result with its text; the keeper is created on the
// first success and Serve releases its claims.
func TestConnectGateErrorThenKeeperOnSuccess(t *testing.T) {
	f := pwfake.New()
	t.Cleanup(f.Srv.Close)
	api, err := client.New(client.Config{URL: f.Srv.URL}, "tok", f.Srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var calls, keepers atomic.Int32
	srv := mcpserver.New(mcpserver.Config{
		Connect: func(context.Context) (client.API, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("not yet: approve it")
			}
			return api, nil
		},
		NewKeeper: func(a client.API) *keeper.Keeper {
			keepers.Add(1)
			return keeper.New(a, keeper.Options{Prefix: "mcp", Session: sess})
		},
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mcpserver.Serve(ctx, srv, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	text := func(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_hosts"})
	if err != nil || !res.IsError || text(res) != "not yet: approve it" || len(res.Content) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if keepers.Load() != 0 {
		t.Fatal("keeper created before the first success")
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "claim_host", Arguments: map[string]any{"tags": []string{"a"}, "purpose": "p", "max_hours": 1}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "my_claims"})
	if err != nil || res.IsError || calls.Load() != 2 || keepers.Load() != 1 {
		t.Fatalf("%v %+v calls=%d keepers=%d: Connect runs until it succeeds, once", err, res, calls.Load(), keepers.Load())
	}
	if n := len(f.Claims); n != 1 {
		t.Fatalf("claims %d", n)
	}
	cancel()
	_ = cs.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if n := len(f.Claims); n != 0 {
		t.Fatalf("claims not released on exit: %d left", n)
	}
}

// A server that never got a key has no keeper: warnings and shutdown cope.
func TestServeWithoutKeeper(t *testing.T) {
	srv := mcpserver.New(mcpserver.Config{
		Connect:   func(context.Context) (client.API, error) { return nil, errors.New("no key") },
		NewKeeper: func(client.API) *keeper.Keeper { t.Fatal("keeper built"); return nil },
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mcpserver.Serve(ctx, srv, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"list_hosts", "my_claims", "release_host", "wait_for_claim"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"claim_id": "x"}})
		if err != nil || !res.IsError || len(res.Content) != 1 {
			t.Fatalf("%s: %v %+v", tool, err, res)
		}
	}
	cancel()
	_ = cs.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}
