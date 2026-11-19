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

func TestMCPRefusesOpenTokenFile(t *testing.T) {
	e, errb := testEnv(t, "mcp", "--url", "https://pw.example.com")
	dir := filepath.Join(e.Home, ".config", "powerwarden")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := dispatch(context.Background(), e); rc != 1 || !strings.Contains(errb.String(), "must not be readable") {
		t.Fatalf("rc=%d %s", rc, errb)
	}
}

func TestMCPEOFExitsCleanly(t *testing.T) {
	e, errb := testEnv(t, "mcp", "--url", "https://pw.example.com")
	dir := filepath.Join(e.Home, ".config", "powerwarden")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte("secret\n"), 0o600)
	// stdin is already at EOF, no claims exist: nothing contacts any server
	if rc := dispatch(context.Background(), e); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
}
