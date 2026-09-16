# aiq

`aiq` pools several paid Claude Code and Codex subscriptions and routes every
`claude` and `codex` invocation to the account that is about to waste the
most quota. Interactive sessions, `claude -p` / `codex exec` workers, and the
workers those agents spawn in turn all go through it. Nobody has to know
which subscription they are on.

```text
$ claude                      # interactive: sticky per workspace
aiq: using claude/work — 5h 43%, weekly 8%

$ codex exec "review the diff" # worker: scored per call, silent on success
```

A small daemon keeps telemetry fresh and serves a status page on
`http://127.0.0.1:7379/`. When you come back to a directory, `aiq resume`
lists the sessions that ran there, shows what each turn asked and answered,
and resumes the one you pick. Works on macOS and Linux.

## How it works

**Accounts are overlay homes.** Each Claude account is a `CLAUDE_CONFIG_DIR`
under `~/.local/share/aiq/claude/<name>/`; each Codex account is a
`CODEX_HOME` under `~/.local/share/aiq/codex/<name>/`. Every entry of the
real `~/.claude` or `~/.codex` is symlinked into the overlay (settings,
plugins, projects, sessions, history, sqlite state), so all accounts share
one configuration and `--resume` works on any of them. Only the credential
is private: the Keychain entry Claude Code keys by config dir on macOS
(`.credentials.json` on Linux), and `auth.json` for Codex. The overlay is
re-synced before every launch under a per-account lock; a file the CLI
created or rewrote in the overlay is moved back into the real home and
re-linked, a directory two sessions created independently is merged, and a
displaced copy is kept as a rotated `.aiq-bak` next to the real file. Two
things stay private per account: the credential, and `.claude.json` (Claude
Code rewrites it with a rename on every change, so sharing it would lose
writes). It is seeded from your `~/.claude.json` when the account is created,
which means project trust and user-scoped MCP servers are per account.

The account matching the current `~/.claude` login is registered as
*native* and launches without `CLAUDE_CONFIG_DIR` at all.

**Shims.** `aiq shim install` writes `claude` and `codex` scripts into
`~/.local/share/aiq/shims/`; put that directory first on `PATH`. Each script
runs `aiq run <provider> -- args`, which picks an account, syncs its
overlay, records a lease, and execs the real CLI. Nested calls carry
`AIQ_DEPTH`, `AIQ_PARENT_ACCOUNT` and a root lease id; `max_depth` stops
runaway recursion. Wrappers other tools put on `PATH` are handled by an
exec chain: a wrapper that bounces back into the shim with the same pid (or
as the direct child of a shell holding that pid) is recognised and the next
candidate on `PATH` is exec'd instead. Only entries after the shim directory
are candidates: a wrapper ahead of the shims has already run and delegated to
them, and running it again would apply its flags twice.

**Two kinds of launch.**

- *Interactive* (a terminal, no `-p`/`exec`): sticky per workspace, i.e. a
  git checkout keeps its account until a binding window crosses
  `switch_pct`. The shim execs the CLI, so the lease follows its pid.
- *Worker* (`claude -p`, `codex exec`, or a non-terminal stdout): scored
  per call, kept off accounts that carry an interactive session when an
  alternative exists, capped at `max_workers_per_account`, and skipped on
  accounts whose weekly remaining is at or below `weekly_reserve_pct`. The
  worker runs as a child with its output tee'd; if the provider rejects it
  with a usage-limit message early (little output, or within seconds) and
  within `retry_max_seconds`, the account is benched and the same command
  reruns on the next account. The bench lasts until the reset of a window
  telemetry shows near its cap, otherwise one hour.

**Telemetry.** aiq polls every account itself, every `interval_seconds`
and right after any limit event. Claude usage comes from Anthropic's OAuth
usage endpoint with a token aiq mints once per account through the same
PKCE flow Claude Code uses (`aiq account authorize`); Claude Code's own
credential is never read. Codex usage comes from the official
`codex app-server` rate-limits RPC, run against the account's home, so Codex
keeps refreshing its own `auth.json`. Claude's status line adds live updates
for the account in use through a multiplexer that chains to whatever status
line you had. Shims never depend on the daemon: state lives in SQLite, so
with the daemon down they route on the last poll.

