# Command-line convention

`pwrwrdn` follows the convention below. Its own codes and output shapes follow it, under
[pwrwrdn](#pwrwrdn).

## Command-line tools

A command-line tool is run by people and by agents: programs that run its commands and read what they print. This
section is the convention such a tool follows, so that both can use it without guessing.

### Output with `--json`

- `--json` on every non-interactive command prints exactly one JSON object on stdout, never a bare array. Lists
  are under a named key, for example `{"items": [...]}`. Human text and progress go to stderr.
- With `--json`, an error is one object on stderr, `{"error":{"code":"...","message":"..."}}`. The exit code is
  non-zero.
- Fields are only added within a major version. Renaming or removing one is a breaking change, recorded in the
  tool's log.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Error. |
| `2` | Usage: a bad command, flag or argument. |
| `75` | Try again later, for example while a key request is pending. |

A command that runs another program exits with that program's code, says so in its help, and with `--json` also
reports `exit_code`.

### Help

- `-h` and `--help` print to stdout and exit `0`.
- Help has usage, a description, flags and at least one example.
- The top-level help has an "Agent tips" section:
  - use `--json`;
  - which commands block, and for how long;
  - how keys are obtained;
  - which commands are interactive.
- A command adds its own tip where it has one.

### Interactive commands

Interactive commands (a TUI, a shell, a tunnel, a server) are exempt. With `--json` they exit `2` with
"interactive command; no --json".

### Prompts and fixes

- Nothing prompts when stdin is not a terminal.
- An error that has a fix names the command to run.

### Conformance

A test walks the tool's command tree and checks every command. For each non-exempt command, `--json` is accepted
and the help carries the required parts.

A tool restates this section verbatim in its own `docs/CLI.md`.

## pwrwrdn

The conformance test is `powerwarden/cli/cmd/pwrwrdn/tree_test.go`. It walks the command table in `main.go`.

### Commands

| Command | `--json` prints (stdout) |
|---|---|
| `claim` | the API's `Claim` object (`id`, `state`, `detail`, `mode`, `hostname`, `host_state`, `expires_at`, `lease_until`, ...) |
| `extend` | the API's `Claim` object |
| `release` | `{"id","released"}` |
| `power` | `{"claim_id","action","already","command"}`; `command` is the API's `Command` or `null`. With `--wait-on`, also `hostname` and `host_state` |
| `hosts` | `{"hosts":[HostView...]}` |
| `claims` | `{"claims":[Claim...]}` |
| `key request` | `{"token_file","result"}`, `result` `already_present` or `stored`; with `stored`, also `name` and `role` |
| `key status` | `{"token_file","token","pending_file","pending"}`; `token` is `present` or `absent`; `pending` is `{"state"}`, `state` `none`, `pending`, `expired` or `foreign`; with `pending`, also `service`, `code` and `expires_at` |
| `setup-token` (deprecated) | `{"deprecated":true,"name","token_file"}`; with `--url`, also `url` and `url_file` |
| `run` | a wrapper: see below |

No `pwrwrdn` command is interactive; every one takes `--json`.

`run` hands stdout to the command it runs. With `--json` it writes one object on stderr,
`{"claim_id","host","mode"}`, before the command starts. When the command ends it writes
`{"claim_id","exit_code"}` on stderr. `pwrwrdn run` exits with the command's exit code, or `128+N` if the
command died of signal `N`. The command's own stderr comes between the two objects. The progress lines are left
out. A lost-claim warning is still a text line.

### Error codes

The `code` of the error object:

| Code | Exit | When |
|---|---|---|
| `usage` | `2` | a bad command, flag or argument |
| `key_pending` | `75` | a key request was filed and waits for approval; the message holds the approval address and the code to check |
| `key_busy` | `75` | another `pwrwrdn` is handling the key request for the same token file |
| `key_denied`, `key_expired`, `key_consumed`, `key_unauthorized`, `key_provider` | `1` | the key request ended without a key |
| `no_key`, `no_provider` | `1` | no token file, and no key provider to ask |
| `config` | `1` | a setting is missing or unusable, for example no powerwarden URL |
| `token_file`, `pending_file` | `1` | the token file or the pending request file cannot be used |
| `claim_failed` | `1` | the claim ended `failed` |
| `timeout` | `1` | `power --wait-on` ran out |
| `interrupted` | `128+N` | `run` got signal `N` while waiting for its claim |
| `exec` | `126`, `127` | `run` could not start its command |
| `exists`, `not_found`, `bao` | `1` | `setup-token`: a file exists, OpenBao has no such token, or the `bao` CLI failed |
| the server's code | `1` | powerwarden refused the call, for example `not_found`, `host_shared` or `claim_failed`; `http_<status>` when the answer has no code |
| `error` | `1` | anything else |

Without `--json`, errors are text on stderr as before: `pwrwrdn: <message>`.
