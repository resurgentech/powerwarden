package main

// B5: serve's key list start path and check's key list suffixes. serve itself
// needs Vault, so the tests call the small functions it is built from
// (buildKeys, startKeys) and check's keyListSuffix.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/resurgentech/keyclient/keyclienttest"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/httpapi"
)

const insecureHint = "; set personal_keys.allow_insecure_http: true only for a network path you trust"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func tokensFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.yaml")
	if err := os.WriteFile(p, []byte("powerwarden: \"aaaaaaaaaaaaaaaa-1\"\noperator: \"bbbbbbbbbbbbbbbb-2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pkCfg(t *testing.T, y string) powermaster.Config {
	t.Helper()
	cfg, err := powermaster.ParseConfig([]byte("personal_keys:\n" + y))
	if err != nil {
		t.Fatal(err)
	}
	cfg.TokensFile = tokensFile(t)
	return cfg
}

// allow_insecure_http: false refuses a non-loopback http url at start; true allows it.
func TestSR8_AllowInsecureHTTPAtBuild(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "keys.cache")
	cfg := pkCfg(t, "  url: http://keys.example/v1/lists/x\n  cache_file: "+cache+"\n")
	tokens, err := httpapi.LoadTokens(cfg.TokensFile)
	if err != nil {
		t.Fatal(err)
	}
	src, err := buildKeys(cfg, tokens, quiet())
	if err == nil || src != nil || !strings.HasPrefix(err.Error(), "personal_keys: ") || !strings.HasSuffix(err.Error(), insecureHint) {
		t.Fatalf("refused: %v %v", src, err)
	}
	cfg.PersonalKeys.AllowInsecureHTTP = true
	if src, err = buildKeys(cfg, tokens, quiet()); err != nil || src == nil {
		t.Fatalf("allowed: %v", err)
	}
	// url unset: nothing built.
	if src, err = buildKeys(powermaster.DefaultConfig(), tokens, quiet()); err != nil || src != nil {
		t.Fatalf("url unset: %v %v", src, err)
	}
}

// The SR-6 log line: one Info record, the whole text is the message, no
// attributes, taken right after Start.
func TestSR6_LogLineAfterStart(t *testing.T) {
	list := keyclienttest.NewList(t)
	list.Add("pulled-a", "full", "pulled-a-key-0123456789abc")
	list.Block() // hold the first pull: the line is taken right after Start, before any list is live
	cfg := pkCfg(t, "  url: "+list.URL()+"\n  cache_file: "+filepath.Join(t.TempDir(), "keys.cache")+"\n")
	tokens, err := httpapi.LoadTokens(cfg.TokensFile)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	src, err := buildKeys(cfg, tokens, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := startKeys(ctx, src, log); err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil {
			if m, _ := rec["msg"].(string); strings.HasPrefix(m, "personal_keys:") {
				found = append(found, rec)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d personal_keys lines: %s", len(found), buf.String())
	}
	rec := found[0]
	if len(rec) != 3 || rec["level"] != "INFO" { // time, level, msg and nothing else
		t.Fatalf("record has attributes or wrong level: %v", rec)
	}
	if !regexp.MustCompile(`^personal_keys: list origin=none entries=0 age=0s$`).MatchString(rec["msg"].(string)) {
		t.Fatalf("msg %q", rec["msg"])
	}
}

// No url: start logs nothing and does nothing.
func TestSR6_NoURLStartsNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := startKeys(context.Background(), nil, slog.New(slog.NewJSONHandler(&buf, nil))); err != nil || buf.Len() != 0 {
		t.Fatalf("%v %q", err, buf.String())
	}
}

func TestSR3_CheckKeyListSuffixes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "keys.cache")
	base := "  url: https://keys.example/v1/lists/x\n  cache_file: " + cache + "\n"
	suffix := func(y string) (string, error) { return keyListSuffix(pkCfg(t, y)) }

	if s, err := suffix(base); err != nil || s != " key_list=on" {
		t.Fatalf("%q %v", s, err)
	}
	// Nothing was sent or created: check does not Start.
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("check wrote the cache file")
	}
	// Plain http to loopback: allowed with no flag; the insecure suffix needs the flag.
	if s, err := suffix("  url: http://127.0.0.1:9/x\n  cache_file: " + cache + "\n"); err != nil || s != " key_list=on" {
		t.Fatalf("loopback: %q %v", s, err)
	}
	if s, err := suffix("  url: http://keys.example/x\n  cache_file: " + cache + "\n  allow_insecure_http: true\n"); err != nil || s != " key_list=on key_list_insecure_http=on" {
		t.Fatalf("insecure: %q %v", s, err)
	}
	// Flag on, https url: no insecure suffix.
	if s, err := suffix(base + "  allow_insecure_http: true\n"); err != nil || s != " key_list=on" {
		t.Fatalf("https with flag: %q %v", s, err)
	}
	// url unset: nothing, and the tokens file is not even read.
	cfg := powermaster.DefaultConfig()
	cfg.TokensFile = filepath.Join(t.TempDir(), "absent.yaml")
	if s, err := keyListSuffix(cfg); err != nil || s != "" {
		t.Fatalf("url unset: %q %v", s, err)
	}
	// Order with key_request: the existing suffix comes first.
	cfg = pkCfg(t, "  request_url: https://keys.example/r\n"+base)
	s, err := keyListSuffix(cfg)
	if err != nil || cfg.CheckSuffix()+s != " key_request=on key_list=on" {
		t.Fatalf("%q %q %v", cfg.CheckSuffix(), s, err)
	}
}

func TestSR3_CheckKeyListRefusals(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "keys.cache")
	_, err := keyListSuffix(pkCfg(t, "  url: http://keys.example/x\n  cache_file: "+cache+"\n"))
	if err == nil || !strings.HasPrefix(err.Error(), "personal_keys: ") || !strings.HasSuffix(err.Error(), insecureHint) {
		t.Fatalf("http: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.token")
	_, err = keyListSuffix(pkCfg(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n  token_file: "+missing+"\n"))
	if err == nil || !strings.HasPrefix(err.Error(), "personal_keys: keylist: ") || strings.Contains(err.Error(), "allow_insecure_http") {
		t.Fatalf("token_file: %v", err)
	}
	// The tokens file is loaded with serve's loader: a bad one fails the check.
	cfg := pkCfg(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n")
	cfg.TokensFile = filepath.Join(t.TempDir(), "absent.yaml")
	if _, err = keyListSuffix(cfg); err == nil {
		t.Fatal("missing tokens file accepted with url set")
	}
	// The pull token is never printed.
	secretFile := filepath.Join(t.TempDir(), "pull.token")
	if err := os.WriteFile(secretFile, []byte("PULL-TOKEN-SECRET-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := keyListSuffix(pkCfg(t, "  url: https://keys.example/x\n  cache_file: "+cache+"\n  token_file: "+secretFile+"\n"))
	if err != nil || strings.Contains(s, "PULL-TOKEN-SECRET-VALUE") {
		t.Fatalf("%q %v", s, err)
	}
}
