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

## 2026-11-19 — B5 key list source in both daemons

- Spec first: shared schema `PersonalKeysStatus` and optional `personal_keys` on `GET /v1/stats` in both specs (info
  version 0.3.0; the `last_error` description is quoted, as the unquoted flow-style text is invalid); both api
  clients regenerated.
- Both daemons: new optional settings `personal_keys.url`, `token_file`, `cache_file` (required with url, no
  default), `refresh` (1m), `refresh_on_miss` (5s), `allow_insecure_http`, with the shared validation wording
  ("set" means written in the file). With url set, a keyclient `keylist.Source` is built after the tokens are loaded
  (powerwarden accepts roles full and agent, powermaster full; tokens.yaml names and hashes win), started with the
  serve context before the listener, and asked after a tokens-file miss (refresh on miss). powerwarden: a pulled
  key's name is the caller and claim owner, and its role goes through the route-to-role table. `GET /v1/stats` gains
  `personal_keys` (UTC times). `check-config` / `check` build the source with serve's options and append
  ` key_list=on` and ` key_list_insecure_http=on`. Start-up line `personal_keys: list origin=… entries=… age=…`.
  Url unset: nothing changes. Both daemons require `keyclient v0.1.1`. SR-8 tests named after keyclient's STATES rows.
- Release: `powermaster/api/v0.3.0`, `powerwarden/api/v0.3.0` on 962df01; 6f2503a moves the daemons' requires and the
  `go.work` replaces to v0.3.0; after the api tags are pushed a go.sum commit follows, and
  `powerwarden/daemon/v0.4.0`, `powermaster/daemon/v0.3.0` go on it (tags to be created after review).
- Commits: 962df01, a6d03b0, 3592420, d918b6b, 6f2503a; this log entry ships in the next commit.
