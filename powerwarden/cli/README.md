# pwrwrdn

`pwrwrdn` is the CLI for powerwarden's claim API. It lets a person, an AI agent
or a script claim a lab host, power it, and give it back. The CLI is the
agent-facing tool here: agents run it with `--json` (see [Commands](#commands)).
An MCP front end, if you want one, is outside this repo.

Claims, modes and the server's rules are described in
[`docs/DESIGN-powerwarden.md`](../../docs/DESIGN-powerwarden.md#claims);
the API is `specs/openapi/powerwarden.yml`.

## Install

```
just install
```

This runs `go install ./cmd/pwrwrdn`, which puts `pwrwrdn` in `$GOBIN`
(default `~/go/bin`). Make sure that directory is on your `PATH`.

## Get a key

The first time you use `pwrwrdn` on a machine with no token file, it asks for a
key. You do not run anything extra:

1. Run any command, for example `pwrwrdn hosts`. It files a key request with the
   key provider the powerwarden server points to (discovery), or the one you
   name with `--key-request-url` or `POWERWARDEN_KEY_REQUEST_URL`. The request
   asks for role `agent` (change it with `--role`), with the name hint
   `<user>@<short hostname>` and tool `pwrwrdn/<version>`.
2. It prints one line on stderr: approve the request at the address shown, and
   check that the code shown there matches the code in the line.
   `pwrwrdn` never opens the address for you.
3. In a terminal the command waits for the approval and then goes on. Without
   a terminal (or with `--no-key-request`) it prints the line and the message
   `key request pending; run this again after approving`, then exits with code
   **75**. Approve, then run the command again: it collects the key and goes on.

The key is stored at `~/.config/powerwarden/token` (or `--token-file` /
`POWERWARDEN_TOKEN_FILE`), mode 0600. The first call made with a key received
this way is sent once more, after 5 seconds, if the service answers `401`
(a new key can take a moment to reach the service).

Exit codes of a command that asked for a key: `75` request pending, or another
`pwrwrdn` is handling the request for the same token file (try again shortly);
`1` for a request that was denied, expired or already handed out (run it again
to file a new request), or refused by the provider (`try again in <n>s` when it
says when); `0` and the command's own result once a key is in place. If the
server has no key provider and you gave no key request URL, the error is the
same "no token" error as before.

Two commands help:

```
pwrwrdn key request [--role R] [--no-key-request]   # ask for a key now, without another command
pwrwrdn key status                                  # token file present or absent; the pending request's service, code and expiry
```

`key status` reads files only: no network, no lock, and it never shows the poll
secret. It asks keyclient (`request.Pending`) about the request your current
settings (`--url`, `--key-request-url` and their env vars) would resume, and
prints one of:

```
token    <path>: present | absent
pending  <path>.pending: none
pending  <path>.pending: service <host>, code <code>, expires <UTC time>
pending  <path>.pending: expired
pending  <path>.pending: filed for another service
```

A pending file keyclient cannot use is an error (exit 1) that names the file
and says to remove it. With a pending file but no powerwarden URL configured,
it says it cannot check the request without one (exit 1).
The contract and state machines are in the keyclient repo's `docs/CONTRACT.md`
and `docs/STATES.md`.

Flags (every command that loads a token, and `key request`):
`--key-request-url URL` (env `POWERWARDEN_KEY_REQUEST_URL`; the flag
wins), `--role ROLE` (default `agent`), `--no-key-request`.

### Deprecated: setup-token

`pwrwrdn setup-token` still works and prints a deprecation notice. Every user
has a token named `<user>@<machine>` with role `agent`, which lives in
OpenBao; you fetch it once per machine.

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

The server URL has no built-in default. Every subcommand takes it from, in order: `--url`, `POWERWARDEN_URL`, then the one-line file
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

## Commands

All of them take `--token-file`. A claim id made by the CLI looks like
`pwrwrdn-<12 hex>`. Durations are `90m`, `2h`, `1d`, `1d12h`.

`pwrwrdn` follows the command-line convention in
[`docs/CLI.md`](../../docs/CLI.md), which also lists its JSON shapes and error
codes:

- `pwrwrdn --help`, `-h`, and `pwrwrdn <command> --help` print to stdout and
  exit 0. The top-level help has "Agent tips".
- `--json` on every command prints one JSON object on stdout:
  `claim` and `extend` print the API's `Claim`, `hosts` prints `{"hosts":[...]}`,
  `claims` prints `{"claims":[...]}`. With `--json` an error is one object on
  stderr, `{"error":{"code":"...","message":"..."}}`; a pending key request is
  code `key_pending`, exit 75.
- `run --json` keeps stdout for the command. It writes `{"claim_id","host","mode"}`
  on stderr before the command starts and `{"claim_id","exit_code"}` after it ends.
- No command is interactive.
- Exit codes: `0` success, `1` error, `2` usage (a bad command, flag or
  argument), `75` try again later; `run` exits with its command's code.

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
- It needs nothing but the CLI, so a job can use it, for example an Ansible task that
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
- Renewal: every 5 minutes (a third of the lease), by `pwrwrdn run`.
- Hard cap: 8 hours by default, 15 minutes to 72 hours at the server. Only
  `extend` moves it; renewing does not.
- Release on exit: `run` releases when the command ends or on a signal.
- Detached claims (`pwrwrdn claim`, and the web UI) have no lease: they last
  until the cap or a release. `run` never makes one.

## Develop

```
just test      # unit tests: httptest fakes, no network
just vet
```
