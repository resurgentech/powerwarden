package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/resurgentech/keyclient/keylist"
	"gopkg.in/yaml.v3"
)

// Tokens maps a caller name ("powerwarden", "alice") to its bearer token.
// Lookups compare against every token in constant time.
type Tokens struct {
	names  []string
	hashes [][32]byte
}

// NewTokens validates name -> token pairs: at least one, no empty values, no
// duplicate tokens.
func NewTokens(m map[string]string) (*Tokens, error) {
	if len(m) == 0 {
		return nil, errors.New("tokens: no tokens configured (fail closed)")
	}
	t := &Tokens{}
	seen := map[[32]byte]string{}
	for name, tok := range m {
		if strings.TrimSpace(name) == "" || tok == "" {
			return nil, fmt.Errorf("tokens: empty name or token for %q", name)
		}
		h := sha256.Sum256([]byte(tok))
		if prev, ok := seen[h]; ok {
			return nil, fmt.Errorf("tokens: %s and %s share a token", prev, name)
		}
		seen[h] = name
		t.names = append(t.names, name)
		t.hashes = append(t.hashes, h)
	}
	return t, nil
}

// LoadTokens reads the tokens file (YAML name: token). The file must not be
// readable by group or other.
func LoadTokens(path string) (*Tokens, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("tokens file %s has mode %v; want 0600", path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("tokens file %s: %v", path, err)
	}
	return NewTokens(m)
}

// Lookup returns the caller name for a presented token. Every configured
// token is compared, in constant time, whatever matches.
func (t *Tokens) Lookup(presented string) (string, bool) {
	h := sha256.Sum256([]byte(presented))
	match := -1
	for i := range t.hashes {
		if subtle.ConstantTimeCompare(h[:], t.hashes[i][:]) == 1 {
			match = i
		}
	}
	if match < 0 || presented == "" {
		return "", false
	}
	return t.names[match], true
}

// PulledRoles are the roles a pulled key list entry may carry (SR-4): the
// roles this daemon knows. Every powermaster token is full; there are no others.
var PulledRoles = []string{"full"}

// TakenLocally reports whether a pulled entry clashes with a loaded token: its
// name is the name of any token, or its SHA-256 (lowercase hex; a value that
// does not decode is not taken) equals the hash of any token. It reads only the
// slices built at load time, so it answers the same for the life of the process
// (keylist.Options.Accept requires that).
func (t *Tokens) TakenLocally(e keylist.Entry) bool {
	for _, n := range t.names {
		if n == e.Name {
			return true
		}
	}
	b, err := hex.DecodeString(e.SHA256)
	if err != nil || len(b) != sha256.Size {
		return false
	}
	var h [sha256.Size]byte
	copy(h[:], b)
	for _, th := range t.hashes {
		if th == h {
			return true
		}
	}
	return false
}
