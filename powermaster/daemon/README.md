# powermaster

Host power control: inventory (Vault), hardware polling, the host state
machine and the `powerup` / `powerdown` / `powercut` / `reboot` sequences,
behind the HTTP API in `specs/openapi/powermaster.yml`.
Design: [`docs/DESIGN-powermaster.md`](../../docs/DESIGN-powermaster.md).

Powermaster is the only process holding BMC, PDU and SSH credentials. State
and commands live in SQLite (`state_path`); poll results live in memory.

## Layout

| Path | What |
|---|---|
| `config.go`, `config.yaml.example` | Config, classes, resolved budgets |
| `manager.go`, `sequence.go`, `poller.go`, `recovery.go`, `view.go` | Per-host goroutines, sequences, pollers, recovery, host views |
| `inventory/` | Vault inventory (`locked`), `Static` source for tests |
| `hw/` | `IPMI`, `PDU`, `Net` interfaces, real implementations, `FakeLab` |
| `store/` | SQLite: `hosts`, `commands`, migrations |
| `internal/events` | Event hub (seq + ring) behind `/v1/events` |
| `internal/httpapi` | `net/http` handlers using `powermasterapi` types; bearer tokens |
| `internal/tui` | `powermaster monitor` |
| `cmd/powermaster` | `serve`, `check` and `monitor`; Dockerfile |

## Build and test

From this directory:

```bash
just test     # gate (fakes only: no network, Vault or hardware)
just vet
just build    # binary at cmd/powermaster/powermaster
```

## Configure

`/etc/powermaster/config.yaml` (`serve --config`). Every key and its default is
in [`config.yaml.example`](config.yaml.example); unknown keys are a startup
error. Durations use Go syntax (`30s`, `5m`).

- `vault.hosts_secret_path` is **required** for `serve` and `check`: the KV v2
  path (mount `kv`) of the inventory secret. There is no default. The secret's
  shape is in the design doc.
- Vault address, token and skip-verify come from the environment variables
  named under `vault:` (default `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_SKIP_VERIFY`).
- Bearer tokens are in `tokens_file` (YAML `name: token`, mode 0600). The
  token name becomes `requested_by`. No tokens means no start (fail closed).
- `classes` group hosts and PDUs and override stage `timings`; a host's class
  overrides the defaults, and for `pdu_outlet_wait` the PDU's class wins.
  `GET /v1/hosts/{hostname}` shows the resolved budgets.
- `personal_keys.request_url` is optional (off by default): an absolute `http`
  or `https` URL where a person requests a key for this service, advertised by
  the unauthenticated `GET /v1/key-request`. Unset, that route is 404.

Secrets never go in `config.yaml`. Check a config against the live inventory
before deploying:

```bash
just check /path/to/config.yaml   # needs the Vault environment; non-zero on error
```

With `personal_keys.request_url` set, the `ok:` line ends with ` key_request=on`.

## Build the image

```bash
just image [tag]   # default tag: latest
just push [tag]    # needs IMAGE_REGISTRY
```

The build context is the repository root. The recipes read these settings from
the environment, or from a gitignored `images.env` found by searching upward
from this directory (template: `images.env.example` at the repository root):

| Setting | Effect |
|---|---|
| `IMAGE_REGISTRY` | Registry prefix, e.g. `registry.example.com`. Empty: local tag `powermaster:<tag>`, and `push` refuses |
| `IMAGE_CA_CERT` | Absolute path to an extra CA (PEM) to trust inside the image, e.g. your Vault's CA. Empty: system CAs only |
| `LOGS_HOST` | Host for `just logs [host]` (SSH, then `docker compose logs -f` in `$LOGS_DIR/powermaster`). No default |
| `LOGS_DIR` | Directory on `LOGS_HOST` that holds the compose projects; `just logs` runs in `$LOGS_DIR/powermaster`. No default: `just logs` fails when it is unset |

## Run

Run the image under any orchestrator; exactly one instance per fleet. It needs:

- `config.yaml` at `/etc/powermaster/config.yaml` and `tokens.yaml` (0600);
- the Vault environment variables;
- a volume at `/var/lib/powermaster` on local disk;
- `CAP_NET_RAW` (ping) and network reach to the hosts, BMCs and PDUs;
- a TLS reverse proxy in front if callers are off-host (it serves plain HTTP on `:8092`).

A compose example and post-deploy checks are in the design doc.

## Operate

```bash
PM=https://powermaster.example.com
curl -s $PM/v1/healthz        # no auth; so is GET /v1/key-request (404 unless configured)
curl -s -H "Authorization: Bearer $TOKEN" $PM/v1/hosts
curl -s -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"action":"reboot","reason":"manual: hung"}' \
  $PM/v1/hosts/host01.example.com/power
curl -sN -H "Authorization: Bearer $TOKEN" $PM/v1/events
```

TUI: `just monitor` (or `powermaster monitor --api-url URL --token-file FILE`).
The API URL has no built-in default; it comes from `--api-url`, then `tui.api_url`
in `--config`, then `POWERMASTER_URL`, then the one-line file
`~/.config/powermaster/url` (beside `~/.config/powermaster/token`).
Keys: `u` powerup, `d` powerdown (on a `soft_off` host, powercut), `t` toggle, `r` reboot. On a locked host the
fourth `t` or `r` tap within `tui.locked_tap_gap` of each other sends with
`force=true` after `tui.locked_force_delay`.
