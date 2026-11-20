package keyreq

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
)

// Gate is the MCP server's way to a key (decision D10). Each tool call asks
// Connect for the API; until a key is stored Connect returns an error whose
// text is the tool result. Ensure runs with Wait false, so Connect never
// blocks on a person, and nothing is printed: the notice goes in the text.
type Gate struct {
	ctx       context.Context
	cfg       client.Config
	opts      request.Options
	hc        *http.Client
	tokenPath string

	mu      sync.Mutex
	pending *request.ErrPending
}

// NewGate builds a Gate. ctx must outlive the tool calls (the background poll
// is bounded by it). Wait and Notify in p are ignored.
func NewGate(ctx context.Context, cfg client.Config, p Params) (*Gate, error) {
	hc, err := client.HTTPClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	p.Wait, p.Notify, p.HTTPClient = false, nil, hc
	return &Gate{ctx: ctx, cfg: cfg, opts: Options(p), hc: hc, tokenPath: p.TokenPath}, nil
}

// Connect returns the API once a key is on this machine, else an error with
// the text for the tool result.
func (g *Gate) Connect(context.Context) (client.API, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p := g.pending; p != nil {
		select {
		case <-p.Done():
			g.pending = nil // the call after a final error files a new request
			if err := p.Err(); err != nil {
				return nil, g.failure(err)
			}
			tok, err := client.LoadToken(g.tokenPath)
			if err != nil {
				return nil, err
			}
			return g.build(tok, true) // the key landed through this flow
		default:
			return nil, errors.New(PendingMCPText(p.Notice))
		}
	}
	res, err := request.Ensure(g.ctx, g.opts)
	if err == nil {
		return g.build(res.Token, res.Name != "")
	}
	var p *request.ErrPending
	if errors.As(err, &p) {
		g.pending = p
		return nil, errors.New(PendingMCPText(p.Notice))
	}
	return nil, g.failure(err)
}

func (g *Gate) build(token string, armed bool) (client.API, error) {
	api, err := client.New(g.cfg, token, WrapRetry(g.hc, armed))
	if err != nil {
		return nil, err
	}
	return api, nil
}

// failure is the tool result text for a final error.
func (g *Gate) failure(err error) error {
	switch {
	case errors.Is(err, request.ErrBusy):
		return errors.New(MCPBusyText)
	case errors.Is(err, request.ErrNoProvider):
		if _, lerr := client.LoadToken(g.tokenPath); lerr != nil {
			return lerr // today's no-token error
		}
		return err
	}
	if lines, _, ok := Classify(err, g.tokenPath); ok {
		return errors.New(strings.Join(lines, "\n"))
	}
	return err
}
