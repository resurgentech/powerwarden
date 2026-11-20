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

## 2026-11-19 — B5 follow-up: tokens loaders never echo a token

- Both daemons' `httpapi.LoadTokens` (used by `serve` and by `check-config` / `check`) redact yaml's backtick-quoted
  scalars to `` `<redacted>` ``, so a malformed tokens file no longer prints a token or its first characters
  (present in both `serve` paths since the initial import). Tests: `TestLoadTokensNeverEchoesAToken` in both,
  `TestCheckNeverEchoesAToken` (powermaster), `TestServeNeverEchoesAToken` (powerwarden).
- Commits: 742804f (powermaster); powerwarden in the commit that carries this entry.

## 2026-11-20 — B8 pwrwrdn: request a key on first use

- Every token-loading command (run, claim, release, extend, power, hosts, claims) asks for a key through keyclient's
  `request.Ensure` when the token file does not exist: `--key-request-url` / `POWERWARDEN_KEY_REQUEST_URL` (else
  discovery at the service), `--role` (default `agent`), `--no-key-request`; name hint `$USER@<short host>` (the
  short host alone without a user), tool `pwrwrdn/0.2.0`. With a terminal on stderr it waits and goes on after
  approval; otherwise it prints the notice and `key request pending; run this again after approving`, exit 75. The
  other outcomes print the shared tool wording (exit 75 for busy, 1 for denied, expired, consumed, unauthorized,
  provider errors); no provider keeps today's no-token error. A key received in the flow gets one retry, after 5 s,
  of its first call if that call is refused with 401.
- New `pwrwrdn key request` and `pwrwrdn key status` (token present or absent; the pending request's service, code
  and expiry, never its poll secret). `setup-token` stays, marked deprecated.
- `pwrwrdn mcp` starts without a key; each tool call goes through a gate: a pending request returns the error result
  `No powerwarden key on this machine yet. <notice> Then retry.`, busy and final errors return their wording, and
  the call after a final error files again; once the key lands the client is built and serves. Nothing else on
  stdout; the approval address is never opened.
- The module requires `keyclient v0.1.1`. README and DESIGN doc updated.
- Release: `powerwarden/mcp/v0.2.0` goes on the commit that carries this entry (tag to be created after review).
- Commits: the B8 commit on branch `b8-tools` (this entry ships in it).

## 2026-11-20 — B9 powermaster monitor: request a key on first use

- Before the TUI starts, a missing token file goes through keyclient's `request.Ensure`: `--key-request-url` /
  `POWERMASTER_KEY_REQUEST_URL` (else discovery), `--role` (default `full`), `--no-key-request`; name hint
  `$USER@<short host>` (the short host alone without a user), tool `powermaster/0.4.0`. With no terminal on stderr
  (or `--no-key-request`) no TUI runs: the notice and `key request pending; run this again after approving`, exit 75.
  The other outcomes use the same wording and exit codes as `pwrwrdn`; no provider keeps today's token-file error.
  A key received in the flow is tried with `GET /v1/hosts` before the TUI; a 401 is retried once after 5 s.
  `main` gains exit codes. README updated.
- Release: `powermaster/daemon/v0.4.0` goes on the commit that carries this entry (tag to be created after review).
- Commits: the B9 commit on branch `b8-tools` (this entry ships in it).

## 2026-11-20 — B8 follow-up: `key status` through keyclient's `request.Pending`

- `pwrwrdn key status` reads the pending request with keyclient v0.1.2's `request.Pending`, built from the options a
  token-loading command would use (`--url`, `--key-request-url` and their env vars); the module's own pending-file
  reader is gone, so nothing in pwrwrdn knows that file's format. Pending line: `none`; `service <host>, code <code>,
  expires <RFC3339 UTC>`; `expired`; `filed for another service`; an unusable file is keyclient's error, exit 1; a
  pending file with no powerwarden URL configured says it cannot be checked without one, exit 1.
- The module requires `keyclient v0.1.2`, which also fixes the test provider's race, so the test workaround is gone.
  README and DESIGN doc updated.
- Release: `powerwarden/mcp/v0.2.0` goes on the commit that carries this entry (tag to be created after review).
- Commits: the commit on branch `b8-status` that carries this entry.
