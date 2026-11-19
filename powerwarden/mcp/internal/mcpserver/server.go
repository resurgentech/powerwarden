// Package mcpserver exposes powerwarden claims to an agent as MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pw "github.com/resurgentech/powerwarden/powerwarden/api"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/client"
	"github.com/resurgentech/powerwarden/powerwarden/mcp/internal/keeper"
)

// waitSeconds is the blocking-read length for wait_for_claim.
const waitSeconds = 60

// Config wires the server. The keeper carries the session id and the claim id
// prefix ("mcp").
type Config struct {
	API    client.API
	Keeper *keeper.Keeper
}

type server struct {
	api client.API
	k   *keeper.Keeper
}

// New builds the MCP server with every tool registered.
func New(c Config) *mcp.Server {
	s := &server{api: c.API, k: c.Keeper}
	srv := mcp.NewServer(&mcp.Implementation{Name: "pwrwrdn", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Claim lab hosts through powerwarden. claim_host returns at once; poll wait_for_claim until state is ready. " +
			"Leases are renewed for you. Always release_host when done.",
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_hosts", Description: "List hosts with explicit tags, name tags (a host is named by its name tag), power state, jobs_blocked and claims. Optional tags filter: a host must carry all of them."}, s.listHosts)
	mcp.AddTool(srv, &mcp.Tool{Name: "claim_host", Description: "Claim one host chosen by tags (name a host with its name tag). Returns at once, never waits for a boot: poll wait_for_claim. mode: hold (default; others may hold/reserve, jobs allowed, host kept on), reserve (like hold but hourchd jobs blocked), dedicated (exclusive, jobs blocked, you control power with power_host)."}, s.claimHost)
	mcp.AddTool(srv, &mcp.Tool{Name: "wait_for_claim", Description: "Block up to about 60s until the claim changes (state, detail, host_state, hostname), then return it. Call again until state is ready, and stop on failed: failed is final (see detail), the claim holds nothing, and a bound claim never moves to another host, so claim again if you still need one. For dedicated, ready does not mean the host is on: read host_state."}, s.waitForClaim)
	mcp.AddTool(srv, &mcp.Tool{Name: "extend_claim", Description: "Set the claim's hard cap to now plus hours (clamped by the server)."}, s.extendClaim)
	mcp.AddTool(srv, &mcp.Tool{Name: "release_host", Description: "Release a claim. Also acknowledges a lost-claim warning for it."}, s.releaseHost)
	mcp.AddTool(srv, &mcp.Tool{Name: "power_host", Description: "Send a power command for the claim's host. hold/reserve: powerup, reboot. dedicated: powerup, powerdown, powercut, reboot. Returns the command, or the refusal code and message."}, s.powerHost)
	mcp.AddTool(srv, &mcp.Tool{Name: "my_claims", Description: "Claims owned by this token, from any session; this_session marks the ones made by this process."}, s.myClaims)
	return srv
}

// Serve runs srv on t until the peer closes (stdin EOF) or ctx ends (SIGTERM),
// then makes a best-effort release of every claim, 5s in total.
func Serve(ctx context.Context, srv *mcp.Server, k *keeper.Keeper, t mcp.Transport) error {
	err := srv.Run(ctx, t)
	rctx, cancel := context.WithTimeout(context.Background(), keeper.ReleaseTimeout)
	defer cancel()
	k.ReleaseAll(rctx)
	k.Close()
	return err
}

// ---- results ----

func (s *server) warning() string {
	lost := s.k.Lost()
	if len(lost) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("WARNING: these claims were lost and are no longer held:")
	for _, l := range lost {
		if l.Failed {
			fmt.Fprintf(&b, " %s (failed: %s);", l.ID, l.Reason)
			continue
		}
		fmt.Fprintf(&b, " %s (%s);", l.ID, l.Reason)
	}
	b.WriteString(" claim again if you still need a host, then call release_host on each lost id to dismiss this warning.")
	return b.String()
}

func (s *server) result(v any, isErr bool) *mcp.CallToolResult {
	var text string
	if str, ok := v.(string); ok {
		text = str
	} else {
		b, err := json.Marshal(v)
		if err != nil {
			text, isErr = err.Error(), true
		} else {
			text = string(b)
		}
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}
	if w := s.warning(); w != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: w})
	}
	return res
}

func (s *server) ok(v any) (*mcp.CallToolResult, any, error) { return s.result(v, false), nil, nil }

func (s *server) fail(err error) (*mcp.CallToolResult, any, error) {
	var ref *client.Refusal
	if errors.As(err, &ref) {
		return s.result(map[string]any{"error": ref.Code, "status": ref.Status, "message": ref.Message}, true), nil, nil
	}
	return s.result(err.Error(), true), nil, nil
}

