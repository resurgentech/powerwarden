package powermaster

import (
	"strings"
	"testing"
	"time"
)

const wantURLFmt = `want an absolute http(s)://host/... URL (or "" to turn the key list off)`

func TestSR2PersonalKeysDefaults(t *testing.T) {
	p := DefaultConfig().PersonalKeys
	if p.URL != "" || p.TokenFile != "" || p.CacheFile != "" || p.AllowInsecureHTTP {
		t.Fatalf("defaults: %+v", p)
	}
	if p.Refresh.D() != time.Minute || p.RefreshOnMiss.D() != 5*time.Second {
		t.Fatalf("durations: %v %v", p.Refresh, p.RefreshOnMiss)
	}
	cfg, err := ParseConfig([]byte("personal_keys:\n  url: https://keys.example/v1/lists/x\n  cache_file: /var/cache/pm/keys.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonalKeys.Refresh.D() != time.Minute || cfg.PersonalKeys.RefreshOnMiss.D() != 5*time.Second {
		t.Fatalf("defaults kept: %+v", cfg.PersonalKeys)
	}
	cfg, err = ParseConfig([]byte("personal_keys:\n  url: https://keys.example/v1/lists/x\n  cache_file: /c\n  token_file: /t\n  refresh: 30s\n  refresh_on_miss: 0s\n  allow_insecure_http: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.PersonalKeys; p.TokenFile != "/t" || p.Refresh.D() != 30*time.Second || p.RefreshOnMiss != 0 || !p.AllowInsecureHTTP {
		t.Fatalf("parsed: %+v", p)
	}
}

func parsePK(y string) (Config, error) { return ParseConfig([]byte("personal_keys:\n" + y)) }

func TestSR2PersonalKeysSubKeyWithoutURL(t *testing.T) {
	// Presence-based: any value, including the default and the zero value, is "set".
	cases := map[string]string{
		"token_file":          "token_file: \"\"\n",
		"cache_file":          "cache_file: /c\n",
		"refresh":             "refresh: 1m\n",
		"refresh_on_miss":     "refresh_on_miss: 0s\n",
		"allow_insecure_http": "allow_insecure_http: false\n",
	}
	for key, y := range cases {
		_, err := parsePK("  " + y)
		want := "personal_keys." + key + " is set but personal_keys.url is empty"
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v (want %q)", key, err, want)
		}
		// url: "" is the same as absent.
		_, err = parsePK("  url: \"\"\n  " + y)
		if err == nil || err.Error() != want {
			t.Errorf("%s with url \"\": %v", key, err)
		}
	}
	// Several at once: the first in the SR-1 table order is the one returned.
	_, err := parsePK("  allow_insecure_http: true\n  refresh_on_miss: 1s\n  refresh: 2m\n  cache_file: /c\n  token_file: /t\n")
	if err == nil || err.Error() != "personal_keys.token_file is set but personal_keys.url is empty" {
		t.Fatalf("first error: %v", err)
	}
	_, err = parsePK("  allow_insecure_http: true\n  refresh_on_miss: 1s\n  refresh: 2m\n")
	if err == nil || err.Error() != "personal_keys.refresh is set but personal_keys.url is empty" {
		t.Fatalf("first error: %v", err)
	}
	// A Config built in code: a value that differs from the default is "set"; a zero
	// duration is "unset".
	c := DefaultConfig()
	c.PersonalKeys.Refresh = Duration(2 * time.Minute)
	if err := c.Validate(); err == nil || err.Error() != "personal_keys.refresh is set but personal_keys.url is empty" {
		t.Fatalf("code-built: %v", err)
	}
	c = DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("code-built defaults: %v", err)
	}
	c = DefaultConfig()
	c.PersonalKeys.Refresh, c.PersonalKeys.RefreshOnMiss = 0, 0
	if err := c.Validate(); err != nil {
		t.Fatalf("code-built zero durations with no url: %v", err)
	}
}

