package ui

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Htpasswd holds bcrypt entries from an htpasswd file (htpasswd -B), the
// standard format for HTTP basic auth (Apache, nginx and most proxies read it).
type Htpasswd struct {
	users map[string][]byte
}

// LoadHtpasswd reads path. Only bcrypt ($2a$, $2b$, $2y$) hashes are accepted.
func LoadHtpasswd(path string) (*Htpasswd, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("htpasswd: %w", err)
	}
	return ParseHtpasswd(b)
}

// ParseHtpasswd parses htpasswd content.
func ParseHtpasswd(b []byte) (*Htpasswd, error) {
	h := &Htpasswd{users: map[string][]byte{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		if !ok || user == "" {
			return nil, fmt.Errorf("htpasswd: line %d: want user:hash", n)
		}
		if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") && !strings.HasPrefix(hash, "$2y$") {
			return nil, fmt.Errorf("htpasswd: line %d: user %q: only bcrypt hashes are supported (htpasswd -B)", n, user)
		}
		h.users[user] = []byte(hash)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(h.users) == 0 {
		return nil, fmt.Errorf("htpasswd: no users")
	}
	return h, nil
}

// dummy keeps unknown-user checks as slow as known-user ones.
var dummy, _ = bcrypt.GenerateFromPassword([]byte("powerwarden-dummy"), bcrypt.DefaultCost)

// Check verifies a user and password.
func (h *Htpasswd) Check(user, pass string) bool {
	hash, ok := h.users[user]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummy, []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(pass)) == nil
}

// sameString compares in constant time (used for origin checks).
func sameString(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
