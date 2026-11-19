package ui

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// minProxySecret is the shortest accepted secret, the same floor as a token in
// tokens.yaml.
const minProxySecret = 16

// proxyNamePattern is the service's name rule: the same pattern as a token
// name (internal/httpapi/tokens.go, tokenNamePattern). It is duplicated here
// rather than imported so the UI does not depend on the API package.
var proxyNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

// ProxyAuth is ui.auth: proxy. A reverse proxy authenticates the user and
// forwards the user name in UserHeader and a shared secret in SecretHeader.
// Only SHA-256 hashes of the secrets are kept in memory.
type ProxyAuth struct {
	SecretHeader, UserHeader string
	hashes                   [][sha256.Size]byte
}

// LoadProxyAuth reads the secret file at path. It fails when any group or
// other permission bit is set (perm & 0o077, the same rule as the token
// loaders), when no secret is present or when a line is shorter than
// 16 characters. Errors name the path or the line number, never a secret.
func LoadProxyAuth(path, secretHeader, userHeader string) (*ProxyAuth, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ui proxy secret file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("ui proxy secret file %s: mode %04o gives group or others access; chmod 600 it", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ui proxy secret file: %w", err)
	}
	p := &ProxyAuth{SecretHeader: secretHeader, UserHeader: userHeader}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < minProxySecret {
			return nil, fmt.Errorf("ui proxy secret file %s: line %d is shorter than %d characters", path, n, minProxySecret)
		}
		p.hashes = append(p.hashes, sha256.Sum256([]byte(line)))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ui proxy secret file %s: unreadable lines", path)
	}
	if len(p.hashes) == 0 {
		return nil, fmt.Errorf("ui proxy secret file %s: no secrets", path)
	}
	return p, nil
}

// secretOK reports whether v equals any stored secret. It hashes v and
// compares against every stored hash in constant time, with no early exit.
func (p *ProxyAuth) secretOK(v string) bool {
	if v == "" {
		return false
	}
	h := sha256.Sum256([]byte(v))
	ok := 0
	for i := range p.hashes {
		ok |= subtle.ConstantTimeCompare(h[:], p.hashes[i][:])
	}
	return ok == 1
}

// Identify returns the user a proxied request is for. Each header must carry
// exactly one value (a proxy that appends instead of replacing would let a
// client's own copy ride along), the secret must match a line of the secret
// file and the user must pass the service's name rule.
func (p *ProxyAuth) Identify(r *http.Request) (string, bool) {
	secrets, users := r.Header.Values(p.SecretHeader), r.Header.Values(p.UserHeader)
	good := p.secretOK(first(secrets))
	if len(secrets) != 1 || len(users) != 1 || !good || !proxyNamePattern.MatchString(users[0]) {
		return "", false
	}
	return users[0], true
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
