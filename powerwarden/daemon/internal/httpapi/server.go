// Package httpapi serves powerwarden's /v1 API (specs/openapi/powerwarden.yml)
// using the generated types from powerwarden/api.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

const maxBody = 64 * 1024

type ctxKey struct{}

// CallerName returns the authenticated token name, if any.
func CallerName(ctx context.Context) string {
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}

// Server is the /v1 handler set.
type Server struct {
	App    *app.App
	Tokens *Tokens
	Log    *slog.Logger
	// KeyRequestURL is personal_keys.request_url; empty makes
	// GET /v1/key-request answer 404.
	KeyRequestURL string
}

// Register adds every /v1 route to mux.
func (s *Server) Register(mux *http.ServeMux) {
	auth := func(pattern string, h http.HandlerFunc) {
		need, ok := RouteRole(pattern)
		if !ok {
			panic("httpapi: no role decision for route " + pattern)
		}
		if need == RoleNone {
			mux.Handle(pattern, h)
			return
		}
		mux.Handle(pattern, s.bearer(need, h))
	}
	auth("GET /v1/healthz", s.healthz)
	auth("GET /v1/key-request", s.keyRequest)
	auth("GET /v1/demands", s.listDemands)
	auth("GET /v1/demands/{id}", s.getDemand)
	auth("PUT /v1/demands/{id}", s.putDemand)
	auth("DELETE /v1/demands/{id}", s.deleteDemand)
	auth("GET /v1/workers", s.listWorkers)
	auth("PUT /v1/workers/{worker_id}", s.putWorker)
	auth("DELETE /v1/workers/{worker_id}", s.deleteWorker)
	auth("GET /v1/claims", s.listClaims)
	auth("GET /v1/claims/{id}", s.getClaim)
	auth("PUT /v1/claims/{id}", s.putClaim)
	auth("DELETE /v1/claims/{id}", s.deleteClaim)
	auth("POST /v1/claims/{id}/extend", s.extendClaim)
	auth("POST /v1/claims/{id}/power", s.claimPower)
	auth("GET /v1/hosts", s.listHosts)
	auth("GET /v1/hosts/{hostname}", s.getHost)
	auth("PUT /v1/hosts/{hostname}/exclude", s.putExclude)
	auth("PUT /v1/hosts/{hostname}/tags", s.putHostTags)
	auth("GET /v1/decisions", s.listDecisions)
	auth("GET /v1/events", s.App.Hub.ServeHTTP)
	auth("GET /v1/stats", s.stats)
}

// bearer authenticates the token and enforces the route's least role.
func (s *Server) bearer(need string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		name, valid := "", false
		if ok {
			name, valid = s.Tokens.Name(strings.TrimSpace(tok))
		}
		if !valid {
			writeErr(w, http.StatusUnauthorized, api.ErrorCodeUnauthorized, "missing or invalid bearer token")
			return
		}
		if !roleAllows(s.Tokens.Role(name), need) {
			writeErr(w, http.StatusForbidden, api.ErrorCodeForbidden, "token role "+s.Tokens.Role(name)+" may not call this operation")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
	})
}

// ---- demands ----

func (s *Server) listDemands(w http.ResponseWriter, r *http.Request) {
	inc, ok := includeExpired(w, r)
	if !ok {
		return
	}
	ds, err := s.App.Demands(r.Context(), inc)
	s.reply(w, http.StatusOK, ds, err)
}

func (s *Server) getDemand(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.DemandIDPattern.MatchString)
	if !ok {
		return
	}
	inc, ok := includeExpired(w, r)
	if !ok {
		return
	}
	d, err := s.App.Demand(r.Context(), id, inc)
	s.reply(w, http.StatusOK, d, err)
}

func (s *Server) putDemand(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.DemandIDPattern.MatchString)
	if !ok {
		return
	}
	var body api.DemandPut
	if !decode(w, r, &body) {
		return
	}
	if body.Id != nil && *body.Id != id {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "body id must equal the path id")
		return
	}
	if body.Tags == nil {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "tags is required (may be empty)")
		return
	}
	in := store.DemandPut{ID: id, Tags: body.Tags, LeaseSeconds: body.LeaseSeconds, Source: string(body.Source)}
	if body.ExternalRef != nil {
		in.ExternalRef = *body.ExternalRef
	}
	d, err := s.App.PutDemand(r.Context(), in)
	if err == nil {
		s.Log.Info("demand put", "id", id, "tags", d.Tags, "by", CallerName(r.Context()))
	}
	s.reply(w, http.StatusOK, d, err)
}

func (s *Server) deleteDemand(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "id", store.DemandIDPattern.MatchString)
	if !ok {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.App.DeleteDemand(r.Context(), id))
}

// ---- workers ----

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	inc, ok := includeExpired(w, r)
	if !ok {
		return
	}
	ws, err := s.App.Workers(r.Context(), inc)
	s.reply(w, http.StatusOK, ws, err)
}

