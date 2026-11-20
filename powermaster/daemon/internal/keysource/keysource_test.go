package keysource

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/keyclient/keylist"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
)

const hint = "; set personal_keys.allow_insecure_http: true only for a network path you trust"

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pk(url string) powermaster.PersonalKeys {
	p := powermaster.DefaultConfig().PersonalKeys
	p.URL = url
	p.CacheFile = "/nonexistent-dir/keys.cache"
	return p
}

func TestSR3NewOffReturnsNil(t *testing.T) {
	src, err := New(powermaster.DefaultConfig().PersonalKeys, []string{"full"}, nil, discard())
	if src != nil || err != nil {
		t.Fatalf("url empty: %v %v", src, err)
	}
	// Nothing is logged for an off list, not even by Start.
	var buf bytes.Buffer
	if err := Start(context.Background(), nil, slog.New(slog.NewTextHandler(&buf, nil))); err != nil || buf.Len() != 0 {
		t.Fatalf("start nil: %v %q", err, buf.String())
	}
}

func TestSR2NewHTTPRuleSuffix(t *testing.T) {
	for _, u := range []string{"http://keys.example/v1/lists/x", "HTTP://Keys.Example:8080/x", "http://10.1.2.3/x"} {
		_, err := New(pk(u), []string{"full"}, nil, discard())
		if err == nil || !strings.HasPrefix(err.Error(), "personal_keys: ") || !strings.HasSuffix(err.Error(), hint) {
			t.Errorf("%s: %v", u, err)
		}
	}
	p := pk("http://keys.example/v1/lists/x")
	p.AllowInsecureHTTP = true
	if src, err := New(p, []string{"full"}, nil, discard()); err != nil || src == nil {
		t.Fatalf("allowed: %v", err)
	}
	for _, u := range []string{"http://localhost:1/x", "http://127.0.0.1:1/x", "http://[::1]:1/x", "http://[::ffff:127.0.0.1]:1/x"} {
		if src, err := New(pk(u), []string{"full"}, nil, discard()); err != nil || src == nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	p = pk("https://keys.example/x")
	p.TokenFile = filepath.Join(t.TempDir(), "missing.token")
	_, err := New(p, []string{"full"}, nil, discard())
	if err == nil || !strings.HasPrefix(err.Error(), "personal_keys: ") || strings.Contains(err.Error(), hint) {
		t.Fatalf("token file: %v", err)
	}
	p = pk("http://keys.example/x")
	p.AllowInsecureHTTP = true
	p.TokenFile = filepath.Join(t.TempDir(), "missing.token")
	if _, err = New(p, []string{"full"}, nil, discard()); err == nil || strings.Contains(err.Error(), hint) {
		t.Fatalf("allowed http, bad token file: %v", err)
	}
	tf := filepath.Join(t.TempDir(), "t")
	if err := os.WriteFile(tf, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = pk("https://keys.example/x")
	p.TokenFile = tf
	if _, err = New(p, []string{"full"}, nil, discard()); err == nil {
		t.Fatal("empty token file accepted")
	}
}

func TestSR4NewUsesAcceptRoles(t *testing.T) {
	p := pk("https://keys.example/x")
	taken := func(e keylist.Entry) bool { return e.Name == "local" }
	src, err := New(p, []string{"full"}, taken, discard())
	if err != nil || src == nil {
		t.Fatal(err)
	}
}
