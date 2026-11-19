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

## 2026-11-19 — A6b follow-up: stricter secret-file mode; fail-fast bad-secret test

- `ui.proxy.secret_file` is now refused when any group or other permission bit is set (`perm & 0o077`, the rule
  the token loaders use), not only the read bits; docs, example and tests updated. `TestProxyRejectsBadSecret`
  reads status only under a deadline, so a broken secret check fails at once instead of hanging on `/ui/events`.
  The `powerwarden/daemon/v0.2.0` tag moves to this commit.
- Commits: the follow-up commit on top of 70d8c94 on branch `phase-a-ui` (this entry ships in it).

## 2026-11-19 — B4 key-request discovery in both daemons

- Spec first: `GET /v1/key-request` (no auth; `200 {"request_url"}` or `404`), schema `KeyRequestDiscovery` and tag
  `key-request` in both specs (info version 0.2.0); fixtures in `specs/fixtures/keyrequest/`; both api clients
  regenerated. New optional daemon setting `personal_keys.request_url` (absolute http/https, host, no userinfo, no
  fragment; no default). Unset: the route is 404 and nothing else changes. powerwarden: `RoleNone` rows for healthz
  and key-request in `routeRoles`; powermaster: an auth=false row. `check-config` / `check` append
  ` key_request=on` when set. Examples, READMEs, DESIGN docs.
- Release: `powermaster/api/v0.2.0`, `powerwarden/api/v0.2.0` on 6020d04; then a follow-up commit moves the daemons'
  requires and the `go.work` replaces to v0.2.0 with go.sum, and `powerwarden/daemon/v0.3.0`,
  `powermaster/daemon/v0.2.0` go on it (tags to be created after review).
- Commits: 6020d04 (code); this log entry ships in the next commit.