## Long-running sessions

A QC loop or an orchestration overseer can run for days and burn a week of
quota. `aiq long claude` / `aiq long codex` starts such a session in a tmux
session named after the workspace and supervises it:

- **Hooks, not a startup instruction.** The session is launched with aiq's
  `SessionStart`, `UserPromptSubmit` and `Stop` hooks (per launch, via
  `--settings` for Claude and `-c hooks.…` for Codex); ordinary sessions are
  untouched. When the account's tightest binding window has `drain_pct` or
  less remaining, the daemon flags the lease and the next hook injects the
  wrap-up: finish the current unit, stop background jobs and subagents, write
  a handoff note, end the turn.
- **Resume, not summarize.** The overlay shares transcripts, so the same
  provider's successor gets `--resume <session-id>` on the next account and
  keeps the whole conversation; the note is a supplement. A different
  provider (every Claude out, a Codex takes over, or the reverse) starts fresh
  from the note plus the working tree. The permission bypass carries over in
  the successor's spelling (`--dangerously-skip-permissions` ↔
  `--dangerously-bypass-approvals-and-sandbox`), and so do a tier model
  (`--model opus` ↔ `--model gpt-5.6-sol`, see the tier table below) and an
  effort level (`--effort high` ↔ `-c model_reasoning_effort=high`). Every
  other flag is provider-specific and dropped. Fallback order is
  `long.fallback`.
- **Takeover in place.** The daemon `respawn-pane`s the same tmux pane with
  the successor, copies the project's trust entry between Claude accounts so
  no dialog blocks the restart, and nudges the successor with a first prompt.
  An idle session is moved without waiting for a wrap-up turn; a session on
  an account that is already blocked is moved at once; when no account has
  headroom the session waits and is moved when a window resets.

```text
aiq long claude [claude args...]   start or attach
aiq long auto [--model-tier 0]      start on whichever pool auto picks, or attach
aiq long auto resume [<id>]         pick a session here and resume it as a long one
aiq long list                       leases, pane, drain state, idle/busy
aiq long drain .                    move this workspace's session now
aiq long attach                     re-attach
aiq long stop .                     end it
```

Choose the initial account with `aiq long codex --account bjola -- --yolo`
(or `--account=bjola`). This also works with Claude and named launchers.
Put `--account` before CLI arguments; `--` ends aiq's options. The daemon
can still move the session to another account later. If a long session already
exists for the workspace, the command attaches to it instead of changing its account.

`aiq long auto` chooses the first account the way `aiq run auto` does:
across both pools, at the requested tier (default 1), with YOLO. Its
takeover order is the chosen provider first, so a successor resumes the same
transcript while that pool has headroom, then the other provider. It takes no
prompt, because a takeover replays the launch arguments; type the task in the
session.

`aiq long auto resume` opens the `aiq resume` browser for this directory;
`r` on a session reopens it in a long session instead of the current
terminal (or give the id: `aiq long auto resume 3f2a`). The transcript fixes
the provider; the account is routed, the model tier and effort apply as for
`auto`, and the bypass is on. As with `aiq resume`, a session that ran under a
launcher resumes through it (`b` or `--bare` skips it), and then gets the
bypass only if its earlier launch passed it. A session already running in
`aiq long` is attached. The pane starts in the current directory, so the CLI
finds the transcript.

Handoff notes live in `~/.local/share/aiq/handoff/`.

## Picking up where the agents left off

After a day of sessions in one directory, several of them run by `aiq long`,
`aiq resume` shows what each agent did there before you start the next one.

```text
aiq resume                  browse this directory's sessions
aiq resume --all            include workers (claude -p, codex exec)
aiq resume <id>             resume one directly (a unique prefix is enough)
aiq resume --print [<id>]   the same lists as plain text, for scripts and agents
aiq resume --bare <id>      resume without the launcher the session ran under
```

The first screen lists every Claude and Codex session whose working directory
is the current one, newest first, with the model, turn and tool counts, and
the session's name or first prompt. Enter opens a session's turns: each prompt
with the agent's final reply for that turn. Enter on a turn shows it in full,
and ←/→ move between turns. `r` resumes the selected session on an account
the pool picks, with `claude --resume <id>` or `codex resume <id>`.

