# Handoff — rebuild and reinstall aiq on the Mac machines

Written 2026-09-21 from the Ubuntu box (`instrumentaladmin-Precision-Tower-3430`, Ubuntu 24.04.4).
Audience: the agent on either Mac. The fleet is two Macs now — **do this on both**, they are
independent installs with their own binary, shims and daemon.

## The ask, in one line

```bash
~/code/aiq/install-or-update.sh        # use your checkout's actual path
```

That fast-forwards the checkout, rebuilds, reinstalls `~/.local/bin/aiq`, refreshes the shims
and restarts the user daemon. Read the two snags below first — one of them is likely to stop you.

## Why it matters (don't skip the reinstall after pulling)

`origin/main` is now at **`c34607e`**. Two new commits, both changing long-session behaviour,
and both of them live in the **daemon** — so a Mac that has pulled but not rebuilt is still
making the *old* takeover decisions. Pulling alone changes nothing at runtime.

- **`8cc95cf` — long: choose the account a resumed long session starts on**
  `aiq long auto resume` now takes `--account`: `aiq long auto resume [--account <name>] [<id>]`.
  Accepts a bare name (`--account claude3`) or a full id (`--account claude/claude3`), which must
  belong to the session's provider. Without it the account is routed as before.
  Authored on `Kozas-MacBook-Air` — so that Mac already has the source, but still needs the
  rebuild + daemon restart to actually *run* it.

- **`c34607e` — long: use low quota as last-resort capacity**
  `successor()` used to hand back "no account with headroom" whenever every candidate sat below
  `drain_pct`, stranding a long session on a spent account. Now it prefers a successor above
  `drain_pct`, and only if none exists falls back to an account with *strictly more* headroom than
  the current one; once the current account is blocked outright, any positive headroom qualifies.
  The strict comparison is what stops two equally-low accounts ping-ponging a lease.

## First: find out where you actually are

```bash
cd ~/code/aiq && git fetch origin && git log --oneline HEAD..origin/main
```

Empty output means you are current on source (you may still need the rebuild).

**Do not use `aiq version` to judge this.** It still prints `aiq 0.6.0` at `c34607e` — the version
string is baked in and was last bumped at `4451bbb`, several commits back. Git is the only
truth here.

## Snag 1 — the script refuses to pull over local changes, and you may well have some

This exact "last-resort capacity" change was written *independently on at least two machines*.
On this Ubuntu box it sat uncommitted and turned out to be a byte-for-byte duplicate of
`c34607e`. If your Mac did the same, `install-or-update.sh` stops with
`checkout has local changes; commit/stash them or use --no-pull`.

**Check whether your diff is already upstream before you touch it:**

```bash
cd ~/code/aiq && git fetch origin
for f in $(git diff --name-only); do
  git diff --quiet origin/main -- "$f" \
    && echo "SAME as origin/main: $f" \
    || echo "DIFFERS:            $f"
done
```

- Everything `SAME` → it is a duplicate. `git stash` it, run the install, then `git stash drop`.
- Something `DIFFERS` → look at it properly (`git diff origin/main -- <file>`) before deciding.
  A file can also "differ" merely by being *behind* origin, with no unique content of its own —
  that was the case for `README.md` here. Only genuinely new work is worth committing and pushing.

Do not reach for `--no-pull` to get around this: it builds your stale tree and you end up
installing a binary that is behind `origin/main`.

## Snag 2 — macOS restarts the daemon differently from Linux

The script branches on `uname -s`. On Darwin it uses `launchctl`, and there is **no separate
restart step** like the Linux `systemctl --user restart aiq.service`. The restart happens inside
`aiq daemon install`, which does `launchctl unload` then `launchctl load`
(`internal/daemon/service.go:159-161`). The script then polls `aiq daemon status` up to 15 times
and fails loudly if the listener never comes up, so a silent half-restart is not a risk — but if
it does fail, that is where to look.

Requirements on each Mac: **Go ≥ 1.25.0** (per `go.mod`), `git`, `launchctl`. Run as your normal
login user — the script refuses to run under `sudo`.

## Verify when it's done

```bash
cd ~/code/aiq && git log -1 --oneline     # → c34607e
ls -l ~/.local/bin/aiq                    # → mtime = just now
aiq daemon status                         # → daemon:  running at http://127.0.0.1:7379/
aiq status                                # → accounts polling, "seen just now"
go test ./...                             # optional; all packages passed here
```

## If long sessions are running on that Mac

They survive. The daemon restart is brief and the shims do not depend on the daemon — with it
down, routing falls back to the last poll in SQLite. Still worth an `aiq long list` before and
after so you can say for certain nothing was dropped. There were no long sessions on this box
when it was updated, so that path is untested today.

## State of this machine, for reference

Ubuntu box is done as of 2026-09-20 16:07 EDT: at `c34607e`, working tree clean, no stashes,
binary rebuilt, daemon restarted (it was PID 1038213), `go test ./...` all green.