// ---- views ----

type claimView struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Detail      string     `json:"detail,omitempty"`
	Hostname    *string    `json:"hostname"`
	HostState   *string    `json:"host_state"`
	Mode        string     `json:"mode"`
	Tags        []string   `json:"tags"`
	Purpose     string     `json:"purpose"`
	Version     int64      `json:"version"`
	LeaseUntil  *time.Time `json:"lease_until"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Detached    bool       `json:"detached"`
	Session     string     `json:"session,omitempty"`
	ThisSession bool       `json:"this_session"`
}

func (s *server) claimView(c pw.Claim) claimView {
	var hs *string
	if c.HostState != nil {
		v := string(*c.HostState)
		hs = &v
	}
	return claimView{ID: c.Id, State: string(c.State), Detail: c.Detail, Hostname: c.Hostname, HostState: hs,
		Mode: string(c.Mode), Tags: c.Tags, Purpose: c.Purpose, Version: c.Version, LeaseUntil: c.LeaseUntil,
		ExpiresAt: c.ExpiresAt, Detached: c.Detached, Session: c.Session,
		ThisSession: c.Session != "" && c.Session == s.k.Session()}
}

type hostClaimView struct {
	ID          string     `json:"id"`
	Owner       string     `json:"owner"`
	Session     string     `json:"session,omitempty"`
	Mode        string     `json:"mode"`
	State       string     `json:"state"`
	Purpose     string     `json:"purpose"`
	Detached    bool       `json:"detached"`
	LeaseUntil  *time.Time `json:"lease_until"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ThisSession bool       `json:"this_session"`
}

type hostView struct {
	Hostname    string          `json:"hostname"`
	NameTags    []string        `json:"name_tags"`
	Tags        []string        `json:"tags"`
	State       string          `json:"state"`
	JobsBlocked bool            `json:"jobs_blocked"`
	Locked      bool            `json:"locked"`
	Exclude     bool            `json:"exclude"`
	Claims      []hostClaimView `json:"claims"`
}

// ---- tools ----

type listHostsIn struct {
	Tags []string `json:"tags,omitempty" jsonschema:"only hosts carrying all of these tags (explicit or name tags)"`
}

