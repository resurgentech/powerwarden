# Work log

Append-only history of what shipped from `ROADMAP.md`. FOREMAN appends
an entry per landed step or tranche. Newest at the bottom. Corrections
are appended, never edited in; a hook refuses rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-18 — initial import

- powermaster (`powermaster/{api,daemon}`), powerwarden (`powerwarden/{api,daemon,mcp}`), their OpenAPI specs
  and design docs imported as five Go modules under `github.com/resurgentech/powerwarden/`, joined by a
  committed `go.work`; each module tagged `<dir>/v0.1.0`. orch process files seeded.
- Commits: the initial commit.

## 2026-11-19 — A6a powerwarden UI: path prefix

- New optional setting `ui.path_prefix` (default empty): the web UI is served under `<prefix>/ui/...` for a
  reverse proxy that keeps the prefix; templates use `{{base}}`, `app.js` reads `<body data-base>`, action
  redirects and the site-root redirect carry the prefix. Empty prefix: every URL unchanged. Validated (leading
  `/`, no trailing `/`, no `..`, unreserved characters per segment). Example config, README and design doc updated.
- Commits: the A6a commit on branch `phase-a-ui` (this entry ships in it).

## 2026-11-19 — A6b powerwarden UI: proxy identity

- New optional setting `ui.auth` (`htpasswd` default, or `proxy`) with `ui.proxy.secret_file`,
  `ui.proxy.secret_header`, `ui.proxy.user_header` (required in proxy mode, no defaults). Proxy mode trusts the
  user header when the secret header equals a line of the secret file (SHA-256, constant-time, every line); bad
  secret or name gets 401 without `WWW-Authenticate`; no basic auth or htpasswd; caller `ui:<user>`. The secret
  file must not be readable by group or others (startup and `check-config` fail). `check-config` loads it and
  appends ` ui=proxy`. htpasswd mode unchanged; `sameOrigin` and `/v1` untouched. Design doc, README, example.
- Release: `powerwarden/daemon/v0.2.0` (tag to be created after review).
- Commits: the A6b commit on branch `phase-a-ui` (this entry ships in it).
