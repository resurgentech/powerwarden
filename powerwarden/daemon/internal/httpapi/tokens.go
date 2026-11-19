package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var tokenNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

// Roles. A role comes only from the tokens.yaml entry, never from the name.
const (
	RoleFull  = "full"
	RoleAgent = "agent"
)

// TokenEntry is one tokens.yaml entry. An empty Role means full.
type TokenEntry struct {
	Token string
	Role  string
}

// UnmarshalYAML accepts a string (role full) or a map {token, role}; unknown
// keys are an error.
func (e *TokenEntry) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*e = TokenEntry{Token: s}
		return nil
	case yaml.MappingNode:
		var m struct {
			Token string `yaml:"token"`
			Role  string `yaml:"role"`
		}
		// Node.Decode does not inherit KnownFields, so check keys by hand.
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i].Value; k != "token" && k != "role" {
				return fmt.Errorf("line %d: unknown key %q (want token, role)", n.Content[i].Line, k)
			}
		}
		if err := n.Decode(&m); err != nil {
			return err
		}
		*e = TokenEntry{Token: m.Token, Role: m.Role}
		return nil
	}
	return fmt.Errorf("line %d: entry must be a token string or a map {token, role}", n.Line)
}

type tokenInfo struct{ name, role string }

// Tokens maps a bearer token to its name and role (hourchd, each operator,
// each user machine).
type Tokens struct {
	byHash map[[32]byte]tokenInfo
	roles  map[string]string
}

// LoadTokens reads tokens.yaml: a mapping of name -> token string (role full)
// or name -> {token, role: full|agent}.
func LoadTokens(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokens: %w", err)
	}
	var m map[string]TokenEntry
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("tokens: %s: %w", path, err)
	}
	return NewTokenEntries(m)
}

// NewTokens builds the set from name -> token, every token role full.
func NewTokens(m map[string]string) (*Tokens, error) {
	e := make(map[string]TokenEntry, len(m))
	for name, tok := range m {
		e[name] = TokenEntry{Token: tok}
	}
	return NewTokenEntries(e)
}

// NewTokenEntries builds the set from name -> entry.
func NewTokenEntries(m map[string]TokenEntry) (*Tokens, error) {
	t := &Tokens{byHash: map[[32]byte]tokenInfo{}, roles: map[string]string{}}
	for name, e := range m {
		if !tokenNamePattern.MatchString(name) {
			return nil, fmt.Errorf("tokens: bad name %q", name)
		}
		role := e.Role
		switch role {
		case "":
			role = RoleFull
		case RoleFull, RoleAgent:
		default:
			return nil, fmt.Errorf("tokens: %q: unknown role %q (want %s or %s)", name, role, RoleFull, RoleAgent)
		}
		if len(e.Token) < 16 {
			return nil, fmt.Errorf("tokens: token for %q shorter than 16 characters", name)
		}
		h := sha256.Sum256([]byte(e.Token))
		if _, dup := t.byHash[h]; dup {
			return nil, fmt.Errorf("tokens: duplicate token for %q", name)
		}
		t.byHash[h] = tokenInfo{name, role}
		t.roles[name] = role
	}
	if len(t.byHash) == 0 {
		return nil, fmt.Errorf("tokens: no tokens configured")
	}
	return t, nil
}

// Name returns the token's name, comparing in constant time against every entry.
func (t *Tokens) Name(token string) (string, bool) {
	h := sha256.Sum256([]byte(token))
	found := ""
	for k, in := range t.byHash {
		if subtle.ConstantTimeCompare(k[:], h[:]) == 1 {
			found = in.name
		}
	}
	return found, found != ""
}

// Role returns the role of the named token ("" if there is no such token).
func (t *Tokens) Role(name string) string { return t.roles[name] }

// routeRoles is the one route-to-role table: the least role that may call each
// route pattern. Register panics on a route missing here, and a test walks every
// spec operation against it. PWC8a adds its handler for the power row below.
var routeRoles = map[string]string{
	"GET /v1/demands":                  RoleAgent,
	"GET /v1/demands/{id}":             RoleAgent,
	"PUT /v1/demands/{id}":             RoleFull,
	"DELETE /v1/demands/{id}":          RoleFull,
	"GET /v1/workers":                  RoleAgent,
	"PUT /v1/workers/{worker_id}":      RoleFull,
	"DELETE /v1/workers/{worker_id}":   RoleFull,
	"GET /v1/claims":                   RoleAgent,
	"GET /v1/claims/{id}":              RoleAgent,
	"PUT /v1/claims/{id}":              RoleAgent,
	"DELETE /v1/claims/{id}":           RoleAgent,
	"POST /v1/claims/{id}/extend":      RoleAgent,
	"POST /v1/claims/{id}/power":       RoleAgent, // PWC8a
	"GET /v1/hosts":                    RoleAgent,
	"GET /v1/hosts/{hostname}":         RoleAgent,
	"PUT /v1/hosts/{hostname}/exclude": RoleFull,
	"PUT /v1/hosts/{hostname}/tags":    RoleFull,
	"GET /v1/decisions":                RoleAgent,
	"GET /v1/events":                   RoleAgent,
	"GET /v1/stats":                    RoleAgent,
}

// RouteRole returns the least role that may call a route pattern.
func RouteRole(pattern string) (string, bool) {
	r, ok := routeRoles[pattern]
	return r, ok
}

// roleAllows reports whether a token of role may call a route needing need.
func roleAllows(role, need string) bool {
	return role == RoleFull || (role == RoleAgent && need == RoleAgent)
}