- A session is resumed through the launcher it ran under, so a `safe-claude`
  session goes back into the same nono sandbox. aiq records every launch
  (account, launcher, directory, arguments) in `state.db` before the CLI
  starts. For a new Claude session it chooses the id itself
  (`--session-id`), so the match is exact. Codex cannot be given an id, so a
  new Codex thread is matched to the latest aiq launch in the same directory
  before it; the list marks such a guess with `?` (`[via safe-codex?]`).
  Once a session has run under a launcher, it keeps resuming there:
  `b` in the browser, or `--bare`, resumes it without one, for that time
  only. `--launcher <name>` picks another launcher. Sessions from before this
  record existed, and anything started without aiq, resume bare.
- A bare resume adds the permission bypass if the transcript shows the
  session ran with it. Through a launcher, aiq adds it only if you passed it
  to that launcher yourself: the safe-* wrappers add it on their own, and
  Codex refuses the flag twice. Anything after `--` is added to the CLI's
  arguments.
- A session still running in `aiq long` is attached, not started a second
  time. For a Claude session that is open in another terminal (● in the
  list), `r` asks once more before it opens a second copy.
- Subagent transcripts are not listed. Their work appears in the parent
  session's turns as tool calls.

## Automatic provider and model selection

```bash
aiq run auto -p "do this thing"
aiq run auto --model-tier 0 --effort 6 -p "analyze this design"
aiq run auto --model-tier 2 --effort 2 -p "fix the failing test"
aiq run auto --model-tier 1             # interactive session
```

`auto` ranks eligible Claude and Codex accounts together using the quota score,
worker reserve and concurrency limits. It skips providers whose CLI is missing.
Selection uses stored telemetry, as ordinary routing does; unknown or stale
telemetry cannot guarantee remaining quota. An early worker quota rejection
can retry on another account, including the other provider, under the existing
worker retry policy. Interactive sessions choose once at launch.

`-p` translates to Claude's print mode or Codex's `exec` command. The prompt is
preserved as one literal argument. Without `-p`, `auto` opens an interactive
session; add `-- "initial prompt"` to start it with a prompt. **Auto always
enables permission bypass**: `--dangerously-skip-permissions` for Claude or
`--yolo` for Codex, including workers and cross-provider retries. An explicit
`--yolo` is still accepted but is redundant. Native provider options require
`aiq run claude` or `aiq run codex`; `auto` does not translate arbitrary flags,
resume sessions (use `aiq resume`, or `aiq long auto resume`), named launchers,
or forced accounts. `--wait` and `--mode` remain available, with worker mode
requiring `-p`. For a supervised session that moves between accounts, use
`aiq long auto` (see Long-running sessions).

| Tier | Claude | Codex |
| --- | --- | --- |
| 0 | Fable (`fable`) | Astra (`gpt-6-astra`) |
| 1 (default for auto) | Opus (`opus`) | Sol (`gpt-5.6-sol`) |
| 2 | Sonnet (`sonnet`) | Terra (`gpt-5.6-terra`) |
| 3 | Haiku (`haiku`) | Luna (`gpt-5.6-luna`) |

The requested tier stays fixed during retries; aiq never silently downgrades it.
Model-scoped quota windows bind only the selected model, so an exhausted Fable
cap does not disqualify an otherwise eligible Opus account.

| Effort | Claude | Codex |
| --- | --- | --- |
| 1 | low | low |
| 2 | medium | medium |
| 3 | high | high |
| 4 | xhigh | xhigh |
| 5 | max | max |
| 6 | ultracode | ultra |

