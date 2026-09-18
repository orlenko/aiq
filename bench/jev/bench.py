#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["anthropic>=1.6.0"]
# ///
"""Pane-state benchmark: TypeSafe Jev vs Claude models.

Every model sees the same screen and the same three questions and is
scored against the labels in cases.py. Results are cached per model in
results/<model>.json so a rerun only calls models it has not seen.

  uv run bench.py                 # run every model that has credentials
  uv run bench.py jev haiku       # run a subset
  uv run bench.py --report        # print the matrix from cached results

Keys: TYPESAFE_API_KEY (also read from ../../.env). Claude models use the
API when ANTHROPIC_API_KEY is set, otherwise `claude -p --bare` on the
Claude Code subscription.
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from cases import CASES  # noqa: E402

RESULTS = HERE / "results"

# Prices in dollars per million tokens: (input, output). Jev bills input only.
PRICES = {
    "jev": (0.042, 0.0),
    "haiku": (1.0, 5.0),
    "sonnet": (2.0, 10.0),
    "opus": (5.0, 25.0),
    "fable": (10.0, 50.0),
}
CLAUDE_IDS = {
    "haiku": "claude-haiku-4-5",
    "sonnet": "claude-sonnet-5",
    "opus": "claude-opus-5",
    "fable": "claude-fable-5-1",
}

STATE_CRITERIA = {
    "idle": "The agent finished its turn and is waiting at an empty input prompt. "
            "The last agent message may ask the user something, but nothing modal is open.",
    "working": "The agent is mid-turn: thinking, streaming a reply, running a tool, "
               "compacting, or loading. A spinner or 'esc to interrupt' is typical.",
    "blocked": "A modal prompt, menu, dialog, or sign-in screen is open and waits for a "
               "keypress or answer before the agent can continue.",
    "exited": "The agent process has ended: a shell prompt, goodbye message, or crash "
              "trace is the last thing on screen.",
}
SAFE_KEY_CRITERIA = {
    "none": "No key should be sent unattended: nothing is open, or what is open needs a "
            "person's decision (permissions, trust, sign-in, a question to the user).",
    "enter": "An informational notice such as a release note asks only to press Enter to continue.",
    "escape": "Escape backs out of something harmless without choosing anything.",
    "two": "A numbered menu is open where option 2 keeps the current model or current "
           "behaviour, and pressing 2 selects it.",
}
QUESTIONS = {
    "state": {
        "type": "choice",
        "instructions": "The state is the last lines of a terminal pane running a coding "
                        "agent (Claude Code or Codex) inside tmux. Ignore anything that is "
                        "quoted inside an agent's message; judge only what is live on screen "
                        "now. What is the session doing?",
        "criteria": STATE_CRITERIA,
    },
    "needs_human": {
        "type": "noul",
        "instructions": "Does a person need to answer, approve, sign in, or decide something "
                        "before this coding-agent session can make progress on its own? A "
                        "session that is working, or idle with nothing asked, does not.",
        "criteria": {
            "true": "A permission or trust dialog, a sign-in screen, a question the agent "
                    "asked the user, or a usage limit that only the user can resolve.",
            "false": "The agent is working, waiting idle with nothing to answer, has exited "
                     "cleanly, or shows a notice that an automated supervisor may dismiss.",
        },
    },
    "safe_key": {
        "type": "choice",
        "instructions": "An unattended supervisor may send at most one keypress to clear "
                        "the screen without changing anything that matters. Which key, if any?",
        "criteria": SAFE_KEY_CRITERIA,
    },
}

CLAUDE_SYSTEM = (
    "You judge the last lines of a terminal pane running a coding agent (Claude Code or "
    "Codex) inside tmux, captured by an unattended supervisor. Ignore anything quoted "
    "inside an agent's message; judge only what is live on screen now.\n\n"
    "state: what the session is doing:\n"
    + "\n".join(f"- {k}: {v}" for k, v in STATE_CRITERIA.items())
    + "\n\nneeds_human: does a person need to answer, approve, sign in, or decide something "
    "before the session can make progress on its own? Working, idle with nothing asked, "
    "exited, or a notice a supervisor may dismiss all mean false. Also give "
    "needs_human_probability, your probability from 0 to 1 that the answer is yes.\n\n"
    "safe_key: the one key the supervisor may send unattended to clear the screen without "
    "changing anything that matters:\n"
    + "\n".join(f"- {k}: {v}" for k, v in SAFE_KEY_CRITERIA.items())
    + "\n\nconfidence: your overall confidence from 0 to 1 in the state answer."
)
CLAUDE_SCHEMA = {
    "type": "object",
    "properties": {
        "state": {"type": "string", "enum": list(STATE_CRITERIA)},
        "needs_human": {"type": "boolean"},
        "needs_human_probability": {"type": "number"},
        "safe_key": {"type": "string", "enum": list(SAFE_KEY_CRITERIA)},
        "confidence": {"type": "number"},
    },
    "required": ["state", "needs_human", "needs_human_probability", "safe_key", "confidence"],
    "additionalProperties": False,
}


def load_env():
    env = HERE.parent.parent / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip())


# ---- Jev ------------------------------------------------------------------

def ask_jev(case):
    body = json.dumps({"state": case["screen"], "model": "jev-latest",
                       "questions": QUESTIONS}).encode()
    req = urllib.request.Request(
        "https://api.typesafe.ai/v1/systemone", data=body, method="POST",
        headers={"Authorization": f"Bearer {os.environ['TYPESAFE_API_KEY']}",
                 "Content-Type": "application/json"})
    for attempt in range(5):
        t0 = time.perf_counter()
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                data = json.load(r)
            break
        except urllib.error.HTTPError as e:
            if e.code in (429, 529) and attempt < 4:
                time.sleep(2 ** attempt)
                continue
            raise RuntimeError(f"jev {e.code}: {e.read()[:300]!r}")
    dt = time.perf_counter() - t0
    a = data["answers"]
    return {
        "state": a["state"]["choice"],
        "state_conf": a["state"]["confidence"],
        "state_probs": a["state"]["probabilities"],
        "needs_human_p": a["needs_human"]["noul"],
        "needs_human": a["needs_human"]["noul"] >= 0.5,
        "safe_key": a["safe_key"]["choice"],
        "safe_key_conf": a["safe_key"]["confidence"],
        "in_tokens": data["usage"]["input_tokens"],
        "out_tokens": data["usage"].get("output_tokens", 0),
        "latency": dt,
        "model": data["model"],
    }


# ---- Claude ---------------------------------------------------------------

def make_ask_claude(short):
    import anthropic
    client = anthropic.Anthropic()
    model = CLAUDE_IDS[short]

    def ask(case):
        t0 = time.perf_counter()
        resp = client.messages.create(
            model=model,
            max_tokens=8000,
            system=CLAUDE_SYSTEM,
            messages=[{"role": "user", "content": "Screen:\n\n" + case["screen"]}],
            output_config={"format": {"type": "json_schema", "schema": CLAUDE_SCHEMA}},
        )
        dt = time.perf_counter() - t0
        if resp.stop_reason == "refusal":
            raise RuntimeError(f"{model} refused: {resp.stop_details}")
        text = "".join(b.text for b in resp.content if b.type == "text")
        out = json.loads(text)
        return {
            "state": out["state"],
            "state_conf": out["confidence"],
            "needs_human_p": out["needs_human_probability"],
            "needs_human": out["needs_human"],
            "safe_key": out["safe_key"],
            "in_tokens": resp.usage.input_tokens,
            "out_tokens": resp.usage.output_tokens,
            "latency": dt,
            "model": resp.model,
        }
    return ask


def make_ask_claude_p(short):
    """Same question through `claude -p` on the Claude Code subscription.

    --tools "" and --system-prompt strip Claude Code's own system prompt and
    tool list, which would otherwise add ~25k input tokens per call. (--bare
    would also skip the keychain read that holds the login.) Cost comes from
    the CLI's total_cost_usd at list price.
    """
    import subprocess
    model = CLAUDE_IDS[short]

    def ask(case):
        cmd = ["claude", "-p", "--tools", "", "--no-session-persistence",
               "--disable-slash-commands", "--strict-mcp-config",
               "--model", model, "--output-format", "json",
               "--system-prompt", CLAUDE_SYSTEM,
               "--json-schema", json.dumps(CLAUDE_SCHEMA),
               "Screen:\n\n" + case["screen"]]
        t0 = time.perf_counter()
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
        dt = time.perf_counter() - t0
        if proc.returncode != 0:
            raise RuntimeError(f"claude -p exit {proc.returncode}: {proc.stderr[:300]}")
        data = json.loads(proc.stdout)
        if data.get("is_error") or "structured_output" not in data:
            raise RuntimeError(f"claude -p: {data.get('subtype')} {data.get('result', '')[:300]}")
        out = data["structured_output"]
        u = data["usage"]
        return {
            "state": out["state"],
            "state_conf": out["confidence"],
            "needs_human_p": out["needs_human_probability"],
            "needs_human": out["needs_human"],
            "safe_key": out["safe_key"],
            "in_tokens": u["input_tokens"] + u.get("cache_read_input_tokens", 0)
                         + u.get("cache_creation_input_tokens", 0),
            "out_tokens": u["output_tokens"],
            "thinking_tokens": u.get("output_tokens_details", {}).get("thinking_tokens", 0),
            "cost": data["total_cost_usd"],
            "latency": data["duration_api_ms"] / 1000,
            "wall": dt,
            # modelUsage also lists a small Haiku sidecar call (~1k tokens)
            # that claude -p makes on every run; name the model we asked for.
            "model": next((k for k in data.get("modelUsage", {}) if k.startswith(model)), "?"),
        }
    return ask


# ---- Runner ---------------------------------------------------------------

def run(short, ask, workers):
    RESULTS.mkdir(exist_ok=True)
    out = RESULTS / f"{short}.json"
    done = json.loads(out.read_text()) if out.exists() else {}
    todo = [c for c in CASES if "error" in done.get(c["id"], {"error": 1})]
    if not todo:
        print(f"{short}: cached")
        return
    print(f"{short}: {len(todo)} cases", flush=True)
    with ThreadPoolExecutor(workers) as ex:
        for case, res in zip(todo, ex.map(lambda c: safe(ask, c), todo)):
            done[case["id"]] = res
    out.write_text(json.dumps(done, indent=1, sort_keys=True))


def safe(ask, case):
    try:
        return ask(case)
    except Exception as e:  # keep the row; the report shows it as an error
        return {"error": str(e)[:300]}


def report():
    rows = []
    for short in PRICES:
        p = RESULTS / f"{short}.json"
        if not p.exists():
            continue
        res = json.loads(p.read_text())
        n = state_ok = human_ok = key_ok = errs = 0
        cost = lat = brier = 0.0
        misses = []
        for c in CASES:
            r = res.get(c["id"])
            if not r:
                continue
            n += 1
            if "error" in r:
                errs += 1
                continue
            s = r["state"] == c["state"]
            h = r["needs_human"] == c["needs_human"]
            k = r["safe_key"] == c["safe_key"]
            state_ok += s
            human_ok += h
            key_ok += k
            brier += (r["needs_human_p"] - float(c["needs_human"])) ** 2
            pin, pout = PRICES[short]
            cost += r.get("cost", r["in_tokens"] * pin / 1e6 + r["out_tokens"] * pout / 1e6)
            lat += r["latency"]
            if not (s and h and k):
                misses.append((c["id"], r["state"], r["needs_human_p"], r["safe_key"]))
        ok = n - errs
        rows.append((short, n, errs, state_ok, human_ok, key_ok, brier / max(ok, 1),
                     cost, lat / max(ok, 1), misses))
    print(f"{'model':7} {'n':>3} {'err':>3} {'state':>6} {'human':>6} {'key':>6} "
          f"{'brier':>6} {'$ total':>8} {'$/1k':>7} {'lat s':>6}")
    for short, n, errs, s, h, k, brier, cost, lat, _ in rows:
        ok = max(n - errs, 1)
        print(f"{short:7} {n:3d} {errs:3d} {s/ok:6.0%} {h/ok:6.0%} {k/ok:6.0%} "
              f"{brier:6.3f} {cost:8.4f} {cost/ok*1000:7.2f} {lat:6.2f}")
    print("\nmisses (case: state, p(needs_human), safe_key):")
    for short, *_, misses in rows:
        for cid, st, p, key in misses:
            print(f"  {short:7} {cid:34} {st:8} {p:4.2f} {key}")


def main(argv):
    load_env()
    if "--report" in argv:
        report()
        return
    want = [a for a in argv if not a.startswith("-")] or list(PRICES)
    for short in want:
        if short == "jev":
            if not os.environ.get("TYPESAFE_API_KEY"):
                print("jev: TYPESAFE_API_KEY not set, skipped")
                continue
            run(short, ask_jev, workers=4)
        elif os.environ.get("ANTHROPIC_API_KEY"):
            run(short, make_ask_claude(short), workers=4)
        else:
            # aiq's claude shim caps concurrent workers per account at 3.
            run(short, make_ask_claude_p(short), workers=2)
    report()


if __name__ == "__main__":
    main(sys.argv[1:])
