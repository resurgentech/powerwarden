package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeToken is a recognisable token value that must never be echoed.
const fakeToken = "FAKE-TOKEN-LEAK-0123456789"

// TestLoadTokensNeverEchoesAToken: a malformed tokens file whose error would
// quote a scalar (yaml.v3 puts it in backticks) yields an error without the
// token text. serve and check both use this loader.
func TestLoadTokensNeverEchoesAToken(t *testing.T) {
	for name, body := range map[string]string{
		"bare token, no name": fakeToken + "\n",
		"tagged value":        "powerwarden: !!int " + fakeToken + "\n",
		"list instead of map": "- " + fakeToken + "\n",
		"syntax error":        "powerwarden: " + fakeToken + "\noperator: [" + fakeToken + "\n",
	} {
		p := filepath.Join(t.TempDir(), "tokens.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadTokens(p)
		if err == nil {
			t.Fatalf("%s: malformed tokens file accepted", name)
		}
		if strings.Contains(err.Error(), "FAKE-TO") {
			t.Errorf("%s: error echoes the token: %v", name, err)
		}
		if !strings.HasPrefix(err.Error(), "tokens file "+p+": ") {
			t.Errorf("%s: error %q lost its context", name, err)
		}
	}
}
