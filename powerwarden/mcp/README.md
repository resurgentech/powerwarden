# pwrwrdn

`pwrwrdn` is the agent-side CLI and MCP server for powerwarden. It lets a
person, an AI agent or a script claim a lab host, power it, and give it back.

One binary:

- `pwrwrdn mcp` is an MCP server on stdio. One process per agent session.
- the other subcommands are a plain CLI for scripts and for humans.

Claims, modes and the server's rules are described in
[`docs/DESIGN-powerwarden.md`](../../docs/DESIGN-powerwarden.md#claims);
the API is `specs/openapi/powerwarden.yml`.

## Install

```
just install
```

This runs `go install ./cmd/pwrwrdn`, which puts `pwrwrdn` in `$GOBIN`
(default `~/go/bin`). Make sure that directory is on your `PATH`.

## Get a token

Every user has a token named `<user>@<machine>` with role `agent`. The token
lives in OpenBao; you fetch it once per machine.

1. The admin mints a token for `NAME` (`<user>@<machine>`), makes the
   powerwarden server accept it, and stores it in OpenBao under field `NAME`
   at `<mount>/<path>`. Where that is depends on your deployment; the admin
   tells you the path.

2. You, logged in to OpenBao as usual (`bao login`, your normal env and token
   helper):

   ```
   export PWRWRDN_BAO_PATH=path/to/user_tokens   # placeholder: ask your admin
   pwrwrdn setup-token [--name NAME] [--force]
   ```

   The OpenBao location is a setting, with no built-in default for the path:
   `--bao-path` (env `PWRWRDN_BAO_PATH`, required) and `--bao-mount` (env
   `PWRWRDN_BAO_MOUNT`, default `kv`). Flags win over the environment.
   It runs `bao kv get -mount=<mount> -field=NAME <path>`
   and writes `~/.config/powerwarden/token` atomically (directory 0700, file
   0600). `NAME` defaults to `$USER@<short hostname>`. It will not overwrite an
   existing file without `--force`. If OpenBao has no token under that name it
   tells you what to ask the admin for. `pwrwrdn` refuses to use a token file
   that group or other can read.

The server URL has no built-in default. Every subcommand (including `mcp`)
takes it from, in order: `--url`, `POWERWARDEN_URL`, then the one-line file
`~/.config/powerwarden/url` (next to the token; whitespace is trimmed):

```bash
echo https://powerwarden.example.com > ~/.config/powerwarden/url
```

`setup-token --url https://powerwarden.example.com` writes that file (mode 0600,
beside the token file) and, like the token, refuses to replace an existing one
without `--force`. With none of the three set, `pwrwrdn` exits with
`powerwarden URL not set: use --url, POWERWARDEN_URL, or write it to
~/.config/powerwarden/url`.

Other config, optional: `POWERWARDEN_CA_FILE` (an extra CA for that server),
`POWERWARDEN_TOKEN_FILE` (or `--token-file`) instead of
`~/.config/powerwarden/token`.

## MCP

Add this to `.mcp.json`. There is no token and no env in it; the server reads
the token file.

```json
{
  "mcpServers": {
    "powerwarden": { "command": "pwrwrdn", "args": ["mcp"] }
  }
}
```

Tools:

| tool | what it does |
|------|--------------|
| `list_hosts` | hosts with explicit tags, name tags, power state, `jobs_blocked`, claims; optional `tags` filter |
| `claim_host` | claim one host by tags (`tags`, `purpose`, `max_hours`, `mode`); returns at once, never waits for a boot |
| `wait_for_claim` | block about 60s until the claim changes; call until `state` is `ready` |
| `extend_claim` | set the hard cap to now plus `hours` |
| `release_host` | release a claim, and dismiss a lost-claim warning |
| `power_host` | `powerup`, `powerdown`, `powercut` or `reboot` for the claim's host |
| `my_claims` | claims owned by your token, from any session |

The MCP server renews the leases of the claims it made, releases them when the
agent session ends, and never makes a detached claim.

## Commands

All of them take `--token-file`. A claim id made by the CLI looks like
`pwrwrdn-<12 hex>`. Durations are `90m`, `2h`, `1d`, `1d12h`.

### `pwrwrdn run [--mode M] --tags a,b [--purpose P] [--max D] [--on] -- <cmd>`

Claims a host, runs `<cmd>` with `POWERWARDEN_CLAIM_ID` and `POWERWARDEN_HOST`
(the bound hostname) in its environment, releases the claim when the command
ends, and exits with the command's exit code (128+N if it died of signal N).

- The claim is leased, never detached, and `pwrwrdn` renews it while the
  command runs. `--max` sets the hard cap. `--purpose` defaults to
  `pwrwrdn run: <argv0>`.
- It blocks until the claim is `ready`, however long that takes (the claim
  stays `pending` while the host drains), using the blocking read. It exits
  non-zero if the claim `failed`.
- `--on` is for a `dedicated` claim: powerwarden never boots a dedicated host,
  so `--on` sends `powerup` and waits until `host_state` is `on` before running
  the command. **Without `--on`, a dedicated claim on an off host runs the
  command as soon as the claim is ready, with the host still off.**
- SIGTERM, SIGINT and SIGHUP are forwarded to the command; the claim is
  released after it exits. If the claim is lost (renewal refused), a warning
  goes to stderr.
- It does not need MCP, so a job can use it, for example an Ansible task that
  updates software and reboots the host with `pwrwrdn power reboot --wait-on 10m`
  inside the `run`.

### `pwrwrdn claim [--mode M] --tags a,b [--purpose P] [--max D] [--wait]`

Makes a **detached** claim and prints its id, bound host (or `pending`) and
`expires_at`, then exits. `--purpose` defaults to `pwrwrdn claim by <owner>`.
`--wait` blocks until `ready` first and exits non-zero on `failed`.

A detached claim has no liveness lease. It lives until its hard cap or
`pwrwrdn release`, so it can outlive whatever you wanted it for. Release it.

### `pwrwrdn release <id>` and `pwrwrdn extend <id> <duration>`

`release` deletes a claim. `extend` sets the hard cap to now plus the
duration (the server clamps it).

### `pwrwrdn power [<id>] <action> [--wait-on D]`

Sends `powerup`, `powerdown`, `powercut` or `reboot` for the claim's host. The
id defaults to `$POWERWARDEN_CLAIM_ID`, which `pwrwrdn run` sets. It prints the
command, or exits non-zero printing the refusal code (for example
`host_shared`). `--wait-on D` then waits up to `D` for `host_state` `on`.

### `pwrwrdn hosts [--tags a,b]`

A table: host, explicit tags, name tags, state, whether jobs are blocked, and
claims (owner, mode, time left to the cap).

### `pwrwrdn claims [--mine]`

A table: id, owner, mode, host, state and detail, `detached` or lease left,
and cap left.

## Modes

- `hold` (default): others may hold or reserve the same host; hourchd jobs
  still run; the host is kept on.
- `reserve`: like `hold`, but hourchd jobs are blocked on the host.
- `dedicated`: you have the host alone, jobs are blocked, and you control its
  power; powerwarden takes no power action on it. `ready` does not mean on:
  read `host_state`, or use `run --on`.

## Name tags

Hosts are chosen by tags. A host's tags are its explicit tags plus its name
tags: its full hostname (`host01.example.com`) and its short name (`host01`). To get one
particular machine, claim by its name tag: `--tags host01`.

## Lifetimes

- Lease: 15 minutes. A leased claim not renewed in that time is dropped.
- Renewal: every 5 minutes (a third of the lease), by `pwrwrdn run` and
  `pwrwrdn mcp`.
- Hard cap: 8 hours by default, 15 minutes to 72 hours at the server. Only
  `extend` moves it; renewing does not.
- Release on exit: `run` releases when the command ends or on a signal; `mcp`
  releases when the agent session ends.
- Detached claims (`pwrwrdn claim`, and the web UI) have no lease: they last
  until the cap or a release. `mcp` and `run` never make one.

## Develop

```
just test      # unit tests: httptest fakes, no network
just vet
```
