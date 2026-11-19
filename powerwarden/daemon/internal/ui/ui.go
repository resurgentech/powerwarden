// Package ui is powerwarden's operator web UI at /ui: html/template pages,
// live updates over /ui/events (the same hub as /v1/events), and three
// actions (exclude toggle, create and delete manual demands). Everything is
// behind htpasswd basic auth; POSTs also pass an Origin/Referer check. The UI
// only calls internal/app; it never talks to powermaster and has no power
// buttons (manual power stays in `powermaster monitor`).
package ui

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	api "github.com/resurgentech/powerwarden/powerwarden/api"

	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/app"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/model"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/reconcile"
	"github.com/resurgentech/powerwarden/powerwarden/daemon/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// UI serves /ui.
type UI struct {
	App   *app.App
	Users *Htpasswd
	Log   *slog.Logger

	pages map[string]*template.Template
}

// New parses the embedded templates.
func New(a *app.App, users *Htpasswd, log *slog.Logger) (*UI, error) {
	u := &UI{App: a, Users: users, Log: log, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"ts":         func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") },
		"mmss":       mmss,
		"pathescape": url.PathEscape,
		"join":       strings.Join,
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	}
	base, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{"hosts", "demands", "workers", "decisions", "claims"} {
		t, err := template.Must(base.Clone()).ParseFS(assets, "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		u.pages[p] = t
	}
	return u, nil
}

// Register adds the /ui routes to mux.
func (u *UI) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(assets, "static")
	get := func(pattern string, h http.Handler) { mux.Handle("GET "+pattern, u.auth(h)) }
	post := func(pattern string, h http.HandlerFunc) { mux.Handle("POST "+pattern, u.auth(u.sameOrigin(h))) }
	// The bare site root goes to the UI; the redirect itself needs no auth.
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/hosts", http.StatusSeeOther))
	get("/ui/{$}", http.RedirectHandler("/ui/hosts", http.StatusSeeOther))
	get("/ui", http.RedirectHandler("/ui/hosts", http.StatusSeeOther))
	get("/ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	get("/ui/events", u.App.Hub)
	get("/ui/hosts", http.HandlerFunc(u.hosts))
	get("/ui/demands", http.HandlerFunc(u.demands))
	get("/ui/workers", http.HandlerFunc(u.workers))
	get("/ui/decisions", http.HandlerFunc(u.decisions))
	get("/ui/claims", http.HandlerFunc(u.claims))
	post("/ui/claims", u.postClaim)
	post("/ui/claims/{id}/delete", u.postReleaseClaim)
	post("/ui/hosts/{hostname}/exclude", u.postExclude)
	post("/ui/hosts/{hostname}/tags", u.postHostTags)
	post("/ui/demands", u.postDemand)
	post("/ui/demands/{id}/delete", u.postDeleteDemand)
}

// ---- auth and CSRF ----

func (u *UI) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !u.Users.Check(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="powerwarden", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects a POST unless Origin (or, failing that, Referer) names
// the host the request was sent to. Browsers attach basic-auth credentials to
// cross-site form posts, so this is the CSRF guard.
func (u *UI) sameOrigin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := r.Header.Get("Origin")
		if src == "" || src == "null" {
			src = r.Header.Get("Referer")
		}
		pu, err := url.Parse(src)
		if src == "" || err != nil || pu.Host == "" || !sameString(pu.Host, r.Host) {
			u.Log.Warn("ui: cross-origin post rejected", "origin", r.Header.Get("Origin"), "referer", r.Header.Get("Referer"), "host", r.Host)
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// ---- pages ----

type header struct {
	Stats        api.Stats
	Connected    bool
	Now          time.Time
	Page         string
	Flash, Error string
}

func (u *UI) header(r *http.Request, page string) header {
	return header{Stats: u.App.Stats(), Connected: u.App.View.Connected(),
		Now: u.App.Clock.Now(), Page: page, Flash: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("err")}
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	name := "layout"
	if r.Header.Get("X-Partial") == "1" {
		name = "live"
	}
	var buf bytes.Buffer
	if err := u.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		u.Log.Error("ui: render", "page", page, "error", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

type hostRow struct {
	api.HostView
	Status []string // policy status labels with countdowns
	// NameNotes flags name-tag trouble: duplicate short name, ignored explicit tags.
	NameNotes []string
}

func (u *UI) hosts(w http.ResponseWriter, r *http.Request) {
	hs, err := u.App.Hosts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := u.App.Clock.Now()
	t := u.App.Cfg.Timings
	last := u.App.Rec.Last().Output.Hosts
	rows := make([]hostRow, 0, len(hs))
	idx := u.App.NameIndex()
	dups := idx.Duplicates()
	for _, h := range hs {
		var notes []string
		if s := reconcile.ShortName(h.Hostname); len(dups[s]) > 1 {
			notes = append(notes, fmt.Sprintf("duplicate short name %q (shared by %s): no short tag", s, strings.Join(dups[s], ", ")))
		}
		if stale := idx.Stale(h.Hostname, h.Tags); len(stale) > 0 {
			notes = append(notes, "ignored: "+strings.Join(stale, ", ")+" (equal to a host name)")
		}
		var st []string
		if h.BusyHeldUntil != nil {
			st = append(st, "busy-held until "+h.BusyHeldUntil.UTC().Format("15:04:05Z"))
		}
		if last[h.Hostname].IdleEligible && h.IdleSince != nil {
			st = append(st, "idle dwell "+mmss(now.Sub(*h.IdleSince))+" / "+mmss(t.MinIdleTime))
		}
		if h.ColdOffDueAt != nil {
			st = append(st, "cold off in "+mmss(h.ColdOffDueAt.Sub(now)))
		}
		if last[h.Hostname].Booting {
			st = append(st, "booting")
		}
		if h.BootGraceUntil != nil {
			st = append(st, "boot grace "+mmss(h.BootGraceUntil.Sub(now)))
		}
		if h.CooldownUntil != nil {
			st = append(st, "cooldown "+mmss(h.CooldownUntil.Sub(now)))
		}
		rows = append(rows, hostRow{HostView: h, Status: st, NameNotes: notes})
	}
	u.render(w, r, "hosts", struct {
		header
		Hosts []hostRow
	}{u.header(r, "hosts"), rows})
}

type demandRow struct {
	api.Demand
	Age, LeaseLeft string
	NoCandidate    bool
}

func (u *UI) demands(w http.ResponseWriter, r *http.Request) {
	ds, err := u.App.Demands(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := u.App.Clock.Now()
	noCand := map[string]bool{}
	for _, id := range u.App.Rec.Last().Output.NoCandidate {
		noCand[id] = true
	}
	rows := make([]demandRow, 0, len(ds))
	for _, d := range ds {
		rows = append(rows, demandRow{Demand: d, Age: mmss(now.Sub(d.CreatedAt)), LeaseLeft: mmss(d.LeaseUntil.Sub(now)),
			NoCandidate: d.CoveredBy == nil && noCand[d.Id]})
	}
	u.render(w, r, "demands", struct {
		header
		Demands []demandRow
	}{u.header(r, "demands"), rows})
}

type workerRow struct {
	api.Worker
	LeaseLeft, BusyHoldLeft string
	Expired                 bool
	Flags                   []string
}

func (u *UI) workers(w http.ResponseWriter, r *http.Request) {
	ws, err := u.App.Workers(r.Context(), true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	policies, err := u.App.Store.HostPolicies(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := u.App.Clock.Now()
	rows := make([]workerRow, 0, len(ws))
	for _, wk := range ws {
		row := workerRow{Worker: wk, Expired: !wk.LeaseUntil.After(now), LeaseLeft: mmss(wk.LeaseUntil.Sub(now))}
		if wk.LastBusyAt != nil {
			if left := wk.LastBusyAt.Add(u.App.Cfg.Timings.BusyGrace).Sub(now); left > 0 {
				row.BusyHoldLeft = mmss(left)
			}
		}
		if _, ok := u.App.View.Host(wk.Hostname); !ok {
			row.Flags = append(row.Flags, "unknown hostname")
		} else if ht := policies[wk.Hostname].Tags; len(ht) > 0 && !(reconcile.Covers(ht, wk.Tags) && reconcile.Covers(wk.Tags, ht)) {
			row.Flags = append(row.Flags, "tags differ from host ("+strings.Join(ht, ",")+")")
		}
		rows = append(rows, row)
	}
	u.render(w, r, "workers", struct {
		header
		Workers []workerRow
	}{u.header(r, "workers"), rows})
}

func (u *UI) decisions(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "decisions", struct {
		header
		Decisions []api.Decision
	}{u.header(r, "decisions"), u.App.Decisions()})
}

// ---- actions ----

func back(w http.ResponseWriter, r *http.Request, page string, err error, ok string) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if ok != "" {
		q.Set("ok", ok)
	}
	http.Redirect(w, r, "/ui/"+page+"?"+q.Encode(), http.StatusSeeOther)
}

func (u *UI) postExclude(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("hostname")
	exclude := r.PostFormValue("exclude") == "true"
	_, err := u.App.SetExclude(r.Context(), host, exclude)
	if errors.Is(err, store.ErrNotFound) {
		err = fmt.Errorf("powermaster does not know host %q", host)
	}
	u.Log.Info("ui: exclude", "host", host, "exclude", exclude, "user", user(r), "error", err)
	back(w, r, "hosts", err, fmt.Sprintf("%s exclude=%v", host, exclude))
}

func (u *UI) postHostTags(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("hostname")
	tags := strings.FieldsFunc(r.PostFormValue("tags"), func(c rune) bool { return c == ',' || c == ' ' })
	_, err := u.App.SetHostTags(r.Context(), host, tags)
	if errors.Is(err, store.ErrNotFound) {
		err = fmt.Errorf("powermaster does not know host %q", host)
	}
	u.Log.Info("ui: host tags", "host", host, "tags", tags, "user", user(r), "error", err)
	back(w, r, "hosts", err, fmt.Sprintf("%s tags=%s", host, strings.Join(tags, ",")))
}

func (u *UI) postDemand(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	tags := strings.FieldsFunc(r.PostFormValue("tags"), func(c rune) bool { return c == ',' || c == ' ' })
	in := store.DemandPut{ID: id, Tags: tags, Source: model.SourceManual,
		ExternalRef: "ui:" + user(r)}
	if v := strings.TrimSpace(r.PostFormValue("lease_seconds")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			back(w, r, "demands", fmt.Errorf("lease_seconds must be a number"), "")
			return
		}
		in.LeaseSeconds = &n
	}
	_, err := u.App.PutDemand(r.Context(), in)
	u.Log.Info("ui: manual demand", "id", id, "tags", tags, "user", user(r), "error", err)
	back(w, r, "demands", err, "manual demand "+id+" stored")
}

func (u *UI) postDeleteDemand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := u.App.Demand(r.Context(), id, true)
	switch {
	case errors.Is(err, store.ErrNotFound):
		err = nil // already gone
	case err == nil && d.Source != api.DemandSourceManual:
		err = fmt.Errorf("only manual demands can be deleted here (%s is %s)", id, d.Source)
	case err == nil:
		err = u.App.DeleteDemand(r.Context(), id)
	}
	u.Log.Info("ui: delete manual demand", "id", id, "user", user(r), "error", err)
	back(w, r, "demands", err, "manual demand "+id+" deleted")
}

type claimRow struct {
	api.Claim
	LeaseLeft, CapLeft string
}

func (u *UI) claims(w http.ResponseWriter, r *http.Request) {
	cs, err := u.App.Claims(r.Context(), store.ClaimFilter{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := u.App.Clock.Now()
	rows := make([]claimRow, 0, len(cs))
	for _, c := range cs {
		row := claimRow{Claim: c, CapLeft: mmss(c.ExpiresAt.Sub(now))}
		if c.LeaseUntil != nil {
			row.LeaseLeft = mmss(c.LeaseUntil.Sub(now))
		}
		rows = append(rows, row)
	}
	u.render(w, r, "claims", struct {
		header
		Claims []claimRow
		User   string
	}{u.header(r, "claims"), rows, user(r)})
}

// uiCaller is the claim identity of a UI user. The ':' cannot appear in a
// token name (^[A-Za-z0-9_.@-]{1,64}$), so a UI owner never equals a token
// name. Every UI user is an operator and may release any claim.
func uiCaller(r *http.Request) app.Caller {
	return app.Caller{Name: "ui:" + user(r), Operator: true}
}

// postClaim creates a detached claim through the same app call as the API.
func (u *UI) postClaim(w http.ResponseWriter, r *http.Request) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		back(w, r, "claims", err, "")
		return
	}
	id := "ui-" + hex.EncodeToString(rnd[:])
	tags := strings.FieldsFunc(r.PostFormValue("tags"), func(c rune) bool { return c == ',' || c == ' ' })
	in := store.ClaimPut{ID: id, Tags: tags, Mode: strings.TrimSpace(r.PostFormValue("mode")),
		Purpose: strings.TrimSpace(r.PostFormValue("purpose")), Detached: true}
	if v := strings.TrimSpace(r.PostFormValue("max_seconds")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			back(w, r, "claims", fmt.Errorf("hard cap must be a number of seconds"), "")
			return
		}
		in.MaxSeconds = &n
	}
	_, err := u.App.PutClaim(r.Context(), uiCaller(r), in)
	u.Log.Info("ui: claim", "id", id, "tags", tags, "user", user(r), "error", err)
	back(w, r, "claims", err, "claim "+id+" created")
}

func (u *UI) postReleaseClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := u.App.ReleaseClaim(r.Context(), uiCaller(r), id)
	u.Log.Info("ui: release claim", "id", id, "user", user(r), "error", err)
	back(w, r, "claims", err, "claim "+id+" released")
}

func user(r *http.Request) string {
	n, _, _ := r.BasicAuth()
	return n
}

// mmss formats a duration as m:ss (negative clamps to 0:00).
func mmss(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
