"""Labelled terminal pane tails for the pane-state benchmark.

Each case is the last dozen or so lines of a tmux pane running Claude Code
or Codex, as aiq's long-run supervisor would capture it, plus the answers a
careful operator would give:

  state       idle     agent finished its turn; empty input prompt
              working  mid-turn: thinking, streaming, or a tool running
              blocked  a modal prompt or menu waits for a keypress
              exited   the agent process ended; shell prompt or exit message
  needs_human whether a person must answer or approve before the agent can go on
  safe_key    the one key aiq may send unattended to clear the screen without
              changing anything that matters: none | enter | escape | two
"""

CASES = [
    # ---- Claude Code -------------------------------------------------------
    {
        "id": "cc_idle_prompt",
        "screen": """\
⏺ Done. The three tests pass and the README mentions the new flag.

╭──────────────────────────────────────────────────────────────╮
│ >                                                            │
╰──────────────────────────────────────────────────────────────╯
  ? for shortcuts                                    ⏵⏵ accept edits on
""",
        "state": "idle", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_idle_after_question",
        "screen": """\
⏺ I can do this two ways: keep the regex as a fast path, or replace it
  outright. Which do you prefer?

╭──────────────────────────────────────────────────────────────╮
│ >                                                            │
╰──────────────────────────────────────────────────────────────╯
  ? for shortcuts
""",
        "state": "idle", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cc_thinking",
        "screen": """\
⏺ Read(internal/longrun/longrun.go)
  ⎿  Read 490 lines

✻ Thinking… (12s · ↓ 1.2k tokens · esc to interrupt)
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_tool_running",
        "screen": """\
⏺ Bash(go test ./... 2>&1 | tail -40)
  ⎿  Running…

  ok    github.com/orlenko/aiq/internal/config   0.412s
  ok    github.com/orlenko/aiq/internal/fsutil   0.088s
* Baking… (48s · ↑ 3.1k tokens · esc to interrupt)
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_streaming_text",
        "screen": """\
⏺ The supervisor reads the pane every tick. When the rate-limit menu is
  open it sends "2", which selects "Keep current model". For sessions
  started with the -c override the menu never appears, so
* Whirring… (6s · esc to interrupt)
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_permission_prompt",
        "screen": """\
⏺ Bash(rm -rf dist && go build -o dist/aiq ./cmd/aiq)

  Do you want to proceed?
  ❯ 1. Yes
    2. Yes, and don't ask again for rm commands in this project
    3. No, and tell Claude what to do differently (esc)

  Enter to confirm · Esc to cancel
""",
        "state": "blocked", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cc_edit_permission",
        "screen": """\
⏺ Update(cmd/aiq/long.go)
  ⎿  Updated cmd/aiq/long.go with 4 additions and 1 removal

  Do you want to make this edit to long.go?
  ❯ 1. Yes
    2. Yes, allow all edits during this session (shift+tab)
    3. No, and tell Claude what to do differently (esc)
""",
        "state": "blocked", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cc_trust_dialog",
        "screen": """\
╭──────────────────────────────────────────────────────────────╮
│ Do you trust the files in this folder?                       │
│                                                              │
│ /Users/vorlenko/personal/aiq                                 │
│                                                              │
│ Claude Code may read files in this folder. Reading untrusted │
│ files may lead Claude Code to behave in unexpected ways.     │
│                                                              │
│ ❯ 1. Yes, proceed                                            │
│   2. No, exit                                                │
╰──────────────────────────────────────────────────────────────╯
""",
        "state": "blocked", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cc_update_notice",
        "screen": """\
╭──────────────────────────────────────────────────────────────╮
│ ✓ Claude Code updated to 2.1.274                             │
│                                                              │
│ What's new:                                                  │
│ • /loop now self-paces when no interval is given             │
│ • Faster startup on large repositories                       │
│                                                              │
│ Press Enter to continue                                      │
╰──────────────────────────────────────────────────────────────╯
""",
        "state": "blocked", "needs_human": False, "safe_key": "enter",
    },
    {
        "id": "cc_compacting",
        "screen": """\
⏺ Bash(git log --oneline -20)
  ⎿  f2beccd long: keep Codex sessions past the rate-limit model-switch prompt
     256993b README: add recipes for aiq long auto and aiq long auto resume
     … +18 lines

✻ Compacting conversation… (esc to interrupt)
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_usage_limit",
        "screen": """\
⏺ Bash(go vet ./...)
  ⎿  Error: You've hit your usage limit · resets at 3pm (America/Toronto)

╭──────────────────────────────────────────────────────────────╮
│ >                                                            │
╰──────────────────────────────────────────────────────────────╯
  ? for shortcuts
""",
        "state": "idle", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cc_exited_shell",
        "screen": """\
⏺ Wrote the handoff note to ~/.local/share/aiq/handoff/3f9a1c2b7d0e4a11.md.
  Ending the turn as instructed.

╭──────────────────────────────────────────────────────────────╮
│ >                                                            │
╰──────────────────────────────────────────────────────────────╯

Goodbye!
vorlenko@mbp aiq %
""",
        "state": "exited", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cc_quoted_menu_in_transcript",
        "screen": """\
⏺ For reference, the menu the supervisor has to dismiss looks like this:

    Approaching rate limits
    › 1. Switch to gpt-5-mini
      2. Keep current model
    Press enter to confirm or esc to go back

  My regex anchors on the last twelve lines so a quote like this one
  does not match.

╭──────────────────────────────────────────────────────────────╮
│ >                                                            │
╰──────────────────────────────────────────────────────────────╯
""",
        "state": "idle", "needs_human": False, "safe_key": "none",
    },
    # ---- Codex -------------------------------------------------------------
    {
        "id": "cx_idle_prompt",
        "screen": """\
• Updated internal/provider/codex/codex.go and added a test for the
  twelve-line window. `go test ./internal/provider/...` passes.

╭──────────────────────────────────────────────────────────────╮
│ ›                                                            │
╰──────────────────────────────────────────────────────────────╯
  gpt-5.3-codex · 62% context left · ? for shortcuts
""",
        "state": "idle", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cx_working",
        "screen": """\
• I'll look at how the supervisor captures the pane before changing the
  matcher.

  exec  rg -n "Capture" internal/longrun internal/tmux
  ⎿  internal/longrun/longrun.go:246:	screen, err := tmux.Capture(l.Pane)

• Working (23s • Esc to interrupt)
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cx_approval_prompt",
        "screen": """\
• I need to run the test suite to confirm the fix.

  Allow command?
    go test ./... 2>&1 | tail -40

  ▶ Yes (y)
    Yes, and don't ask again for this session (a)
    No, and tell Codex what to do differently (n)
""",
        "state": "blocked", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cx_rate_limit_menu",
        "screen": """\
• Applied the patch to cmd/aiq/run.go.

  Approaching rate limits
  You have used 92% of your weekly limit. Switch to a cheaper model
  to keep working?

  › 1. Switch to gpt-5-mini
    2. Keep current model

  Press enter to confirm or esc to go back
""",
        "state": "blocked", "needs_human": False, "safe_key": "two",
    },
    {
        "id": "cx_rate_limit_menu_keep_selected",
        "screen": """\
  Approaching rate limits
  You have used 92% of your weekly limit. Switch to a cheaper model
  to keep working?

    1. Switch to gpt-5-mini
  › 2. Keep current model

  Press enter to confirm or esc to go back
""",
        "state": "blocked", "needs_human": False, "safe_key": "two",
    },
    {
        "id": "cx_exited",
        "screen": """\
• Handoff note written. Stopping here.

╭──────────────────────────────────────────────────────────────╮
│ ›                                                            │
╰──────────────────────────────────────────────────────────────╯

Thanks for using Codex.
vorlenko@station:~/personal/aiq$
""",
        "state": "exited", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cx_crashed",
        "screen": """\
• Working (4s • Esc to interrupt)
thread 'main' panicked at codex-tui/src/app.rs:412:17:
called `Option::unwrap()` on a `None` value
note: run with `RUST_BACKTRACE=1` environment variable to display a backtrace
vorlenko@station:~/personal/aiq$
""",
        "state": "exited", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "cx_login_required",
        "screen": """\
╭──────────────────────────────────────────────────────────────╮
│ Sign in to Codex                                             │
│                                                              │
│ Your session has expired. Open the link below to sign in:    │
│ https://auth.openai.com/codex/device?code=KJHR-PLMQ          │
│                                                              │
│ Waiting for sign-in…  (Esc to quit)                          │
╰──────────────────────────────────────────────────────────────╯
""",
        "state": "blocked", "needs_human": True, "safe_key": "none",
    },
    {
        "id": "cx_idle_asks_question",
        "screen": """\
• Two of the fixtures disagree about whether a trust dialog counts as
  "blocked" or "exited". Which label do you want?

╭──────────────────────────────────────────────────────────────╮
│ ›                                                            │
╰──────────────────────────────────────────────────────────────╯
  gpt-5.3-codex · 40% context left
""",
        "state": "idle", "needs_human": True, "safe_key": "none",
    },
    # ---- Neither ----------------------------------------------------------
    {
        "id": "shell_only",
        "screen": """\
Last login: Wed Sep 17 09:12:04 on ttys004
vorlenko@mbp aiq %
""",
        "state": "exited", "needs_human": False, "safe_key": "none",
    },
    {
        "id": "tmux_respawn_in_progress",
        "screen": """\
aiq: moving this session to account codex/work-2 (3% left, resets 14:00)
aiq: resuming transcript 8c1d…
Loading…
""",
        "state": "working", "needs_human": False, "safe_key": "none",
    },
]
