# `aiq __app` bridge contract, schema 1

These files are the contract between the Go bridge (`cmd/aiq/app_*.go`) and the native Herdr app. `TestAppGolden` writes them from invented data:

```
go test ./cmd/aiq -run TestAppGolden -update
```

Each file is a reply, indented for reading. The bridge writes the same document on one line. All values are invented.

## Commands

```
aiq __app hello
aiq __app asks
aiq __app ask-detail --id N
aiq __app ask-action --id N --revision HEX64 --action dismiss|restore --host HOSTID
aiq __app target     --ask N --revision HEX64
aiq __app target     --provider claude|codex|agy --conversation ID
aiq __app quotas
```

Planned for the next slices: `sessions`, `conversation`, `ask-context`.

Arguments come only as `--name value` pairs, each at most once. Values are enums, decimal ids, lowercase hex, or conversation ids. No paths or free text are accepted. Anything else is `invalid_request`.

## Process contract

- stdout is exactly one complete JSON document of at most 4 MiB (4194304 bytes) followed by a newline. The bridge measures before writing and never writes a cut document.
  - `hc exec` silently cuts stdout at 8 MiB with exit 0, so the app must also treat unparseable output as `internal`.
- Exit 0 for every handled outcome, including `ok:false`. A nonzero exit means a crash or an aiq without `__app`: an old aiq prints usage and exits 2. stderr is never part of the contract.
- Deadlines: reads 8 s, `ask-action` 5 s.
- The bridge reads this machine only. It never starts the `[fleet]` SSH poller. The app asks each Herdr machine itself.

## Envelope

| field | meaning |
|---|---|
| `schema` | 1 |
| `ok` | false: `error.code` is set and there is no `data`, `partial` or `omitted` |
| `kind` | the command (`""` when the command itself is unknown) |
| `aiq` | aiq version |
| `host.id` | 32 hex, this installation's source ID: a hash of the machine identity (machine ID or platform UUID first) and the canonical aiq data directory. Empty when the identity is unavailable; no action accepts an empty ID. |
| `host.name` | hostname, display only |
| `generated_at_ms` | unix ms |
| `partial` | rows or facts are missing; never read missing as zero |
| `omitted` | counts of rows left out, by reason (`rows`) |
| `error.code` | `busy` `unavailable` `not_set_up` `too_large` `invalid_request` `not_found` `conflict` `unknown_outcome` `internal` |

## hello (`hello.json`)

`setup.asks` / `setup.quotas` is `ready` or `not_set_up`. `commands` lists what this aiq answers.

## asks (`asks.json`, `asks.not_set_up.json`)

- `setup`: `not_set_up` is never zero asks; then `counts.known` and `watcher.known` are false.
- `counts` are exact per section over the whole store, even when rows are omitted. Sections are `asks`, `warnings`, `other_open`, `probably_done` and `closed_dismissed`.
- Rows come in section order, then newest raised first, as a prefix that fits. `omitted.rows` counts the rest.
- `first` / `latest` give `preview` (at most 280 characters), `bytes` (full UTF-8 size) and `truncated`. Use `ask-detail` for the full text.
- `session.name_source` is `none` for now. No transcript is parsed on this poll.
- `terminal` is null or `{relation, address, core_id, witness:{terminal_pid, label}}`, verified when the reply was built. `terminal_reason` explains a null.

## ask-detail (`ask-detail.json`)

The full first and latest question text from the asks store, each capped at 256 KiB (`truncated`). No transcript is read. Unknown id: `not_found`.

## ask-action (`ask-action.ok.json`, `ask-action.conflict.json`, `error.invalid_request.json`)

- The action applies only if `--revision` matches and `--host` equals this bridge's `host.id`. Otherwise the reply is `invalid_request`.
- Success returns the new `state` and `revision`.
- `conflict`: the ask changed. A replay of an action that already applied is also a conflict, never a second write.
- `unknown_outcome`, or no reply at all: the app reads `asks` once and reports applied, not applied, or changed. It never retries on its own.

## target (`target.json`)

A fresh resolution, read when the person clicks. The app never navigates from a cached target.

- `--ask/--revision` gives the ask's own conversation (`relation:"origin"`). Failing any lease for that, it gives the one long session in the ask's workspace (`workspace_current`). A different revision is `conflict`.
- `--provider/--conversation` gives that conversation only.

The bridge exports a terminal only after these checks:
1. The lease is the conversation's single top-level long lease, on a Herdr terminal.
2. Its child is observed Running.
3. The child's pinned core, read-only, still has the same core ID, a live terminal at the recorded address, the recorded root pid, and the witness label `aiq.target`. The runner writes that label at each child's registration; it is generation-specific.

The app then requires `hc get <machine/address> --json` to show `state:"live"`, `pid == terminal_pid` and `labels["aiq.target"] == label` before it focuses. A kept replacement shell keeps the label but fails the pid check.

Interactive sessions (`aiq claude` and the like, not `aiq long`) are targets too, with fewer witnesses:
- At launch, aiq records the terminal only when the current Herdr core answers and the terminal's root process shares aiq's TTY. It writes nothing into that terminal, so there is no label: `witness.label` is `""`.
- On each read, the bridge requires three things. The lease's process must still be the one that took the lease. The recorded terminal root must be its ancestor. The core must still show that ID, a live terminal at that address, and that root pid.
- A Claude conversation is linked to an interactive lease only through Claude's own `sessions/<pid>.json`, for the lease's process or a descendant, written after that process started. Codex and Antigravity interactive conversations are never linked; their sessions are reached by lease only.
- The app checks `state:"live"` and `pid == terminal_pid`, and checks the label only when it is non-empty.

`terminal_reason` values:
- `no_lease`
- `ambiguous`
- `not_herdr` (tmux or other)
- `ended`
- `unknown`
- `label_missing`
- `interactive_unmapped`: an interactive session with no recorded Herdr terminal

## quotas (`quotas.json`)

- This machine only, with no `machines`.
- `poll.known` is always false: a bridge process cannot read the daemon's poll bookkeeping. Use each account's `observed_at` for freshness.
- `model_scope`, `accounts` and `rankings` have the same fields and units as `/api/nav/quotas` schema 2. Times are unix seconds, not ms.
- The app shows one section per Herdr machine and does not merge subscriptions.
