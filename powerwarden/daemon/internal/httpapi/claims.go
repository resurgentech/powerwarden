package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

// caller is the authenticated token as the app layer sees it.
func (s *Server) caller(r *http.Request) app.Caller {
	name := CallerName(r.Context())
	return app.Caller{Name: name, Operator: s.Tokens.Role(name) == RoleFull}
}

func (s *Server) listClaims(w http.ResponseWriter, r *http.Request) {
	inc, ok := includeExpired(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := store.ClaimFilter{Owner: q.Get("owner"), Hostname: q.Get("hostname"), IncludeExpired: inc}
	switch q.Get("mine") {
	case "", "false":
	case "true":
		me := CallerName(r.Context())
		if f.Owner != "" && f.Owner != me {
			writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "mine=true conflicts with a different owner")
			return
		}
		f.Owner = me
	default:
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "mine must be true or false")
		return
	}
	cs, err := s.App.Claims(r.Context(), f)
	s.reply(w, http.StatusOK, cs, err)
}

func (s *Server) getClaim(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.ClaimIDPattern.MatchString)
	if !ok {
		return
	}
	inc, ok := includeExpired(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var since *int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "since must be a non-negative integer")
			return
		}
		since = &n
	}
	wait := time.Duration(math.MaxInt64) // omitted: the configured maximum
	if v := q.Get("wait"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "wait must be a non-negative number of seconds")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	c, err := s.App.WaitClaim(r.Context(), id, inc, since, wait)
	if r.Context().Err() != nil {
		return // client went away; nobody to answer
	}
	s.reply(w, http.StatusOK, c, err)
}

func (s *Server) putClaim(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.ClaimIDPattern.MatchString)
	if !ok {
		return
	}
	var body api.ClaimPut
	if !decode(w, r, &body) {
		return
	}
	in := store.ClaimPut{ID: id, Tags: body.Tags, Purpose: body.Purpose,
		MaxSeconds: body.MaxSeconds, LeaseSeconds: body.LeaseSeconds}
	if body.Session != nil {
		if *body.Session == "" {
			writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "session must be omitted or non-empty")
			return
		}
		in.Session = *body.Session
	}
	if body.Mode != nil {
		in.Mode = string(*body.Mode)
	}
	if body.Detached != nil {
		in.Detached = *body.Detached
	}
	c, err := s.App.PutClaim(r.Context(), s.caller(r), in)
	if err == nil {
		s.Log.Info("claim put", "id", id, "owner", c.Owner, "state", c.State, "by", CallerName(r.Context()))
	}
	s.reply(w, http.StatusOK, c, err)
}

func (s *Server) extendClaim(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.ClaimIDPattern.MatchString)
	if !ok {
		return
	}
	var body api.ClaimExtend
	if !decode(w, r, &body) {
		return
	}
	c, err := s.App.ExtendClaim(r.Context(), s.caller(r), id, body.MaxSeconds)
	s.reply(w, http.StatusOK, c, err)
}

func (s *Server) deleteClaim(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.ClaimIDPattern.MatchString)
	if !ok {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.App.ReleaseClaim(r.Context(), s.caller(r), id))
}

func (s *Server) claimPower(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.ClaimIDPattern.MatchString)
	if !ok {
		return
	}
	var body api.ClaimPower
	if !decode(w, r, &body) {
		return
	}
	switch body.Action {
	case api.ClaimPowerActionPowerup, api.ClaimPowerActionPowerdown, api.ClaimPowerActionPowercut, api.ClaimPowerActionReboot:
	default:
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "action must be powerup, powerdown, powercut or reboot")
		return
	}
	cmd, code, err := s.App.ClaimPower(r.Context(), s.caller(r), id, string(body.Action))
	s.reply(w, code, cmd, err)
}
