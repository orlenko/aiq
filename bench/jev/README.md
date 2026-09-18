# Pane-state benchmark: Jev vs Claude

Can a model tell, from the last lines of a tmux pane, whether a coding-agent
session is idle, working, blocked on a prompt, or gone, and whether a human is
needed? That is the judgment aiq's long-run supervisor makes today with hook
timestamps and one regex (`internal/longrun/longrun.go`,
`internal/provider/codex/codex.go`).

`cases.py` holds 24 hand-written pane tails with labels. `bench.py` sends each
one to TypeSafe Jev (`jev-latest`) and to Claude Haiku 4.5, Sonnet 5, Opus 5
and Fable 5.1 with the same three questions, then prints accuracy per
question, Brier score for the `needs_human` probability, cost, and latency.

```sh
cd bench/jev
uv run bench.py            # all models with credentials; results cached in results/
uv run bench.py jev opus   # subset
uv run bench.py --report   # matrix from cache
```

Keys: `TYPESAFE_API_KEY` (also read from the repo's `.env`) and
`ANTHROPIC_API_KEY`. Claude models run at their defaults (adaptive thinking
where the model has it) with a JSON-schema output; output tokens include
thinking, which is what the cost column charges.

The cases are synthetic. Before trusting a number, swap in real captures from
`tmux capture-pane -p` on live sessions.
