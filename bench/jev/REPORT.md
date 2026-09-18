# Jev vs Claude on pane-state judgment — first run, 2026-09-17

Jev answered 83–88% of the questions right for $0.0009 total and 0.4 s per
call. Sonnet 5, Opus 5 and Fable 5.1 answered 96–100% right for 400–1700×
the price and 8–12× the latency. Haiku 4.5 sat between on accuracy and cost.
Jev's misses were all in the conservative direction (it called idle screens
"blocked" and refused to name a safe key), which for aiq's supervisor means
a missed shortcut, not a wrong keypress.

24 hand-written cases, three questions each. Synthetic screens; n is small.
Read the numbers as "worth a real trial", not as a verdict.

## The matrix

| model | state | needs_human | safe_key | Brier(needs_human) | $ / 24 calls | $ / call | mean latency |
|---|---|---|---|---|---|---|---|
| Jev 1.13 | 83% | 88% | 88% | 0.111 | 0.0009 | 0.00004 | 0.38 s |
| Haiku 4.5 | 96% | 88% | 92% | 0.121 | 0.21 | 0.0088 | 10.9 s |
| Sonnet 5 | 100% | 92% | 100% | 0.057 | 0.37 | 0.0156 | 4.5 s |
| Opus 5 | 100% | 96% | 100% | 0.023 | 0.82 | 0.0341 | 3.0 s |
| Fable 5.1 | 100% | 96% | 100% | 0.024 | 1.54 | 0.0641 | 3.2 s |

Brier is mean squared error of the yes-probability for `needs_human`; 0 is
perfect, 0.25 is "always say 0.5". Jev's number is calibrated by training;
the Claude numbers are self-reported probabilities.

Tokens and time per call:

| model | input tok | output tok | of which thinking | latency mean / max |
|---|---|---|---|---|
| Jev | 912 | 107 (free) | 0 | 0.38 / 0.56 s |
| Haiku 4.5 | 3,187 | 916 | 744 | 10.9 / 34.0 s |
| Sonnet 5 | 3,986 | 298 | 138 | 4.5 / 14.3 s |
| Opus 5 | 3,765 | 188 | 22 | 3.0 / 5.8 s |
| Fable 5.1 | 3,769 | 178 | 4 | 3.2 / 4.9 s |

## What each model was asked

The task: given the last lines of a tmux pane running Claude Code or Codex,
say what the session is doing, whether a person is needed, and which single
key an unattended supervisor could safely send. Every model got the same
screen text and the same definitions; only the wrapping differs.

### Jev request

One HTTP POST per case to `https://api.typesafe.ai/v1/systemone`, body
`{"state": <screen text>, "model": "jev-latest", "questions": …}` with these
three questions (sent verbatim):

```json
{
  "state": {
    "type": "choice",
    "instructions": "The state is the last lines of a terminal pane running a coding agent (Claude Code or Codex) inside tmux. Ignore anything that is quoted inside an agent's message; judge only what is live on screen now. What is the session doing?",
    "criteria": {
      "idle": "The agent finished its turn and is waiting at an empty input prompt. The last agent message may ask the user something, but nothing modal is open.",
      "working": "The agent is mid-turn: thinking, streaming a reply, running a tool, compacting, or loading. A spinner or 'esc to interrupt' is typical.",
      "blocked": "A modal prompt, menu, dialog, or sign-in screen is open and waits for a keypress or answer before the agent can continue.",
      "exited": "The agent process has ended: a shell prompt, goodbye message, or crash trace is the last thing on screen."
    }
  },
  "needs_human": {
    "type": "noul",
    "instructions": "Does a person need to answer, approve, sign in, or decide something before this coding-agent session can make progress on its own? A session that is working, or idle with nothing asked, does not.",
    "criteria": {
      "true": "A permission or trust dialog, a sign-in screen, a question the agent asked the user, or a usage limit that only the user can resolve.",
      "false": "The agent is working, waiting idle with nothing to answer, has exited cleanly, or shows a notice that an automated supervisor may dismiss."
    }
  },
  "safe_key": {
    "type": "choice",
    "instructions": "An unattended supervisor may send at most one keypress to clear the screen without changing anything that matters. Which key, if any?",
    "criteria": {
      "none": "No key should be sent unattended: nothing is open, or what is open needs a person's decision (permissions, trust, sign-in, a question to the user).",
      "enter": "An informational notice such as a release note asks only to press Enter to continue.",
      "escape": "Escape backs out of something harmless without choosing anything.",
      "two": "A numbered menu is open where option 2 keeps the current model or current behaviour, and pressing 2 selects it."
    }
  }
}
```

Jev returns, per question, the chosen option, a probability for every
option, and a confidence; for the Noul, a single probability of "yes".

### Claude request

One `claude -p` call per case on the Claude Code subscription:

```
claude -p --tools "" --no-session-persistence --disable-slash-commands --strict-mcp-config \
  --model <id> --output-format json --system-prompt <below> --json-schema <below> \
  "Screen:\n\n<screen text>"
```

System prompt (sent verbatim; the same definitions as Jev's criteria, folded
into prose):

