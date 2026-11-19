// Package httpapi is powermaster's hand-written net/http server for
// specs/openapi/powermaster.yml, using the generated types in powermasterapi.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	api "github.com/resurgentech/powerwarden/powermaster/api"

	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

// Server serves the powermaster API.
type Server struct {
	mgr       *powermaster.Manager
	tokens    *Tokens
	log       *slog.Logger
	heartbeat time.Duration
	mux       *http.ServeMux
}

// Options configure a Server.
type Options struct {
	Manager   *powermaster.Manager
	Tokens    *Tokens
	Log       *slog.Logger
	Heartbeat time.Duration // events.heartbeat; default 15s
}

type route struct {
	method, path string
	auth         bool
	h            func(http.ResponseWriter, *http.Request, string)
}

// New builds the server and its routes.
func New(o Options) (*Server, error) {
	if o.Manager == nil || o.Tokens == nil {
		return nil, errors.New("httpapi: manager and tokens are required")
	}
	s := &Server{mgr: o.Manager, tokens: o.Tokens, log: o.Log, heartbeat: o.Heartbeat, mux: http.NewServeMux()}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.heartbeat <= 0 {
		s.heartbeat = 15 * time.Second
	}
	for _, rt := range s.routes() {
		rt := rt
		s.mux.HandleFunc(rt.method+" "+rt.path, func(w http.ResponseWriter, r *http.Request) {
			caller := ""
			if rt.auth {
				name, ok := s.authenticate(r)
				if !ok {
					writeError(w, http.StatusUnauthorized, api.ErrorCodeUnauthorized, "missing or invalid bearer token")
					return
				}
				caller = name
			}
			rt.h(w, r, caller)
		})
	}
	// Anything else under the API: JSON 404 rather than the mux's text.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, api.ErrorCodeNotFound, "no such route")
	})
	return s, nil
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/v1/hosts", true, s.listHosts},
		{"GET", "/v1/hosts/{hostname}", true, s.getHost},
		{"POST", "/v1/hosts/{hostname}/power", true, s.requestPower},
		{"GET", "/v1/commands", true, s.listCommands},
		{"GET", "/v1/commands/{id}", true, s.getCommand},
		{"GET", "/v1/events", true, s.streamEvents},
		{"GET", "/v1/healthz", false, s.healthz},
		{"GET", "/v1/stats", true, s.stats},
	}
}

