package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/config"
)

// TestServeNeverEchoesAToken: serve's build fails on a malformed tokens file
// with an error (printed to stderr by main) that holds no token text, and
// logs nothing that does either.
func TestServeNeverEchoesAToken(t *testing.T) {
	const tok = "FAKE-TOKEN-LEAK-0123456789"
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.TokensFile = write(t, dir, "tokens.yaml", tok+"\n")
	cfg.Powermaster.URL = "http://127.0.0.1:1"
	cfg.Powermaster.TokenFile = write(t, dir, "powermaster.token", "pm-token\n")
	var logged bytes.Buffer
	d, err := build(cfg, slog.New(slog.NewTextHandler(&logged, nil)), nil)
	if err == nil {
		d.Close()
		t.Fatal("malformed tokens file accepted")
	}
	if strings.Contains(err.Error(), "FAKE-TO") || strings.Contains(logged.String(), "FAKE-TO") {
		t.Fatalf("token echoed: err %v; log %q", err, logged.String())
	}
}
