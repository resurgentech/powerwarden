package client_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
)

func write(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadToken(t *testing.T) {
	if tok, err := client.LoadToken(write(t, 0o600, " abc \n")); err != nil || tok != "abc" {
		t.Fatalf("%q %v", tok, err)
	}
	for _, m := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		if _, err := client.LoadToken(write(t, m, "abc")); err == nil || !strings.Contains(err.Error(), "must not be readable") {
			t.Errorf("mode %o: %v", m, err)
		}
	}
	if _, err := client.LoadToken(write(t, 0o600, "\n")); err == nil {
		t.Error("empty token accepted")
	}
	if _, err := client.LoadToken(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestTokenPathPrecedence(t *testing.T) {
	env := func(k string) string {
		if k == client.EnvTokenFile {
			return "/env/tok"
		}
		return ""
	}
	none := func(string) string { return "" }
	if got := client.TokenPath("/flag/tok", env, "/h"); got != "/flag/tok" {
		t.Error(got)
	}
	if got := client.TokenPath("", env, "/h"); got != "/env/tok" {
		t.Error(got)
	}
	if got := client.TokenPath("", none, "/h"); got != "/h/.config/powerwarden/token" {
		t.Error(got)
	}
}

func urlHome(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	if content != "" {
		dir := filepath.Join(home, ".config", "powerwarden")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "url"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestResolveConfigURLPrecedence(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"POWERWARDEN_URL": "https://env.example.com/", "POWERWARDEN_CA_FILE": "/ca"}[k]
	}
	none := func(string) string { return "" }
	home := urlHome(t, "  https://file.example.com/ \n")

	c, err := client.ResolveConfig("https://flag.example.com", env, home)
	if err != nil || c.URL != "https://flag.example.com" || c.CAFile != "/ca" {
		t.Errorf("flag: %+v %v", c, err)
	}
	if c, err = client.ResolveConfig("", env, home); err != nil || c.URL != "https://env.example.com" {
		t.Errorf("env: %+v %v", c, err)
	}
	if c, err = client.ResolveConfig("", none, home); err != nil || c.URL != "https://file.example.com" {
		t.Errorf("file: %+v %v", c, err)
	}
	_, err = client.ResolveConfig("", none, urlHome(t, ""))
	want := "powerwarden URL not set: use --url, POWERWARDEN_URL, or write it to ~/.config/powerwarden/url"
	if err == nil || err.Error() != want {
		t.Errorf("unset: %v", err)
	}
	// a blank file counts as not set
	if _, err = client.ResolveConfig("", none, urlHome(t, " \n")); err == nil || !strings.Contains(err.Error(), "URL not set") {
		t.Errorf("blank: %v", err)
	}
}

func TestIDs(t *testing.T) {
	if !regexp.MustCompile(`^s-[0-9a-f]{12}$`).MatchString(client.NewSessionID()) {
		t.Error("session id shape")
	}
	if !regexp.MustCompile(`^mcp-[0-9a-f]{12}$`).MatchString(client.NewClaimID("mcp")) {
		t.Error("claim id shape")
	}
	if !regexp.MustCompile(`^pwrwrdn-[0-9a-f]{12}$`).MatchString(client.NewClaimID("pwrwrdn")) {
		t.Error("claim id prefix param")
	}
}