```
You judge the last lines of a terminal pane running a coding agent (Claude Code or Codex) inside tmux, captured by an unattended supervisor. Ignore anything quoted inside an agent's message; judge only what is live on screen now.

state: what the session is doing:
- idle: The agent finished its turn and is waiting at an empty input prompt. The last agent message may ask the user something, but nothing modal is open.
- working: The agent is mid-turn: thinking, streaming a reply, running a tool, compacting, or loading. A spinner or 'esc to interrupt' is typical.
- blocked: A modal prompt, menu, dialog, or sign-in screen is open and waits for a keypress or answer before the agent can continue.
- exited: The agent process has ended: a shell prompt, goodbye message, or crash trace is the last thing on screen.

needs_human: does a person need to answer, approve, sign in, or decide something before the session can make progress on its own? Working, idle with nothing asked, exited, or a notice a supervisor may dismiss all mean false. Also give needs_human_probability, your probability from 0 to 1 that the answer is yes.

safe_key: the one key the supervisor may send unattended to clear the screen without changing anything that matters:
- none: No key should be sent unattended: nothing is open, or what is open needs a person's decision (permissions, trust, sign-in, a question to the user).
- enter: An informational notice such as a release note asks only to press Enter to continue.
- escape: Escape backs out of something harmless without choosing anything.
- two: A numbered menu is open where option 2 keeps the current model or current behaviour, and pressing 2 selects it.

confidence: your overall confidence from 0 to 1 in the state answer.
```

Output schema: `{state: idle|working|blocked|exited, needs_human: bool,
needs_human_probability: number, safe_key: none|enter|escape|two,
confidence: number}`, all required, no extra fields.

### Two example screens

Easy (`cc_tool_running`, label: working / no human / none):

```
⏺ Bash(go test ./... 2>&1 | tail -40)
  ⎿  Running…

  ok    github.com/orlenko/aiq/internal/config   0.412s
  ok    github.com/orlenko/aiq/internal/fsutil   0.088s
* Baking… (48s · ↑ 3.1k tokens · esc to interrupt)
```

Harder (`cc_quoted_menu_in_transcript`, label: idle / no human / none; the
menu is quoted inside the agent's message, not open):

```
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
```

Every model, Jev included, got that one right. All 24 screens are in
`cases.py`.

## Where each model went wrong

Jev, 8 cases with at least one wrong answer:

| case | expected | Jev said |
|---|---|---|
| cc_idle_prompt | idle | blocked (p=0.92) |
| cc_idle_after_question | idle, human | blocked |
| cx_idle_asks_question | idle, human | blocked |
| cc_usage_limit | idle, human | blocked |
| cc_update_notice | blocked, enter | blocked, enter, but needs_human 0.56 |
| cx_rate_limit_menu | blocked, key two | blocked, key none (conf 0.77) |
| cx_rate_limit_menu_keep_selected | blocked, key two | blocked, key none |
| cc_streaming_text | working, none | working, key two |

Two patterns. Jev reads Claude Code's bordered input box `╭─╮ │ > │ ╰─╯` as
a dialog and says "blocked" instead of "idle"; four of the eight misses are
that. And it will not name "2" as a safe key on the Codex rate-limit menu,
the one case the regex in `internal/provider/codex/codex.go` exists for. Its
confidence on the misses (0.80–0.89) overlaps its confidence on hits (0.46–1.0),
so a threshold alone does not separate them; a rephrased `idle` criterion that
describes the box, or a screen preprocessed to strip box-drawing, is the
next thing to try.

Haiku, 6: called `shell_only` idle instead of exited, `cx_working` safe_key
escape, and named "2" on an edit-permission prompt (the dangerous direction).

Sonnet, 2; Opus, 1; Fable, 1: all on `cc_usage_limit` (a usage-limit error
followed by an empty prompt: they said idle with `needs_human` false, the label
says a human is needed). Sonnet also gave `needs_human` 0.75 on a crash trace.
That label is arguable; the three big models agree with each other.

## Cost caveats

- Claude costs are `total_cost_usd` from `claude -p` at list price. Each call
  carried ~3k tokens of residual Claude Code context plus a ~1k-token Haiku
  sidecar call (~$0.001) that `claude -p` makes on every run, and most calls
  wrote the prompt cache (1-hour TTL) rather than reading it. A warm-cache
  probe on Opus cost $0.0068 instead of the run's median $0.032. A bare API
  call with a cached system prompt would land at roughly $0.005 (Haiku,
  thinking on) to $0.007 (Opus). Jev stays 100–200× cheaper even against that.
- Haiku's cost and 11 s latency are thinking: Claude Code turned thinking on
  (744 thinking tokens per call). Without it Haiku would be ~$0.001 and faster.
- Jev bills input only, $0.042 per million tokens.
- One run each; no variance measured. 24 cases; one case is 4 points.

## What this means for aiq

The supervisor's two open problems are "is this hookless session idle or
mid-turn" and "is that menu open, and what dismisses it". On this set Jev is
reliable on working vs not-working (the `cc_streaming_text` slip was on
`safe_key`, not `state`) and on `needs_human`; it is not yet reliable on
idle vs blocked, and it declines to authorise the rate-limit keypress.

Recommendation: use Jev for the cheap, per-tick judgment ("is anything
happening; does a person need to look"), keep the regex for the one keypress
it already handles, and gate any new unattended keypress on Jev agreeing with
a code check. Before that, replace the synthetic screens with real
`tmux capture-pane -p` output from live sessions and rerun; `bench.py` caches
results per model, so only the new cases cost anything.
