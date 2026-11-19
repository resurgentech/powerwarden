# powerwarden

Demand-driven host power policy. Design: [`docs/DESIGN-powerwarden.md`](../../docs/DESIGN-powerwarden.md).
It serves the demand, worker-liveness and claim API (`specs/openapi/powerwarden.yml`)
and a web UI. It reconciles demand and claims against powermaster's host view, and asks
powermaster to power hosts up, down or off at the PDU. It holds no hardware credentials.

## Layout

| Package | Role |
|---|---|
| `internal/config` | `config.yaml`; unknown keys are an error |
| `internal/store` | SQLite: `demands`, `workers`, `host_policy`, `settings`, `claims`; claim binding |
| `internal/reconcile` | `Covers`, name tags, the pure `Plan` (11 ordered rules), executor, event loop plus timer |
| `internal/pm` | powermaster `Client` interface, `HTTP` over `powermasterapi`, test `Fake` |
| `internal/hostview` | cached powermaster host view, folding in events |
| `internal/source` | loads demand and liveness rows (written through the API) for each pass |
| `internal/decisions` | in-memory ring served by `/v1/decisions` |
| `internal/events` | SSE hub for `/v1/events` and `/ui/events` |
| `internal/app` | operations shared by the API and the UI |
| `internal/httpapi` | `/v1` handlers (generated `powerwardenapi` types), named bearer tokens |
| `internal/ui` | `/ui`: `html/template`, `EventSource` script, `go:embed`, htpasswd basic auth |

## Build and test

From this directory:

```sh
just test                 # unit tests, no docker
just vet
just build                # binary at cmd/powerwarden/powerwarden
./cmd/powerwarden/powerwarden check-config --config /path/to/config.yaml --tokens-file /path/to/tokens.yaml
./cmd/powerwarden/powerwarden serve --config /path/to/config.yaml
```

## Configure

`/etc/powerwarden/config.yaml` (`serve --config`). Every key and its default is in
[`config.yaml.example`](config.yaml.example); unknown keys are a startup error.

- `powermaster.url` is **required** (no default): the base URL of powermaster,
  e.g. `http://powermaster:8092` beside it or `https://powermaster.example.com`.
- `host_tags` optionally seeds host capability tags; after that they live in the
  database and are edited through the API or UI.

`check-config` loads the config, then the tokens file with the loader `serve` uses. `--tokens-file` overrides `tokens_file`, so a rendered config and tokens pair can be checked off-box. A missing or invalid tokens file exits 1. On success it prints `ok: listen=… powermaster=… tokens=N (full=a, agent=b)`. It never prints a token value.

`serve` needs these files:

- `tokens_file`: YAML of `name: token`, mode 0600. Tokens must be at least 16 characters. Give one to the scheduler (hourchd) and one to each operator. Never put one on a worker host.
  An entry is a string (role `full`) or a map `{token: "...", role: full|agent}`; unknown keys and unknown roles fail at startup. Names match `^[A-Za-z0-9_.@-]{1,64}$`, so a user token is `<user>@<machine>`. A role comes only from the entry, never from the name.
  - `full` (scheduler, operators): every operation.
  - `agent` (user tokens): every `GET` (including `/v1/events`) and `PUT`/`POST`/`DELETE` under `/v1/claims*`. It gets 403 `forbidden` on `PUT`/`DELETE /v1/demands*`, `PUT`/`DELETE /v1/workers*`, `PUT /v1/hosts/{h}/tags` and `PUT /v1/hosts/{h}/exclude`, and cannot release another owner's claim. Enforced in `internal/httpapi/tokens.go` (`routeRoles`).
- `powermaster.token_file`: powerwarden's token for powermaster.
- `ui.htpasswd_file`: when `ui.enabled`. bcrypt entries only (`htpasswd -B`).
- `ui.path_prefix`: default empty. Set (e.g. `/powerwarden`) when a reverse proxy serves the UI under a path prefix it does not strip; the UI is then at `<prefix>/ui`.

## Auth

- `/v1/*` uses `Authorization: Bearer <token>`, except `GET /v1/healthz`.
- `/ui/*` uses htpasswd basic auth. The browser's `EventSource` cannot send a bearer header, so the UI's live feed is `/ui/events`: the same hub as `/v1/events`, behind basic auth.
- UI POSTs must carry an `Origin` (or `Referer`) whose host equals the request `Host`. This is the CSRF guard, so a reverse proxy in front must preserve `Host`.
- The UI's writes are the exclude toggle, host tags, manual demands and detached claims (create, release). It has no power buttons; manual power stays in `powermaster monitor`.

## Build the image

```sh
just image [tag]   # default tag: latest
just push [tag]    # needs IMAGE_REGISTRY
```

The build context is the repository root. The recipes read these settings from
the environment, or from a gitignored `images.env` found by searching upward
from this directory (template: `images.env.example` at the repository root):

| Setting | Effect |
|---|---|
| `IMAGE_REGISTRY` | Registry prefix, e.g. `registry.example.com`. Empty: local tag `powerwarden:<tag>`, and `push` refuses |
| `IMAGE_CA_CERT` | Absolute path to an extra CA (PEM) to trust inside the image, e.g. the CA behind powermaster's TLS. Empty: system CAs only |
| `LOGS_HOST` | Host for `just logs [host]` (SSH, then `docker compose logs -f` in `$LOGS_DIR/powerwarden`). No default |
| `LOGS_DIR` | Directory on `LOGS_HOST` that holds the compose projects; `just logs` runs in `$LOGS_DIR/powerwarden`. No default: `just logs` fails when it is unset |

## Run

Run the image under any orchestrator; exactly one instance per fleet. Mount
`config.yaml`, `tokens.yaml`, `powermaster.token` and `ui.htpasswd` under
`/etc/powerwarden/`, and a volume at `/var/lib/powerwarden` on local disk. It
serves plain HTTP on `:8091`; put a TLS proxy in front. It needs network reach to
powermaster and nothing else (no Vault, no hardware).

A compose example and post-deploy checks are in the design doc.

Demand and worker liveness come only from the API: the scheduler publishes them,
and operators can add manual demands. The keys `demand_source` and `redis` are
rejected at startup.
