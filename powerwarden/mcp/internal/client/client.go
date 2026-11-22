// Package client holds what every pwrwrdn subcommand shares: token loading,
// the generated powerwarden client with bearer auth and an optional CA, id
// generation and refusal decoding.
package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
)

// Version is pwrwrdn's version; as pwrwrdn/<Version> it is the Tool of a key
// request.
const Version = "0.2.0"

const (
	EnvURL           = "POWERWARDEN_URL"
	EnvCAFile        = "POWERWARDEN_CA_FILE"
	EnvTokenFile     = "POWERWARDEN_TOKEN_FILE"
	EnvKeyRequestURL = "POWERWARDEN_KEY_REQUEST_URL"
	defaultTokenPath = ".config/powerwarden/token"
	defaultURLPath   = ".config/powerwarden/url"
)

// API is the slice of the generated client pwrwrdn uses. *pw.ClientWithResponses
// satisfies it; tests may too.
type API interface {
	PutClaimWithResponse(ctx context.Context, id pw.ClaimId, body pw.PutClaimJSONRequestBody, reqEditors ...pw.RequestEditorFn) (*pw.PutClaimResponse, error)
	GetClaimWithResponse(ctx context.Context, id pw.ClaimId, params *pw.GetClaimParams, reqEditors ...pw.RequestEditorFn) (*pw.GetClaimResponse, error)
	DeleteClaimWithResponse(ctx context.Context, id pw.ClaimId, reqEditors ...pw.RequestEditorFn) (*pw.DeleteClaimResponse, error)
	ExtendClaimWithResponse(ctx context.Context, id pw.ClaimId, body pw.ExtendClaimJSONRequestBody, reqEditors ...pw.RequestEditorFn) (*pw.ExtendClaimResponse, error)
	ClaimPowerWithResponse(ctx context.Context, id pw.ClaimId, body pw.ClaimPowerJSONRequestBody, reqEditors ...pw.RequestEditorFn) (*pw.ClaimPowerResponse, error)
	ListClaimsWithResponse(ctx context.Context, params *pw.ListClaimsParams, reqEditors ...pw.RequestEditorFn) (*pw.ListClaimsResponse, error)
	ListHostsWithResponse(ctx context.Context, params *pw.ListHostsParams, reqEditors ...pw.RequestEditorFn) (*pw.ListHostsResponse, error)
}

var _ API = (*pw.ClientWithResponses)(nil)

// Config is the connection config common to all subcommands.
type Config struct {
	URL    string
	CAFile string
}

// URLPath is the per-user URL file beside the token file.
func URLPath(home string) string { return filepath.Join(home, defaultURLPath) }

// ResolveConfig picks the base URL: the flag, then POWERWARDEN_URL, then the
// contents of ~/.config/powerwarden/url (whitespace trimmed). No URL is compiled in; when
// none of the three is set the error names all of them. POWERWARDEN_CA_FILE
// supplies the extra CA.
func ResolveConfig(flagURL string, getenv func(string) string, home string) (Config, error) {
	c := Config{CAFile: getenv(EnvCAFile)}
	for _, v := range []string{flagURL, getenv(EnvURL)} {
		if v = strings.TrimSpace(v); v != "" {
			c.URL = strings.TrimRight(v, "/")
			return c, nil
		}
	}
	b, err := os.ReadFile(URLPath(home))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("url file: %w", err)
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		c.URL = strings.TrimRight(v, "/")
		return c, nil
	}
	return c, errors.New("powerwarden URL not set: use --url, " + EnvURL + ", or write it to ~/" + defaultURLPath)
}

// TokenPath picks the token file: the flag, then POWERWARDEN_TOKEN_FILE, then
// ~/.config/powerwarden/token.
func TokenPath(flagValue string, getenv func(string) string, home string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := getenv(EnvTokenFile); v != "" {
		return v
	}
	return filepath.Join(home, defaultTokenPath)
}

// LoadToken reads the token file. A file readable by group or other is refused.
func LoadToken(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %04o: it must not be readable by group or other (chmod 600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// New builds the generated client with bearer auth. hc may be nil.
func New(cfg Config, token string, hc *http.Client) (*pw.ClientWithResponses, error) {
	if hc == nil {
		var err error
		if hc, err = HTTPClient(cfg.CAFile); err != nil {
			return nil, err
		}
	}
	return pw.NewClientWithResponses(cfg.URL, pw.WithHTTPClient(hc),
		pw.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
}

// HTTPClient returns an http client trusting the system roots plus caFile.
func HTTPClient(caFile string) (*http.Client, error) {
	hc := &http.Client{Timeout: 90 * time.Second}
	if caFile == "" {
		return hc, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("ca file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca file %s holds no certificate", caFile)
	}
	hc.Transport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return hc, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NewSessionID returns s-<12 hex>.
func NewSessionID() string { return "s-" + randHex(6) }

// NewClaimID returns <prefix>-<12 hex>; pwrwrdn's prefix is "pwrwrdn".
func NewClaimID(prefix string) string { return prefix + "-" + randHex(6) }

// Refusal is a non-success answer from powerwarden.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (r *Refusal) Error() string {
	if r.Code == "" {
		return fmt.Sprintf("powerwarden answered %d: %s", r.Status, r.Message)
	}
	return fmt.Sprintf("powerwarden refused (%d %s): %s", r.Status, r.Code, r.Message)
}

// Transient reports a 5xx: worth retrying, not a verdict on the claim.
func (r *Refusal) Transient() bool { return r.Status >= 500 }

// Check returns nil when status is one of ok, otherwise a *Refusal decoded
// from the error body.
func Check(status int, body []byte, ok ...int) error {
	for _, s := range ok {
		if status == s {
			return nil
		}
	}
	r := &Refusal{Status: status}
	var e pw.Error
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		r.Code, r.Message = string(e.Error), e.Message
	} else {
		r.Message = strings.TrimSpace(string(body))
	}
	return r
}