// Routes lists "METHOD /path" for every route (for the spec coverage test).
func (s *Server) Routes() []string {
	var out []string
	for _, r := range s.routes() {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) authenticate(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return s.tokens.Lookup(strings.TrimSpace(h[len(prefix):]))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code api.ErrorCode, msg string) {
	writeJSON(w, status, api.Error{Error: code, Message: msg})
}

// --- conversions (core types -> generated API types) ---

func toStatus(s powermaster.Status) api.HostStatus {
	return api.HostStatus{Pdu: s.PDU, Ipmi: s.IPMI, Ping: s.Ping, Ssh: s.SSH, ObservedAt: s.ObservedAt}
}

func toActive(a *powermaster.ActiveCommand) *api.ActiveCommand {
	if a == nil {
		return nil
	}
	return &api.ActiveCommand{Id: a.ID, Action: api.PowerAction(a.Action), State: api.CommandState(a.State), Deadline: a.Deadline}
}

func toHost(v powermaster.HostView) api.Host {
	return api.Host{
		Hostname: v.Hostname, Locked: v.Locked, Class: v.Class, State: api.HostState(v.State), StateSince: v.StateSince,
		Status: toStatus(v.Status), LastError: v.LastError, PowerFailedAt: v.PowerFailedAt, ActiveCommand: toActive(v.ActiveCommand),
	}
}

func toCommand(c store.Command) api.Command {
	return api.Command{
		Id: c.ID, Hostname: c.Hostname, Action: api.PowerAction(c.Action), State: api.CommandState(c.State),
		Force: c.Force, Escalated: c.Escalated, RequestedBy: c.RequestedBy, Reason: c.Reason, Attempt: c.Attempt,
		CreatedAt: c.CreatedAt, StartedAt: c.StartedAt, Deadline: c.Deadline, FinishedAt: c.FinishedAt, Error: c.Error,
	}
}

// --- handlers ---

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request, _ string) {
	views, err := s.mgr.Hosts()
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]api.Host, 0, len(views))
	for _, v := range views {
		out = append(out, toHost(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("hostname")
	d, err := s.mgr.Host(name)
	if errors.Is(err, powermaster.ErrUnknownHost) {
		writeError(w, http.StatusNotFound, api.ErrorCodeNotFound, fmt.Sprintf("host %s not found", name))
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	h := toHost(d.HostView)
	writeJSON(w, http.StatusOK, api.HostDetail{
		Hostname: h.Hostname, Locked: h.Locked, Class: h.Class, State: h.State, StateSince: h.StateSince, Status: h.Status,
		LastError: h.LastError, PowerFailedAt: h.PowerFailedAt, ActiveCommand: h.ActiveCommand,
		Budgets: api.Budgets{Powerup: d.Budgets[powermaster.ActionPowerup], Powerdown: d.Budgets[powermaster.ActionPowerdown], Powercut: d.Budgets[powermaster.ActionPowercut], Reboot: d.Budgets[powermaster.ActionReboot]},
	})
}

const maxReason = 256

func (s *Server) requestPower(w http.ResponseWriter, r *http.Request, caller string) {
	name := r.PathValue("hostname")
	force := false
	if v := r.URL.Query().Get("force"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "force must be true or false")
			return
		}
		force = b
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "cannot read body")
		return
	}
	var req api.PowerRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "invalid body: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "trailing data after body")
		return
	}
	switch req.Action {
	case api.Powerup, api.Powerdown, api.Powercut, api.Reboot:
	default:
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "action must be powerup, powerdown, powercut or reboot")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	if len(reason) > maxReason {
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "reason longer than 256 characters")
		return
	}
	res, err := s.mgr.Request(powermaster.Request{Hostname: name, Action: string(req.Action), Force: force, RequestedBy: caller, Reason: reason})
	switch {
	case errors.Is(err, powermaster.ErrUnknownHost):
		writeError(w, http.StatusNotFound, api.ErrorCodeNotFound, fmt.Sprintf("host %s not found", name))
		return
	case errors.Is(err, powermaster.ErrLocked):
		writeError(w, http.StatusConflict, api.ErrorCodeHostLocked, fmt.Sprintf("host %s is locked; retry with force=true", name))
		return
	case errors.Is(err, powermaster.ErrInFlight):
		writeError(w, http.StatusConflict, api.ErrorCodeCommandInFlight,
			fmt.Sprintf("%s command %d is %s for %s", res.Command.Action, res.Command.ID, res.Command.State, name))
		return
	case errors.Is(err, powermaster.ErrBadAction):
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "invalid action")
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	status := http.StatusAccepted
	if res.Outcome == store.OutcomeAlreadyThere {
		status = http.StatusOK
	}
	writeJSON(w, status, toCommand(res.Command))
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	f := store.CommandFilter{Hostname: q.Get("hostname"), State: q.Get("state")}
	switch f.State {
	case "", store.CmdQueued, store.CmdRunning, store.CmdSucceeded, store.CmdFailed:
	default:
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "invalid state")
		return
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "since must be RFC 3339 date-time")
			return
		}
		f.Since = &t
	}
	cmds, err := s.mgr.Store().ListCommands(f)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]api.Command, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, toCommand(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCommand(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "id must be an integer")
		return
	}
	c, err := s.mgr.Store().GetCommand(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, api.ErrorCodeNotFound, fmt.Sprintf("command %d not found", id))
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCommand(c))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request, _ string) {
	if err := s.mgr.Store().Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, api.Healthz{Status: api.Unavailable})
		return
	}
	writeJSON(w, http.StatusOK, api.Healthz{Status: api.Ok})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, _ string) {
	views, err := s.mgr.Hosts()
	if err != nil {
		s.internal(w, err)
		return
	}
	st := api.Stats{HostsByState: map[string]int{}, Subscribers: s.mgr.Hub().Subscribers()}
	for _, v := range views {
		st.HostsByState[v.State]++
	}
	running, err := s.mgr.Store().ListCommands(store.CommandFilter{State: store.CmdRunning})
	if err != nil {
		s.internal(w, err)
		return
	}
	st.CommandsRunning = len(running)
	if last, err := s.mgr.Store().ListCommands(store.CommandFilter{Limit: 1}); err == nil && len(last) == 1 {
		t := last[0].CreatedAt
		st.LastCommandAt = &t
	}
	if failed, err := s.mgr.Store().ListCommands(store.CommandFilter{State: store.CmdFailed, Limit: 1}); err == nil && len(failed) == 1 {
		f := failed[0]
		at := f.CreatedAt
		if f.FinishedAt != nil {
			at = *f.FinishedAt
		}
		st.LastPowerError = &api.PowerError{Hostname: f.Hostname, CommandId: f.ID, Error: f.Error, At: at}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("api internal error", "error", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":"internal","message":"internal error"}` + "\n"))
}

// --- SSE ---

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, _ string) {
	var last uint64
	hasLast := false
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, api.ErrorCodeBadRequest, "Last-Event-ID must be an integer")
			return
		}
		last, hasLast = n, true
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, errors.New("response writer cannot flush"))
		return
	}
	sub := s.mgr.Hub().Subscribe(last, hasLast)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if sub.Resync {
		if writeEvent(w, s.mgr.Hub().ResyncEvent()) != nil {
			return
		}
	}
	for _, ev := range sub.Backlog {
		if writeEvent(w, ev) != nil {
			return
		}
	}
	flusher.Flush()

	hb := time.NewTicker(s.heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				// Dropped for lagging: the client reconnects with Last-Event-ID.
				return
			}
			if writeEvent(w, ev) != nil {
				return
			}
			flusher.Flush()
		case <-hb.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w io.Writer, ev events.Event) error {
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, ev.Data)
	return err
}
