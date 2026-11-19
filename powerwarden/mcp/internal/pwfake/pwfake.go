// Package pwfake is an httptest stand-in for powerwarden's claims and hosts
// API. It records every request. Tests override answers through Respond.
package pwfake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	pw "github.com/resurgentech/powerwarden/powerwarden/api"
)

// Req is one recorded request.
type Req struct {
	Method, Path string
	Query        map[string][]string
	Body         []byte
	Auth         string
}

// Fake is the fake server.
type Fake struct {
	Srv *httptest.Server

	// Respond, when set, may answer a request itself; return handled=false
	// to fall through to the default behaviour.
	Respond func(r Req) (status int, body any, handled bool)

	mu     sync.Mutex
	reqs   []Req
	Claims map[string]*pw.Claim
	Hosts  []pw.HostView
	Owner  string

	// OnCreate, when set, runs under the fake's lock right after a claim is
	// created; tests use it to bind a claim or fail it.
	OnCreate func(c *pw.Claim)
	// OnPower, when set, runs under the lock after a power command is accepted.
	OnPower func(c *pw.Claim, action string)
}

// New starts a fake. Close it with Srv.Close (or t.Cleanup).
func New() *Fake {
	f := &Fake{Claims: map[string]*pw.Claim{}, Owner: "jared@box"}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Requests returns a copy of the recorded requests.
func (f *Fake) Requests() []Req {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Req(nil), f.reqs...)
}

// Count counts recorded requests by method and exact path.
func (f *Fake) Count(method, path string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// Last returns the last request matching method and path.
func (f *Fake) Last(method, path string) (Req, bool) {
	rs := f.Requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Method == method && rs[i].Path == path {
			return rs[i], true
		}
	}
	return Req{}, false
}

// Claim returns a copy of a stored claim.
func (f *Fake) Claim(id string) (pw.Claim, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.Claims[id]
	if c == nil {
		return pw.Claim{}, false
	}
	return *c, true
}

// SetClaim stores a claim server side.
func (f *Fake) SetClaim(c *pw.Claim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Claims[c.Id] = c
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Err builds an Error body.
func Err(code pw.ErrorCode, msg string) pw.Error { return pw.Error{Error: code, Message: msg} }

func (f *Fake) serve(w http.ResponseWriter, hr *http.Request) {
	body, _ := io.ReadAll(hr.Body)
	r := Req{Method: hr.Method, Path: hr.URL.Path, Query: hr.URL.Query(), Body: body, Auth: hr.Header.Get("Authorization")}
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	respond := f.Respond
	f.mu.Unlock()
	if respond != nil {
		if st, b, ok := respond(r); ok {
			write(w, st, b)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.Path
	switch {
	case path == "/v1/hosts" && r.Method == http.MethodGet:
		write(w, 200, f.Hosts)
	case path == "/v1/claims" && r.Method == http.MethodGet:
		out := []pw.Claim{}
		for _, c := range f.Claims {
			out = append(out, *c)
		}
		write(w, 200, out)
	case strings.HasPrefix(path, "/v1/claims/"):
		rest := strings.TrimPrefix(path, "/v1/claims/")
		id, sub, _ := strings.Cut(rest, "/")
		f.claim(w, r, id, sub)
	default:
		write(w, 404, Err(pw.ErrorCodeNotFound, "no route"))
	}
}

func (f *Fake) claim(w http.ResponseWriter, r Req, id, sub string) {
	c := f.Claims[id]
	switch {
	case sub == "" && r.Method == http.MethodPut:
		var in pw.ClaimPut
		if err := json.Unmarshal(r.Body, &in); err != nil {
			write(w, 400, Err(pw.ErrorCodeBadRequest, err.Error()))
			return
		}
		now := time.Now().UTC()
		if c == nil {
			lease := 900
			if in.LeaseSeconds != nil {
				lease = *in.LeaseSeconds
			}
			maxs := 8 * 3600
			if in.MaxSeconds != nil {
				maxs = *in.MaxSeconds
			}
			mode := pw.Hold
			if in.Mode != nil {
				mode = *in.Mode
			}
			lu := now.Add(time.Duration(lease) * time.Second)
			sess := ""
			if in.Session != nil {
				sess = *in.Session
			}
			c = &pw.Claim{Id: id, Owner: f.Owner, Session: sess, Purpose: in.Purpose, Tags: in.Tags,
				Mode: mode, State: pw.Pending, Detail: "waiting for box1", Version: 1,
				LeaseSeconds: &lease, LeaseUntil: &lu, ExpiresAt: now.Add(time.Duration(maxs) * time.Second),
				CreatedAt: now, UpdatedAt: now}
			if in.Detached != nil && *in.Detached {
				c.Detached, c.LeaseSeconds, c.LeaseUntil = true, nil, nil
			}
			f.Claims[id] = c
			if f.OnCreate != nil {
				f.OnCreate(c)
			}
		} else if c.LeaseSeconds != nil {
			lu := now.Add(time.Duration(*c.LeaseSeconds) * time.Second)
			c.LeaseUntil = &lu
		}
		write(w, 200, c)
	case c == nil:
		if r.Method == http.MethodDelete {
			write(w, 204, nil)
			return
		}
		write(w, 404, Err(pw.ErrorCodeNotFound, "no such claim"))
	case sub == "" && r.Method == http.MethodGet:
		write(w, 200, c)
	case sub == "" && r.Method == http.MethodDelete:
		delete(f.Claims, id)
		write(w, 204, nil)
	case sub == "extend" && r.Method == http.MethodPost:
		var in pw.ClaimExtend
		_ = json.Unmarshal(r.Body, &in)
		c.ExpiresAt = time.Now().UTC().Add(time.Duration(in.MaxSeconds) * time.Second)
		write(w, 200, c)
	case sub == "power" && r.Method == http.MethodPost:
		var in pw.ClaimPower
		_ = json.Unmarshal(r.Body, &in)
		if f.OnPower != nil {
			f.OnPower(c, string(in.Action))
		}
		write(w, 202, pw.Command{Action: string(in.Action), State: "queued", Hostname: "box1.labtwo", Id: 7})
	default:
		write(w, 404, Err(pw.ErrorCodeNotFound, "no route"))
	}
}
