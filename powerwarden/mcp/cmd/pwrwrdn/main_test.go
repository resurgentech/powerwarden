package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

func testEnv(t *testing.T, args ...string) (env, *bytes.Buffer) {
	t.Helper()
	var errb bytes.Buffer
	return env{Args: args, Getenv: func(string) string { return "" }, Home: t.TempDir(),
		Stdin: io.NopCloser(strings.NewReader("")), Stdout: nopWC{io.Discard}, Stderr: &errb}, &errb
}

func TestUnknownSubcommand(t *testing.T) {
	e, errb := testEnv(t, "frobnicate")
	if rc := dispatch(context.Background(), e); rc != 2 || !strings.Contains(errb.String(), "unknown subcommand") {
		t.Fatalf("rc=%d %s", rc, errb)
	}
}

// The MCP server is gone from pwrwrdn: `mcp` is an unknown subcommand like any
// other, with or without flags, and nothing is served on stdio.
func TestMCPIsUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{{"mcp"}, {"mcp", "--url", "https://pw.example.com"}, {"mcp", "--help"}} {
		e, errb := testEnv(t, args...)
		dir := filepath.Join(e.Home, ".config", "powerwarden")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token"), []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if rc := dispatch(context.Background(), e); rc != 2 || !strings.Contains(errb.String(), `unknown subcommand "mcp"`) {
			t.Errorf("%v: rc=%d %s", args, rc, errb)
		}
	}
}