Effort is optional; omission preserves the CLI default. Claude's sixth setting
means `xhigh` plus dynamic workflows, rather than a sixth model reasoning level;
it requires Claude Code 2.1.203 or later. See the
[Claude CLI reference](https://code.claude.com/docs/en/cli-reference).
Model and client support still apply to each setting.

Both shared options also work with an explicit provider, before `--`:

```bash
aiq run claude --model-tier 0 --effort 6 -- -p "review this design"
aiq run codex --model-tier 2 --effort 3 -- exec "fix the failing test"
```

Explicit providers keep their configured model when no tier is supplied.
Do not combine a tier with `--model-scope` or a native model override; the tier
sets both the model and its quota scope. Pass native named effort values after
`--`, for example `aiq run claude -- --effort high -p "review this"`.

## Recipes

Two habits that make the pool disappear from day-to-day work.

### Start a session without choosing anything

```bash
aiq run auto
```

When the question is "can I finish this session" rather than "which model
do I want", let aiq decide. It picks the provider and account with the most
quota about to expire, opens an interactive session on tier 1 (Opus or Sol)
with permission bypass on, and stays there for the session. Pin the tier for
the session with `--model-tier`:

```bash
aiq run auto --model-tier 0              # the strongest model that has quota
aiq run auto --model-tier 2 --effort 2   # cheap and quick
```

Add `-- "initial prompt"` to open the session with a first message already
sent.

### `aip`: ask from the shell prompt

A one-shot question should cost one line, with no quoting. This shell
function turns everything after `aip` into a single `aiq run auto -p` prompt,
and a line widget quotes the rest of the line at Enter so apostrophes,
question marks, globs and `!` history references pass through untouched:

```zsh
# ~/.zshrc
aip() {
  aiq run auto -p "$*"
}

# Shorthands whose arguments get passed verbatim when typed at the prompt.
verbatim_cmds=(aip)

_verbatim_accept_line() {
  local name rest
  for name in $verbatim_cmds; do
    [[ $BUFFER == "$name "* ]] || continue
    rest=${BUFFER#"$name "}
    # Skip lines already quoted, e.g. recalled from history.
    [[ $rest == "${(qq)${(Q)rest}}" ]] || BUFFER="$name ${(qq)rest}"
    break
  done
  zle .accept-line
}
zle -N accept-line _verbatim_accept_line
```

Then:

```text
$ aip how do I run a command on a remote machine over ssh without a tty?
$ aip what's the git command to drop the last commit but keep its changes?
$ aip why does !ls fail on this box?
```

Each call is a worker: scored per call, kept off accounts that carry an
interactive session when another is available, and retried on the next
account if the provider rejects it with a usage limit. The widget rewrites
the buffer before it is accepted, so the quoting is visible on the line as
it runs, history keeps the quoted form, and recalling a line does not quote
it twice. Add other functions to
`verbatim_cmds` to give them the same treatment.

## Scoring: spend what is about to vanish

For every *binding* window `w` of an account (the 5h and weekly windows,
plus a model-scoped weekly cap such as "Fable" when that model is your
default):

```text
remaining_w = 100 − used_w
hours_w     = time until w resets, floored at min_hours
perish_w    = min(remaining_w, remaining_weekly) / hours_w
score       = Σ perish_w × weight_w  ÷  (1 + worker leases on the account)
```

`perish_w` is the number of percentage points that vanish per hour if the
account sits idle. `weight_w` is 1 for the session window and
`weekly_weight` (default 5) for weekly windows, because one weekly point is
several session points' worth of tokens: an account whose week rolls over
in ten hours with 40% unspent gets the work all afternoon, not only in the
last hour. Highest score wins. Exhausted accounts (a binding window
at 100%, or a limit hit), disabled accounts, and accounts without a login
are filtered first; workers additionally respect the weekly reserve and the
per-account cap. Telemetry older than `stale_after_seconds` falls back to a
neutral prior. `aiq status --explain` prints the ranking with every term.

## Install

For a first install on macOS or Ubuntu, install Git and Go (the version in
`go.mod` or newer), then clone and run:

```bash
git clone https://github.com/orlenko/aiq.git
cd aiq
./install-or-update.sh
```

For updates, run the same script from your existing checkout, from any directory:

```bash
~/code/aiq/install-or-update.sh       # use your checkout's actual path
```

The script fast-forwards the checkout's tracking branch, builds and atomically
installs `~/.local/bin/aiq`, refreshes provider and launcher shims, and installs
or restarts the current user's daemon (launchd on macOS, systemd on Ubuntu).
Run it as your regular user, without `sudo`. It refuses to pull over local
changes. Use `--no-pull` to install the current working tree, or `--no-daemon`
to install only the CLI and shims. On Ubuntu, daemon installation needs a
running systemd user manager. Add the printed shim directory and `~/.local/bin`
to your shell's `PATH`; the script does not edit shell startup files.

Alternatively, install manually:

```bash
go install github.com/orlenko/aiq/cmd/aiq@latest   # or: go build -o ~/.local/bin/aiq ./cmd/aiq
aiq shim install                 # then add the printed PATH line to your shell rc
aiq daemon install               # launchd (macOS) or systemd --user (Linux)
```

After either installation method, add your first accounts:

```bash
aiq account add claude work      # Claude Code login + quota poll grant (two browser steps)
aiq account add claude home
aiq account add codex work       # codex login (one browser step)
aiq doctor
```

Sign in as the intended account at each browser step: sign out of claude.ai
or chatgpt.com first, or use a private window, so the browser does not hand
back the account it is already signed in as. aiq warns when two accounts
turn out to be the same login.

If you run [aiquota](https://github.com/orlenko/misc/tree/main/quota-monitor) on the same machine,
`aiq account import` adopts its accounts instead: Codex `auth.json` files and
Claude poll grants move into the aiq homes and aiquota is re-pointed at them,
labels and grid order come along, and the Claude account matching the
current `~/.claude` login is registered as native. aiquota is not needed
for anything else.

## Commands

```text
aiq claude [args...]                 same as the shim: route and launch
aiq codex  [args...]
aiq run <provider> [--account <name>] [--next] [--mode interactive|worker] -- [args...]

aiq status [--json] [--refresh] [--explain]
aiq top                              live console view

aiq account list
aiq account add <provider> <name>    new overlay home + login (+ poll grant for Claude)
aiq account login <provider>/<name>  redo the CLI login
aiq account authorize claude/<name>  redo the quota poll grant
aiq account poll [<id>...]           poll now
aiq account label <id> <text>        label shown on the status page
aiq account order [<id>...]          display order within each provider column
aiq account enable|disable|remove <provider>/<name> [--purge]
aiq account use <provider>/<name>    pin this workspace
aiq account next <provider>          rotate this workspace
aiq account import [id[=name]...]    adopt aiquota accounts, if you have aiquota

aiq mark <provider>/<name> exhausted [--until 14:42|+2h|RFC3339]
aiq mark <provider>/<name> ready
aiq reset codex/<name>               consume an earned Codex reset credit

aiq resume [--all] [--print] [--launcher <name> | --bare] [<id>] [-- args]
                                     this directory's sessions: browse turns, resume one

aiq shim install|uninstall|path
aiq statusline install|uninstall|status   Claude status-line multiplexer
aiq daemon run|install|uninstall|status
aiq doctor
```

Launch flags for orchestrators (also as `AIQ_MODEL_SCOPE`, `AIQ_WAIT`,
`AIQ_MODE` in the environment of a child):

- `--model-scope <name>` binds the named model-scoped window for this launch
  (`fable` skips accounts whose Fable weekly cap is dry; `opus` or `none`
  ignores it). Default comes from `providers.<p>.model_scope`.
- `--wait <duration>` (`600`, `600s`, `10m`) makes a worker poll for a free
  slot when every eligible account is at `max_workers_per_account`, instead
  of refusing; it still refuses at once when the pool is exhausted, since
  waiting cannot fix that. A value that does not parse is `bad-flags`.
  `[worker] wait_for_slot_seconds` sets a default.

Escape hatches: `AIQ_BYPASS=1 claude ...` runs the real CLI on your real
home with no routing; `claude auth ...`, `claude setup-token`, `claude
update`, `codex login` and `codex logout` always pass through.

## Machine interface

Two things are stable for other tools: exit codes and the status JSON.

**Exit codes.** A child's own exit code passes through unchanged. aiq's own
refusals exit with a distinct code and print one machine-readable line,
`aiq: refused: <token>`, before the human-readable message:

| code | tokens | meaning |
|---|---|---|
| 75 | `pool-exhausted`, `at-worker-cap`, `wait-timeout` | nothing available right now; try later or elsewhere |
| 78 | `no-accounts`, `no-binary`, `bad-flags`, `max-depth`, `account-not-found` | nothing to route to; a human has to change something |

`bad-flags` covers aiq's own flags only (`--mode`, `--wait`). An unknown
flag before `--` is not aiq's: it is forwarded to the CLI, which fails in its
own way with its own exit code.

**Status JSON.** `aiq status --json` and `GET http://127.0.0.1:7379/api/state`
(under `view`) return the same document. `schema_version` is 1; additions do
not bump it, a change of meaning does. Stable fields:

```text
schema_version, generated_at, hostname
worker_capacity{<provider>: n}        free worker slots per provider, summed over eligible accounts
worker_capacity_by_scope{<provider>: {none: n, <scope>: n, …}}
                                      same, with that model-scoped window binding ("none" = ignored)
accounts[]:
  id, provider, name, enabled, native, identity, plan
  has_credential, eligible, ineligible, exhausted, reason, cooldown_until
  score, rank, terms[]                worker ranking (see Scoring)
  worker_slots                        free slots on this account (0 when ineligible)
  workers, interactive                live leases by kind
  windows[]: key, label, kind, scope, binding, used_pct, resets_at, window_seconds, source, observed_at
  leases[]: pid, mode, cwd, depth, started_at, drain, pane, workspace
rankings{"<provider>/<mode>": [{id, score, eligible, reason, terms}]}
events[]: ts, provider, account, type, detail
```

Size a batch fleet from `worker_capacity_by_scope[provider][scope]`; pick a
provider per model tier from the same map.

**Example: a review fleet spawning Codex seats.** An orchestrator that runs
inside a routed interactive `claude` session (depth 0) launches one process
per seat, which spawns `codex` by bare name with the prompt on stdin and the
result in a file:

```text
env:  AIQ_MODEL_SCOPE=<tier scope>  AIQ_WAIT=600     (inherited by the spawn)
argv: codex exec --ephemeral -C <repo> -s read-only -m <model> \
        -c model_reasoning_effort=<effort> --color never -o <result.md> -
stdio: stdin ← prompt (written once, then closed); stdout ignored; stderr tail kept
```

What aiq does with it: the shim classifies it as a worker (`exec` first,
stdout a pipe), routes it to an eligible account under that scope, forwards
stdin unchanged, tees stderr and stdout for the usage-limit patterns, and
reruns the same command on the next account if the provider rejected it
early. Six seats launched at once on a one-account pool run three at a
time with `AIQ_WAIT` set; without it, seats four to six exit 75
`at-worker-cap` immediately. A timeout that signals the process group reaches
both aiq and its child; aiq also forwards `SIGTERM`/`SIGHUP` to the child.
Depth is 1, well under `max_depth`.

## Storage

| What | Where |
|---|---|
| Overlay homes (credential, poll grant, `.claude.json` private; everything else symlinked) | `~/.local/share/aiq/claude/<name>/`, `~/.local/share/aiq/codex/<name>/` |
| Shims | `~/.local/share/aiq/shims/` |
| State (accounts, windows, leases, affinity, launches, events; no credentials) | `~/.local/share/aiq/state.db` |
| Daemon log | `~/.local/share/aiq/log/daemon.log` |
| Config | `~/.config/aiq/config.toml` |
| Session transcripts `aiq resume` reads (never writes) | `~/.claude/projects/`, `~/.codex/sessions/`, plus any unsynced copies in the overlay homes |
| Service | `~/Library/LaunchAgents/dev.aiq.daemon.plist` or `~/.config/systemd/user/aiq.service` |

`AIQ_DATA_DIR` and `AIQ_CONFIG` relocate the data directory and the config
file.

## Configuration

Everything has defaults; the file is optional.

```toml
version = 2

[providers.claude]
binary = ""              # discovered on PATH (outside the shim dir) when unset
model_scope = "auto"     # scoped weekly cap that binds; "auto" reads settings.json model
wrapper = ""             # launcher for interactive and long sessions (see below)

[providers.codex]
binary = ""
model_scope = "auto"     # reads config.toml model
wrapper = ""

[selection]
interactive_policy = "sticky"   # sticky | score
switch_pct = 95.0
stale_after_seconds = 900
weekly_reserve_pct = 5.0
max_workers_per_account = 3
max_depth = 3
min_hours = 0.25
weekly_weight = 5.0

[worker]
retry = true
retry_max_seconds = 60.0
wait_for_slot_seconds = 0       # workers poll this long for a free slot at the cap
# limit_patterns = ["(?i)hit your (usage|session|weekly) limit", ...]

[poll]
interval_seconds = 300
timeout_seconds = 60

[display]                       # status page and `aiq status`
order = ["claude/work", "claude/home", "codex/work"]
[display.labels]
"claude/work" = "Claude · work"

[daemon]
listen = "127.0.0.1:7379"

[long]
drain_pct = 4.0                 # ask for a wrap-up at this much remaining
fallback = ["claude", "codex"]  # takeover order after the session's own provider
check_interval_seconds = 30
idle_grace_seconds = 20
tmux_prefix = "aiq"

[telemetry]
claude_statusline = true
```

## Launchers

A launcher is a named program that starts the CLI in an environment of its
own: a sandbox, a recorder, a profiler. Routing happens first, so the
launcher inherits a session that already has an account.

Nothing is ever wrapped unless its name is the word you typed. Plain
`claude` and `codex` keep exec'ing the CLI directly.

```bash
aiq launcher add boxed --provider claude --credential file \
  --env BOX_ALLOW='$CLAUDE_CONFIG_DIR' -- /usr/local/bin/boxed --profile strict
```

which registers

```toml
[launchers.boxed]
provider   = "claude"
command    = "/usr/local/bin/boxed"
args       = ["--profile", "strict"]
credential = "file"
fallback   = ["boxed-codex"]
[launchers.boxed.env]
BOX_ALLOW = "$CLAUDE_CONFIG_DIR"
```

and writes `~/.local/share/aiq/shims/boxed`, so all three of these start it:

```text
boxed [args...]            the shim
aiq boxed [args...]        the same, spelled out
aiq long boxed [args...]   supervised, with drain and takeover
```

The contract:

- The launcher receives the CLI's arguments, after any `args` of its own,
  and starts the CLI itself.
- `CLAUDE_CONFIG_DIR` (or `CODEX_HOME`) already names the chosen account's
  overlay home. A launcher that confines the CLI has to let it reach that
  directory, and the real `~/.claude` / `~/.codex` the overlay links into.
  An `env` value may name those variables, as `BOX_ALLOW` does above; they
  expand when the launcher runs.
- aiq drops its own shim directory from `PATH` first, so a launcher that
  starts `claude` or `codex` by bare name reaches the real CLI instead of
  routing a second time. A launcher may therefore be named after the very
  command it wraps. `AIQ_BINARY` holds the resolved path, `AIQ_LAUNCHER` the
  name in use.
- `credential = "file"` writes the account's credential into the overlay
  home before the launch, for a launcher that cuts the CLI off from the
  system credential store. Without it such a launcher gets a login prompt.
  A token the CLI then refreshes stays in that file; `aiq doctor` says so
  when the copy in the store has fallen behind.
- Workers, logins, passthrough commands and telemetry probes never use a
  launcher. They are not sessions a person is sitting in front of, and some
  have no terminal at all.

A long session started under a launcher keeps it across a takeover.
`fallback` names the launchers it may move to, in order; the providers of
those launchers are the only ones it moves between, so a session never
quietly loses the launcher it was started with. `aiq long <name>` prints the
order it will use.

## Scope

Not in this version: agy (Antigravity) accounts and automatic consumption of
Codex reset credits (`aiq reset codex/<name>` is manual).

## A note on the Claude quota endpoint

Codex quota comes from the documented `codex app-server` RPC. Claude has no
documented read path for subscription usage, so aiq uses the same OAuth
usage endpoint Claude Code's own `/usage` calls, with a grant minted through
Claude Code's OAuth client. That endpoint is not a public API and may change
or be restricted without notice; if it stops answering, routing degrades to
the status-line feed (live for the account in use) and worker rejections,
and `aiq doctor` says so.

## License

[MIT](LICENSE)
