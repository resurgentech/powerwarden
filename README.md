# powerwarden

Two small services that switch a fleet of lab hosts on and off, plus a client for people and agents.

- **powermaster** drives the hardware. It reads the host and PDU inventory from Vault, polls IPMI, PDUs, ping
  and SSH, runs the `powerup` / `powerdown` / `powercut` / `reboot` sequences, and serves an HTTP API with the
  host view and a command queue. It is the only process that holds BMC, PDU and SSH credentials.
- **powerwarden** decides when hosts should be on. Schedulers register *demands* (work that needs a host with
  some tags), workers report liveness, and people or agents take *claims* on hosts. powerwarden reconciles
  those against powermaster's host view and asks powermaster to power hosts up, down or off. It holds no
  hardware credentials, and serves its own API and a web UI.
- **pwrwrdn** is the CLI for powerwarden's claim API: claim a host, power it, give it back. It is the
  agent-facing tool here (agents run it with `--json`); an MCP front end is outside this repo.

```
scheduler / workers ──demands, liveness──▶ powerwarden ──power commands──▶ powermaster ──IPMI, PDU, SSH──▶ hosts
people / agents ──pwrwrdn (claims)──────▶      ▲                              │
                                               └──────── host view, events ───┘
```

Design: [`docs/DESIGN-powermaster.md`](docs/DESIGN-powermaster.md),
[`docs/DESIGN-powerwarden.md`](docs/DESIGN-powerwarden.md).

## Layout

| Path | Module | What |
|---|---|---|
| `specs/openapi/powermaster.yml`, `specs/openapi/powerwarden.yml` | | API contracts (source of truth) |
| `powermaster/api` | `github.com/resurgentech/powerwarden/powermaster/api` | Generated Go client and types (`package powermasterapi`) |
| `powermaster/daemon` | `github.com/resurgentech/powerwarden/powermaster/daemon` | The powermaster service and its TUI ([README](powermaster/daemon/README.md)) |
| `powerwarden/api` | `github.com/resurgentech/powerwarden/powerwarden/api` | Generated Go client and types (`package powerwardenapi`) |
| `powerwarden/daemon` | `github.com/resurgentech/powerwarden/powerwarden/daemon` | The powerwarden service and web UI ([README](powerwarden/daemon/README.md)) |
| `powerwarden/mcp` | `github.com/resurgentech/powerwarden/powerwarden/mcp` | The `pwrwrdn` CLI ([README](powerwarden/mcp/README.md)) |
| `docs/` | | Design docs, work log |
| `go.work` | | Joins the five modules for in-repo work |

## Build and test

Needs Go 1.25 or newer and [`just`](https://github.com/casey/just). Each module has its own `justfile`; run
recipes from the module directory (or `just --justfile <dir>/justfile -d <dir> <recipe>` from the root).

```sh
cd powermaster/daemon && just test && just vet     # likewise powerwarden/daemon, powerwarden/mcp
cd powermaster/api && just test && just drift      # likewise powerwarden/api
cd powerwarden/mcp && just install                 # pwrwrdn into $GOBIN
```

`just generate` in an `api/` module regenerates the client from its spec; `just drift` fails when the committed
client is stale. Change the spec first, regenerate, then the handlers, clients and tests together.

## Images

`just image [tag]` in `powermaster/daemon` or `powerwarden/daemon` builds the container image with the repo root as
context (the Dockerfiles copy only `go.work`, `go.work.sum`, `powermaster/`, `powerwarden/` and `specs/`).
`just push [tag]` pushes it. Settings come from the environment or a gitignored `images.env` at the repo root;
copy [`images.env.example`](images.env.example):

| Setting | Effect |
|---|---|
| `IMAGE_REGISTRY` | Registry prefix, e.g. `registry.example.com`. Empty: local tag `<name>:<tag>`, and `push` refuses |
| `IMAGE_CA_CERT` | Path to an extra CA (PEM) to trust inside the image. Empty: system CAs only |
| `LOGS_HOST` | Host for `just logs [host]` (SSH, then `docker compose logs -f`). No default |
| `LOGS_DIR` | Directory on `LOGS_HOST` holding the compose projects; `just logs` runs in `$LOGS_DIR/<service>`. No default |

How to configure and run each service: the package READMEs and the design docs.

## Development notes

- **Versions.** Each module is versioned on its own with a directory-prefixed tag, e.g. `powerwarden/api/v0.1.0`.
  Stay on v0.x.
- **Intra-repo requires and `go.work`.** The daemons and `pwrwrdn` `require` the `api` modules at a tag
  (`v0.1.0`), so `go install <module>@<tag>` and builds with `GOWORK=off` resolve real versions. Go reads the
  `go.mod` of the required version even inside a workspace, so `go.work` also carries one *versioned* `replace` per
  intra-repo require, pointing that version at the local copy; in-repo work never needs a published tag.
  **When an intra-repo `require` moves to a new version, the matching `go.work` replace moves with it.**
  `go.mod` files carry no `replace` lines.
- **Releasing an api change**: change the spec, `just generate`, tag `<dir>/vX.Y.Z`, push the tag, then bump the
  `require` in the dependent modules and the `replace` in `go.work` in the same commit.
- **No site values in code.** Hosts, registries and paths of a particular deployment live in config files,
  `images.env` or the `*.example` files.

## License

MIT, see [`LICENSE`](LICENSE).
