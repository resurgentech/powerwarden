package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTokens(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runTokens(t *testing.T, body string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{"check-config", "--config", "../../config.yaml.example", "--tokens-file", writeTokens(t, body)}, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckConfigTokensBothForms(t *testing.T) {
	code, out, errs := runTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-full-plain\"\n"+
		"op: {token: \"bbbbbbbbbbbbbbbb-op\", role: full}\n"+
		"jared@laptop: {token: \"cccccccccccccccc-agent\", role: agent}\n"+
		"jared@desk: {token: \"dddddddddddddddd-agent\", role: agent}\n")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	if !strings.Contains(out, "tokens=4 (full=2, agent=2)") || !strings.HasPrefix(out, "ok: listen=") || !strings.Contains(out, "powermaster=") {
		t.Fatalf("output %q", out)
	}
	for _, v := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd"} {
		if strings.Contains(out, v) {
			t.Fatalf("token value in output %q", out)
		}
	}
}

func TestCheckConfigTokensBad(t *testing.T) {
	cases := map[string]string{
		"unknown role": "x: {token: \"SECRETSECRETSECRET1\", role: root}\n",
		"short token":  "x: \"SHORTSECRET\"\n",
		"scalar file":  "SECRETSECRETSECRET2\n",
		"unknown key":  "x: {token: \"SECRETSECRETSECRET3\", rol: full}\n",
		"empty":        "{}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errs := runTokens(t, body)
			if code != 1 {
				t.Fatalf("code %d, out %q err %q", code, out, errs)
			}
			if errs == "" || strings.Contains(out, "ok:") {
				t.Fatalf("out %q err %q", out, errs)
			}
			if strings.Contains(errs, "SECRET") || strings.Contains(out, "SECRET") {
				t.Fatalf("token value leaked: %q", errs)
			}
		})
	}
}

func TestCheckConfigTokensMissing(t *testing.T) {
	var out, errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := run([]string{"check-config", "--config", "../../config.yaml.example", "--tokens-file", missing}, &out, &errb); code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errb.String(), "tokens") {
		t.Fatalf("err %q", errb.String())
	}
}

func TestCheckConfigTokensFileFromConfigAndOverride(t *testing.T) {
	dir := t.TempDir()
	good := writeTokens(t, "a: \"aaaaaaaaaaaaaaaa-1\"\n")
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("powermaster:\n  url: http://pm.example:8092\ntokens_file: "+filepath.Join(dir, "absent.yaml")+"\n"), 0o600)
	var out, errb bytes.Buffer
	// tokens_file from the config is missing: fail.
	if code := run([]string{"check-config", "--config", cfg}, &out, &errb); code != 1 {
		t.Fatalf("config tokens_file not loaded (%d): %s", code, out.String())
	}
	// --tokens-file overrides it.
	out.Reset()
	errb.Reset()
	if code := run([]string{"check-config", "--config", cfg, "--tokens-file", good}, &out, &errb); code != 0 {
		t.Fatalf("override failed (%d): %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tokens=1 (full=1, agent=0)") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCheckConfig(t *testing.T) {
	var out, errb bytes.Buffer
	tok := writeTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-1\"\n")
	if code := run([]string{"check-config", "--config", "../../config.yaml.example", "--tokens-file", tok}, &out, &errb); code != 0 {
		t.Fatalf("example rejected (%d): %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "ok: listen=") {
		t.Fatalf("output %q", out.String())
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("nonsense_key: 1\n"), 0o600)
	if code := run([]string{"check-config", "--config", bad, "--tokens-file", tok}, &out, &errb); code != 1 {
		t.Fatalf("unknown key accepted (%d)", code)
	}
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatal("no args should be usage")
	}
}

func proxyConfigFile(t *testing.T, secretMode os.FileMode, secretBody string) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "ui-proxy.secret")
	if err := os.WriteFile(secret, []byte(secretBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, secretMode); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	y := "powermaster:\n  url: http://pm.example:8092\nui:\n  auth: proxy\n  htpasswd_file: " + filepath.Join(dir, "absent.htpasswd") +
		"\n  proxy:\n    secret_file: " + secret + "\n    secret_header: X-Example-Proxy-Secret\n    user_header: X-Example-User\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCheckConfigProxyMode(t *testing.T) {
	const secret = "0123456789abcdef-test-secret"
	tok := writeTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-1\"\n")
	var out, errb bytes.Buffer
	if code := run([]string{"check-config", "--config", proxyConfigFile(t, 0o600, secret+"\n"), "--tokens-file", tok}, &out, &errb); code != 0 {
		t.Fatalf("proxy config rejected (%d): %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "ok: listen=") || !strings.Contains(out.String(), "tokens=1 (full=1, agent=0) ui=proxy") {
		t.Fatalf("output %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), secret) {
		t.Fatal("secret in output")
	}

	for name, c := range map[string]struct {
		mode os.FileMode
		body string
	}{
		"readable":    {0o644, secret + "\n"},
		"group write": {0o620, secret + "\n"},
		"empty":       {0o600, "# none\n"},
		"short line":  {0o600, "short\n"},
	} {
		out.Reset()
		errb.Reset()
		if code := run([]string{"check-config", "--config", proxyConfigFile(t, c.mode, c.body), "--tokens-file", tok}, &out, &errb); code != 1 {
			t.Errorf("%s: code %d", name, code)
		}
		if errb.Len() == 0 || strings.Contains(out.String()+errb.String(), secret) || strings.Contains(out.String(), "ok:") {
			t.Errorf("%s: out %q err %q", name, out.String(), errb.String())
		}
	}
}

func TestCheckConfigKeyRequestSuffix(t *testing.T) {
	tok := writeTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-1\"\n")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	y := "powermaster:\n  url: http://pm.example:8092\npersonal_keys:\n  request_url: https://keys.example.com/r\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"check-config", "--config", cfg, "--tokens-file", tok}, &out, &errb); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	if !strings.HasSuffix(out.String(), "tokens=1 (full=1, agent=0) key_request=on\n") {
		t.Fatalf("output %q", out.String())
	}
	// With proxy mode too: the suffix follows ui=proxy.
	out.Reset()
	pc := proxyConfigFile(t, 0o600, "0123456789abcdef-test-secret\n")
	b, _ := os.ReadFile(pc)
	if err := os.WriteFile(pc, append(b, []byte("personal_keys:\n  request_url: https://keys.example.com/r\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"check-config", "--config", pc, "--tokens-file", tok}, &out, &errb); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	if !strings.HasSuffix(out.String(), " ui=proxy key_request=on\n") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCheckConfigHtpasswdModeLineUnchanged(t *testing.T) {
	tok := writeTokens(t, "hourchd: \"aaaaaaaaaaaaaaaa-1\"\n")
	var out, errb bytes.Buffer
	if code := run([]string{"check-config", "--config", "../../config.yaml.example", "--tokens-file", tok}, &out, &errb); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "ui=") || !strings.HasSuffix(out.String(), "tokens=1 (full=1, agent=0)\n") {
		t.Fatalf("output %q", out.String())
	}
}