func (s *server) listHosts(ctx context.Context, _ *mcp.CallToolRequest, in listHostsIn) (*mcp.CallToolResult, any, error) {
	var p pw.ListHostsParams
	if len(in.Tags) > 0 {
		t := strings.Join(in.Tags, ",")
		p.Tags = &t
	}
	resp, err := s.api.ListHostsWithResponse(ctx, &p)
	if err != nil {
		return s.fail(err)
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return s.fail(err)
	}
	out := []hostView{}
	if resp.JSON200 != nil {
		for _, h := range *resp.JSON200 {
			v := hostView{Hostname: h.Hostname, NameTags: nz(h.NameTags), Tags: nz(h.Tags), State: string(h.State),
				JobsBlocked: h.JobsBlocked, Locked: h.Locked, Exclude: h.Exclude, Claims: []hostClaimView{}}
			for _, c := range h.Claims {
				v.Claims = append(v.Claims, hostClaimView{ID: c.Id, Owner: c.Owner, Session: c.Session, Mode: string(c.Mode),
					State: string(c.State), Purpose: c.Purpose, Detached: c.Detached, LeaseUntil: c.LeaseUntil,
					ExpiresAt: c.ExpiresAt, ThisSession: c.Session != "" && c.Session == s.k.Session()})
			}
			out = append(out, v)
		}
	}
	return s.ok(map[string]any{"hosts": out})
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type claimHostIn struct {
	Tags     []string `json:"tags" jsonschema:"tags a host must cover; to name a host use its name tag (see list_hosts)"`
	Purpose  string   `json:"purpose" jsonschema:"what the host is for; shown to other users"`
	MaxHours float64  `json:"max_hours" jsonschema:"hard cap on the claim, in hours (server clamps to 15m..72h)"`
	Mode     string   `json:"mode,omitempty" jsonschema:"hold (default), reserve or dedicated"`
}

func (s *server) claimHost(ctx context.Context, _ *mcp.CallToolRequest, in claimHostIn) (*mcp.CallToolResult, any, error) {
	if len(in.Tags) == 0 {
		return s.fail(errors.New("tags: at least one tag is required"))
	}
	if strings.TrimSpace(in.Purpose) == "" {
		return s.fail(errors.New("purpose: required"))
	}
	if !(in.MaxHours > 0) {
		return s.fail(errors.New("max_hours: must be greater than 0"))
	}
	mode := pw.Hold
	switch in.Mode {
	case "":
	case "hold", "reserve", "dedicated":
		mode = pw.ClaimMode(in.Mode)
	default:
		return s.fail(fmt.Errorf("mode: %q is not hold, reserve or dedicated", in.Mode))
	}
	c, err := s.k.Create(ctx, keeper.Spec{Tags: in.Tags, Purpose: in.Purpose, Mode: mode, MaxSeconds: hoursToSeconds(in.MaxHours)})
	if err != nil {
		return s.fail(err)
	}
	return s.ok(s.claimView(*c))
}

func hoursToSeconds(h float64) int {
	const max = 1000 * 3600 // far above the server cap; avoids int overflow
	if h > 1000 {
		return max
	}
	n := int(h * 3600)
	if n < 1 {
		n = 1
	}
	return n
}

type claimIDIn struct {
	ClaimID string `json:"claim_id" jsonschema:"claim id from claim_host or my_claims"`
}

func (s *server) waitForClaim(ctx context.Context, _ *mcp.CallToolRequest, in claimIDIn) (*mcp.CallToolResult, any, error) {
	wait := waitSeconds
	p := pw.GetClaimParams{Wait: &wait, Since: s.k.Seen(in.ClaimID)}
	resp, err := s.api.GetClaimWithResponse(ctx, in.ClaimID, &p)
	if err != nil {
		return s.fail(err)
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		if ref, ok := err.(*client.Refusal); ok && (ref.Status == 404 || ref.Status == 410) {
			s.k.MarkLost(in.ClaimID, ref.Error())
		}
		return s.fail(err)
	}
	c := resp.JSON200
	if c == nil {
		return s.fail(errors.New("powerwarden answered 200 without a claim"))
	}
	s.k.SetSeen(c.Id, c.Version)
	if c.State == pw.Failed {
		s.k.MarkFailed(c.Id, c.Detail) // final: this session's claim holds nothing
	}
	return s.ok(s.claimView(*c))
}

type extendIn struct {
	ClaimID string  `json:"claim_id"`
	Hours   float64 `json:"hours" jsonschema:"new hard cap: now plus this many hours"`
}

func (s *server) extendClaim(ctx context.Context, _ *mcp.CallToolRequest, in extendIn) (*mcp.CallToolResult, any, error) {
	if !(in.Hours > 0) {
		return s.fail(errors.New("hours: must be greater than 0"))
	}
	resp, err := s.api.ExtendClaimWithResponse(ctx, in.ClaimID, pw.ClaimExtend{MaxSeconds: hoursToSeconds(in.Hours)})
	if err != nil {
		return s.fail(err)
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return s.fail(err)
	}
	return s.ok(s.claimView(*resp.JSON200))
}

func (s *server) releaseHost(ctx context.Context, _ *mcp.CallToolRequest, in claimIDIn) (*mcp.CallToolResult, any, error) {
	if err := s.k.Release(ctx, in.ClaimID); err != nil {
		return s.fail(err)
	}
	return s.ok(map[string]any{"released": in.ClaimID})
}

type powerIn struct {
	ClaimID string `json:"claim_id"`
	Action  string `json:"action" jsonschema:"powerup, powerdown, powercut or reboot (hold/reserve claims: powerup and reboot only)"`
}

func (s *server) powerHost(ctx context.Context, _ *mcp.CallToolRequest, in powerIn) (*mcp.CallToolResult, any, error) {
	resp, err := s.api.ClaimPowerWithResponse(ctx, in.ClaimID, pw.ClaimPower{Action: pw.ClaimPowerAction(in.Action)})
	if err != nil {
		return s.fail(err)
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200, 202); err != nil {
		return s.fail(err)
	}
	cmd := resp.JSON202
	already := false
	if resp.StatusCode() == 200 {
		cmd, already = resp.JSON200, true
	}
	return s.ok(map[string]any{"already_there": already, "command": cmd})
}

func (s *server) myClaims(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	mine := true
	resp, err := s.api.ListClaimsWithResponse(ctx, &pw.ListClaimsParams{Mine: &mine})
	if err != nil {
		return s.fail(err)
	}
	if err := client.Check(resp.StatusCode(), resp.Body, 200); err != nil {
		return s.fail(err)
	}
	out := []claimView{}
	if resp.JSON200 != nil {
		for _, c := range *resp.JSON200 {
			out = append(out, s.claimView(c))
		}
	}
	return s.ok(map[string]any{"claims": out})
}