func (s *Server) putWorker(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "worker_id", store.WorkerIDPattern.MatchString)
	if !ok {
		return
	}
	var body api.WorkerPut
	if !decode(w, r, &body) {
		return
	}
	if body.WorkerId != nil && *body.WorkerId != id {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "body worker_id must equal the path worker_id")
		return
	}
	if body.Tags == nil {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "tags is required (may be empty)")
		return
	}
	in := store.WorkerPut{WorkerID: id, Hostname: body.Hostname, Tags: body.Tags,
		JobStatus: string(body.JobStatus), LeaseSeconds: body.LeaseSeconds}
	if body.DemandId != nil {
		in.DemandID = *body.DemandId
	}
	wk, err := s.App.PutWorker(r.Context(), in)
	s.reply(w, http.StatusOK, wk, err)
}

func (s *Server) deleteWorker(w http.ResponseWriter, r *http.Request) {
	id, ok := pathMatch(w, r, "worker_id", store.WorkerIDPattern.MatchString)
	if !ok {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.App.DeleteWorker(r.Context(), id))
}

// ---- hosts, decisions, stats, health ----

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !q.Has("tags") {
		hs, err := s.App.Hosts(r.Context())
		s.reply(w, http.StatusOK, hs, err)
		return
	}
	tags := strings.Split(q.Get("tags"), ",")
	for _, t := range tags {
		if !store.TagPattern.MatchString(t) {
			writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, fmt.Sprintf("tags: %q does not match %s", t, store.TagPattern))
			return
		}
	}
	hs, err := s.App.HostsByTags(r.Context(), tags)
	s.reply(w, http.StatusOK, hs, err)
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	h, err := s.App.Host(r.Context(), r.PathValue("hostname"))
	s.reply(w, http.StatusOK, h, err)
}

func (s *Server) putExclude(w http.ResponseWriter, r *http.Request) {
	var body api.ExcludePut
	if !decode(w, r, &body) {
		return
	}
	h, err := s.App.SetExclude(r.Context(), r.PathValue("hostname"), body.Exclude)
	s.reply(w, http.StatusOK, h, err)
}

func (s *Server) putHostTags(w http.ResponseWriter, r *http.Request) {
	var body api.HostTagsPut
	if !decode(w, r, &body) {
		return
	}
	if body.Tags == nil {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "tags is required")
		return
	}
	h, err := s.App.SetHostTags(r.Context(), r.PathValue("hostname"), body.Tags)
	s.reply(w, http.StatusOK, h, err)
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.Decisions())
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.Stats())
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	h := api.Healthz{Status: api.Ok, Powermaster: api.Connected}
	if !s.App.View.Connected() {
		h.Powermaster = api.Unreachable
	}
	code := http.StatusOK
	if !s.App.Healthy(r.Context()) {
		h.Status, code = api.Unavailable, http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

// keyRequest advertises where keys are requested; no authentication.
func (s *Server) keyRequest(w http.ResponseWriter, r *http.Request) {
	if s.KeyRequestURL == "" {
		writeErr(w, http.StatusNotFound, api.ErrorCodeNotFound, "no key request URL configured")
		return
	}
	writeJSON(w, http.StatusOK, api.KeyRequestDiscovery{RequestUrl: s.KeyRequestURL})
}

// ---- helpers ----

func (s *Server) reply(w http.ResponseWriter, code int, v any, err error) {
	var pe *app.PowerError
	switch {
	case err == nil && code == http.StatusNoContent:
		w.WriteHeader(code)
	case err == nil:
		writeJSON(w, code, v)
	case errors.As(err, &pe):
		writeErr(w, pe.Status, pe.Code, pe.Msg)
	case errors.Is(err, store.ErrBadRequest):
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, err.Error())
	case errors.Is(err, store.ErrCapacity):
		writeErr(w, http.StatusTooManyRequests, api.ErrorCodeCapacity, err.Error())
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, api.ErrorCodeForbidden, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, api.ErrorCodeConflict, err.Error())
	case errors.Is(err, store.ErrExpired):
		writeErr(w, http.StatusGone, api.ErrorCodeClaimExpired, err.Error())
	case errors.Is(err, store.ErrClaimFailed):
		msg := err.Error()
		var cf *store.ClaimFailedError
		if errors.As(err, &cf) {
			msg = cf.Detail // the message is the claim's detail
		}
		writeErr(w, http.StatusGone, api.ErrorCodeClaimFailed, msg)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, api.ErrorCodeNotFound, "not found")
	default:
		s.Log.Error("request failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func pathMatch(w http.ResponseWriter, r *http.Request, name string, ok func(string) bool) (string, bool) {
	v := r.PathValue(name)
	if !ok(v) {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, fmt.Sprintf("%s %q is invalid", name, v))
		return "", false
	}
	return v, true
}

func includeExpired(w http.ResponseWriter, r *http.Request) (bool, bool) {
	switch v := r.URL.Query().Get("include_expired"); v {
	case "", "false":
		return false, true
	case "true":
		return true, true
	default:
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "include_expired must be true or false")
		return false, false
	}
}

// decode reads a JSON body, rejecting unknown fields and trailing data.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeErr(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "trailing data after JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ec api.ErrorCode, msg string) {
	writeJSON(w, code, api.Error{Error: ec, Message: msg})
}
