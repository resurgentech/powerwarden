// Package keysource builds and starts the pulled key list (keyclient's
// keylist.Source) from the personal_keys config. serve and check-config share
// New, so check-config builds the source with exactly the options serve uses.
package keysource

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/keylist"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
)

// insecureHint is added when the refusal is the plain-http rule.
const insecureHint = "; set personal_keys.allow_insecure_http: true only for a network path you trust"

// New builds the Source. With no url it returns nil and no error and builds
// nothing. It does not connect, start or log. roles are the roles this daemon
// knows and takenLocally is a fixed check against the tokens loaded at start
// (see keylist.AcceptRoles). The error is "personal_keys: <keyclient's error>",
// with the hint when the refusal is the plain-http rule.
func New(p powermaster.PersonalKeys, roles []string, takenLocally func(keylist.Entry) bool, log *slog.Logger) (*keylist.Source, error) {
	if p.URL == "" {
		return nil, nil
	}
	src, err := keylist.New(keylist.Options{
		URL:               p.URL,
		TokenFile:         p.TokenFile,
		CacheFile:         p.CacheFile,
		Refresh:           p.Refresh.D(),
		RefreshOnMiss:     p.RefreshOnMiss.D(),
		AllowInsecureHTTP: p.AllowInsecureHTTP,
		Accept:            keylist.AcceptRoles(roles, takenLocally),
		Logger:            log,
	})
	if err != nil {
		if httpRuleRefusal(p) {
			return nil, fmt.Errorf("personal_keys: %w%s", err, insecureHint)
		}
		return nil, fmt.Errorf("personal_keys: %w", err)
	}
	return src, nil
}

// httpRuleRefusal reports whether keylist.New refuses p because of the http
// rule: plain http, no allow_insecure_http, and a host that is not loopback by
// keyclient's own rule. It does not look at keyclient's error text.
func httpRuleRefusal(p powermaster.PersonalKeys) bool {
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "http" || p.AllowInsecureHTTP {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	a, err := netip.ParseAddr(host)
	return !(err == nil && a.Unmap().IsLoopback())
}

// Start starts src with ctx and logs the one line of the lifecycle. A nil src
// (the key list is off) does nothing.
func Start(ctx context.Context, src *keylist.Source, log *slog.Logger) error {
	if src == nil {
		return nil
	}
	if err := src.Start(ctx); err != nil {
		return fmt.Errorf("personal_keys: %w", err)
	}
	log.Info(listLine(src.Status()))
	return nil
}

// listLine is the one start-up line: a single message, no attributes, the age
// rounded to whole seconds (0s when origin is none).
func listLine(st keylist.Status) string {
	return fmt.Sprintf("personal_keys: list origin=%s entries=%d age=%s", st.Origin, st.Entries, st.Age.Round(time.Second))
}
