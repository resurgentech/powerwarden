package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckNeverEchoesAToken: with personal_keys.url set, check loads the
// tokens file; a malformed one fails the check, and the error main prints
// (stderr) holds no token text. check prints nothing to stdout on failure.
func TestCheckNeverEchoesAToken(t *testing.T) {
	const tok = "FAKE-TOKEN-LEAK-0123456789"
	cache := filepath.Join(t.TempDir(), "keys.cache")
	cfg := pkCfg(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n")
	cfg.TokensFile = filepath.Join(t.TempDir(), "tokens.yaml")
	if err := os.WriteFile(cfg.TokensFile, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	suffix, err := keyListSuffix(cfg)
	if err == nil {
		t.Fatalf("malformed tokens file accepted: %q", suffix)
	}
	if strings.Contains(err.Error(), "FAKE-TO") {
		t.Fatalf("check error echoes the token: %v", err)
	}
}