func TestSR2PersonalKeysURLMalformed(t *testing.T) {
	cases := map[string]struct{ in, shown string }{
		"relative":   {"/v1/lists/x", "/v1/lists/x"},
		"ftp":        {"ftp://keys.example/x", "ftp://keys.example/x"},
		"no host":    {"https:///x", "https:///x"},
		"userinfo":   {"https://u:secretpw@keys.example/x", "https://u:xxxxx@keys.example/x"},
		"user only":  {"https://tokenuser@keys.example/x", "https://tokenuser@keys.example/x"},
		"fragment":   {"https://keys.example/x#f", "https://keys.example/x#f"},
		"empty frag": {"https://keys.example/x#", "https://keys.example/x"},
		"unparsable": {"http://[::1", "xxxxx"},
	}
	for name, c := range cases {
		_, err := parsePK("  cache_file: /c\n  url: \"" + c.in + "\"\n")
		want := `personal_keys.url "` + c.shown + `": ` + wantURLFmt
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %q", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: password in the message: %v", name, err)
		}
	}
	for _, u := range []string{"https://keys.example/v1/lists/x", "HTTPS://Keys.Example/x", "http://127.0.0.1:8080/x"} {
		if _, err := parsePK("  cache_file: /c\n  url: " + u + "\n"); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestSR2PersonalKeysRequiredAndDurations(t *testing.T) {
	const u = "  url: https://keys.example/x\n"
	_, err := parsePK(u)
	if err == nil || err.Error() != "personal_keys.cache_file: required when personal_keys.url is set" {
		t.Fatalf("no cache_file: %v", err)
	}
	_, err = parsePK(u + "  cache_file: \"\"\n")
	if err == nil || err.Error() != "personal_keys.cache_file: required when personal_keys.url is set" {
		t.Fatalf("empty cache_file: %v", err)
	}
	for _, v := range []string{"0s", "-1s"} {
		_, err = parsePK(u + "  cache_file: /c\n  refresh: " + v + "\n")
		if err == nil || err.Error() != "personal_keys.refresh: must be greater than zero" {
			t.Errorf("refresh %s: %v", v, err)
		}
	}
	_, err = parsePK(u + "  cache_file: /c\n  refresh_on_miss: -1s\n")
	if err == nil || err.Error() != "personal_keys.refresh_on_miss: must not be negative" {
		t.Fatalf("refresh_on_miss: %v", err)
	}
	if _, err = parsePK(u + "  cache_file: /c\n  refresh_on_miss: 0s\n"); err != nil {
		t.Fatalf("refresh_on_miss 0: %v", err)
	}
	// First error, in the D2 order: malformed url, then cache_file, refresh, refresh_on_miss.
	_, err = parsePK("  url: ftp://x/y\n  refresh: 0s\n  refresh_on_miss: -1s\n")
	if err == nil || err.Error() != `personal_keys.url "ftp://x/y": `+wantURLFmt {
		t.Fatalf("first: %v", err)
	}
	_, err = parsePK("  url: https://x/y\n  refresh: 0s\n  refresh_on_miss: -1s\n")
	if err == nil || err.Error() != "personal_keys.cache_file: required when personal_keys.url is set" {
		t.Fatalf("second: %v", err)
	}
	_, err = parsePK("  url: https://x/y\n  cache_file: /c\n  refresh: 0s\n  refresh_on_miss: -1s\n")
	if err == nil || err.Error() != "personal_keys.refresh: must be greater than zero" {
		t.Fatalf("third: %v", err)
	}
}

func TestSR2PersonalKeysUnknownKeyStillRefused(t *testing.T) {
	_, err := parsePK("  request_url: https://keys.example/r\n  bogus: 1\n")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown key under personal_keys: %v", err)
	}
	// request_url is still validated first.
	_, err = parsePK("  request_url: ftp://x\n  url: ftp://y\n")
	if err == nil || !strings.Contains(err.Error(), "personal_keys.request_url") {
		t.Fatalf("request_url: %v", err)
	}
	// LoadConfig prefixes the path as today.
	p := writeConfig(t, "personal_keys:\n  cache_file: /c\n")
	if _, err = LoadConfig(p); err == nil || !strings.HasSuffix(err.Error(), ": personal_keys.cache_file is set but personal_keys.url is empty") || !strings.HasPrefix(err.Error(), p+": ") {
		t.Fatalf("LoadConfig: %v", err)
	}
}
