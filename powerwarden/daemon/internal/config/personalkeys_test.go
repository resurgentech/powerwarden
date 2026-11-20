package config

import (
	"strings"
	"testing"
	"time"
)

const wantURLFmt = `want an absolute http(s)://host/... URL (or "" to turn the key list off)`

func TestSR2PersonalKeysDefaults(t *testing.T) {
	p := Default().PersonalKeys
	if p.URL != "" || p.TokenFile != "" || p.CacheFile != "" || p.AllowInsecureHTTP {
		t.Fatalf("defaults: %+v", p)
	}
	if p.Refresh != time.Minute || p.RefreshOnMiss != 5*time.Second {
		t.Fatalf("durations: %v %v", p.Refresh, p.RefreshOnMiss)
	}
	cfg, err := parseReq("personal_keys:\n  url: https://keys.example/v1/lists/x\n  cache_file: /var/cache/pw/keys.json\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonalKeys.Refresh != time.Minute || cfg.PersonalKeys.RefreshOnMiss != 5*time.Second {
		t.Fatalf("defaults kept: %+v", cfg.PersonalKeys)
	}
	cfg, err = parseReq("personal_keys:\n  url: https://keys.example/v1/lists/x\n  cache_file: /c\n  token_file: /t\n  refresh: 30s\n  refresh_on_miss: 0s\n  allow_insecure_http: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.PersonalKeys; p.TokenFile != "/t" || p.Refresh != 30*time.Second || p.RefreshOnMiss != 0 || !p.AllowInsecureHTTP {
		t.Fatalf("parsed: %+v", p)
	}
}

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
		_, err := parseReq("personal_keys:\n  " + y)
		want := "personal_keys." + key + " is set but personal_keys.url is empty"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", key, err, want)
		}
		// url: "" is the same as absent.
		_, err = parseReq("personal_keys:\n  url: \"\"\n  " + y)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s with url \"\": %v", key, err)
		}
	}
	// All five at once, in the SR-1 table order, and nothing else from the block.
	_, err := parseReq("personal_keys:\n  allow_insecure_http: true\n  refresh_on_miss: 1s\n  refresh: 2m\n  cache_file: /c\n  token_file: /t\n")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	last := -1
	for _, k := range []string{"token_file", "cache_file", "refresh", "refresh_on_miss", "allow_insecure_http"} {
		w := "personal_keys." + k + " is set but personal_keys.url is empty"
		i := strings.Index(msg, w)
		if i < 0 || i < last {
			t.Fatalf("%s missing or out of order in %q", k, msg)
		}
		last = i
	}
	if strings.Contains(msg, "required when") || strings.Contains(msg, "must be") {
		t.Fatalf("extra errors for an empty url: %q", msg)
	}
	// A Config built in code: a value that differs from the default is "set".
	c := Default()
	c.Powermaster.URL = "http://pm.example:8092"
	c.PersonalKeys.Refresh = 2 * time.Minute
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "personal_keys.refresh is set but personal_keys.url is empty") {
		t.Fatalf("code-built: %v", err)
	}
	c = Default()
	c.Powermaster.URL = "http://pm.example:8092"
	if err := c.Validate(); err != nil {
		t.Fatalf("code-built defaults: %v", err)
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
		"unparsable": {"http://[::1", "<redacted>"},
	}
	for name, c := range cases {
		_, err := parseReq("personal_keys:\n  cache_file: /c\n  url: \"" + c.in + "\"\n")
		want := `personal_keys.url "` + c.shown + `": ` + wantURLFmt
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v\nwant %q", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: password in the message: %v", name, err)
		}
	}
	// A valid URL is not reported; scheme case does not matter.
	for _, u := range []string{"https://keys.example/v1/lists/x", "HTTPS://Keys.Example/x", "http://127.0.0.1:8080/x"} {
		if _, err := parseReq("personal_keys:\n  cache_file: /c\n  url: " + u + "\n"); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestSR2PersonalKeysRequiredAndDurations(t *testing.T) {
	const u = "personal_keys:\n  url: https://keys.example/x\n"
	_, err := parseReq(u)
	if err == nil || !strings.Contains(err.Error(), "personal_keys.cache_file: required when personal_keys.url is set") {
		t.Fatalf("no cache_file: %v", err)
	}
	_, err = parseReq(u + "  cache_file: \"\"\n")
	if err == nil || !strings.Contains(err.Error(), "personal_keys.cache_file: required when personal_keys.url is set") {
		t.Fatalf("empty cache_file: %v", err)
	}
	for _, v := range []string{"0s", "-1s"} {
		_, err = parseReq(u + "  cache_file: /c\n  refresh: " + v + "\n")
		if err == nil || !strings.Contains(err.Error(), "personal_keys.refresh: must be greater than zero") {
			t.Errorf("refresh %s: %v", v, err)
		}
	}
	_, err = parseReq(u + "  cache_file: /c\n  refresh_on_miss: -1s\n")
	if err == nil || !strings.Contains(err.Error(), "personal_keys.refresh_on_miss: must not be negative") {
		t.Fatalf("refresh_on_miss: %v", err)
	}
	// Zero is allowed for refresh_on_miss (off).
	if _, err = parseReq(u + "  cache_file: /c\n  refresh_on_miss: 0s\n"); err != nil {
		t.Fatalf("refresh_on_miss 0: %v", err)
	}
	// With a malformed url the url-set checks still run (D2c), in the D2 order,
	// and every error is collected under one "config: " prefix.
	_, err = parseReq("personal_keys:\n  url: ftp://x/y\n  refresh: 0s\n  refresh_on_miss: -1s\n")
	if err == nil || !strings.HasPrefix(err.Error(), "config: ") {
		t.Fatalf("collect: %v", err)
	}
	msg := err.Error()
	a := strings.Index(msg, "personal_keys.url ")
	b := strings.Index(msg, "personal_keys.cache_file: required")
	c := strings.Index(msg, "personal_keys.refresh: must be greater than zero")
	d := strings.Index(msg, "personal_keys.refresh_on_miss: must not be negative")
	if a < 0 || b < a || c < b || d < c {
		t.Fatalf("order: %q", msg)
	}
}

func TestSR2PersonalKeysUnknownKeyStillRefused(t *testing.T) {
	_, err := parseReq("personal_keys:\n  request_url: https://keys.example/r\n  bogus: 1\n")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown key under personal_keys: %v", err)
	}
	// request_url still validated first and alone.
	_, err = parseReq("personal_keys:\n  request_url: ftp://x\n")
	if err == nil || !strings.Contains(err.Error(), "personal_keys.request_url") {
		t.Fatalf("request_url: %v", err)
	}
}
