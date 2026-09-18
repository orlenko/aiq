#!/usr/bin/env python3
"""Can Jev check an orchestra message's headers against its body at send time?

Jev sees the body only, with the header block cut off, and answers what the
headers should have said. Comparing that with the headers the sender wrote
gives the disagreement rate for free. Hand labels on a sample say who is
right when they disagree.

  ./bench.py run            # ask Jev about every message in data/messages.jsonl (cached)
  ./bench.py sample [-n N]  # write data/to_label.jsonl, oversampling disagreements
  ./bench.py report [--write]

Needs TYPESAFE_API_KEY (also read from ../../../../.env). data/ is gitignored:
it holds real messages. REPORT.md gets counts and rates only.
"""

import argparse
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
DATA = HERE / "data"
ACTS = ("ask", "tell", "done", "block", "dissent", "assign", "status")

ACT_CRITERIA = {
    "ask": "A question the sender cannot continue without an answer to.",
    "tell": "Information for the recipient that needs no reply.",
    "done": "The sender finished the work it owned and gives the result.",
    "block": "The sender is stuck and says what stops it.",
    "dissent": "The sender says another member's claim or work does not hold.",
    "assign": "The sender hands the recipient a new piece of work to own.",
    "status": "A summary of where the sender's own work stands: accepted, started, "
              "progress, or state on request.",
}
PREAMBLE = ("The state is the body of a message one coding agent sends to other agents in a "
            "team called an orchestra; its header lines are removed. ")
QUESTIONS = {
    "act": {
        "type": "choice",
        "instructions": PREAMBLE + "Which kind of message is this? Judge the body's main "
                        "purpose, not phrases quoted from other messages.",
        "criteria": ACT_CRITERIA,
    },
    "needs_reply": {
        "type": "noul",
        "instructions": PREAMBLE + "Does the sender expect the recipient to answer or do "
                        "something in response?",
        "criteria": {
            "true": "It asks a question, requests a decision, evidence, or an action, assigns "
                    "work, or asks for an acknowledgement.",
            "false": "It informs, reports, or closes something, and nothing is asked of the "
                     "recipient.",
        },
    },
    "blocked": {
        "type": "noul",
        "instructions": PREAMBLE + "Does the sender say it cannot proceed with its own work "
                        "until someone else acts or answers?",
        "criteria": {
            "true": "It names something outside its control that stops it: a decision, "
                    "access, a failing dependency, another member's work.",
            "false": "It can continue, is finished, or is only informing.",
        },
    },
    "done_evidence": {
        "type": "noul",
        "instructions": PREAMBLE + "Does the body claim finished work and anchor that claim "
                        "in something checkable?",
        "criteria": {
            "true": "It says work is finished and names concrete evidence: a commit sha, a "
                    "test command or its result, a file path, a PR, or a URL.",
            "false": "It makes no finished-work claim, or claims one without anything a "
                      "reader could check.",
        },
    },
    "worth_sending": {
        "type": "noul",
        "instructions": PREAMBLE + "Would this message change what the recipient does next?",
        "criteria": {
            "true": "It carries a question, a request, a result, a correction, or information "
                    "the recipient needs to act on.",
            "false": "A bare acknowledgement, thanks, or progress narration nobody waits on.",
        },
    },
}


def load_env():
    env = HERE.parents[3] / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip())


def body_of(text: str) -> str:
    parts = text.split("\n\n", 1)
    return parts[1].strip() if len(parts) == 2 else ""


def load_messages() -> list[dict]:
    rows = []
    for line in (DATA / "messages.jsonl").read_text(encoding="utf-8").splitlines():
        row = json.loads(line)
        row["body"] = body_of(row["text"])
        if len(row["body"]) >= 20:
            rows.append(row)
    return rows


def ask_jev(state: str) -> dict:
    body = json.dumps({"state": state, "model": "jev-latest", "questions": QUESTIONS}).encode()
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
    a = data["answers"]
    return {
        "act": a["act"]["choice"], "act_probs": a["act"]["probabilities"],
        "act_conf": a["act"]["confidence"],
        **{f"{q}_p": a[q]["noul"] for q in ("needs_reply", "blocked", "done_evidence", "worth_sending")},
        "in_tokens": data["usage"]["input_tokens"], "latency": time.perf_counter() - t0,
        "model": data["model"],
    }


def load_jev() -> dict[str, dict]:
    path = DATA / "jev.jsonl"
    if not path.exists():
        return {}
    return {r["key"]: r for r in map(json.loads, path.read_text().splitlines())}


def cmd_run(args):
    load_env()
    done = load_jev()
    todo = [m for m in load_messages() if m["key"] not in done]
    print(f"{len(todo)} to ask, {len(done)} cached")
    errors = 0
    with (DATA / "jev.jsonl").open("a", encoding="utf-8") as out, ThreadPoolExecutor(8) as pool:
        futures = {pool.submit(ask_jev, m["body"]): m for m in todo}
        for i, future in enumerate(futures, 1):
            m = futures[future]
            try:
                out.write(json.dumps({"key": m["key"], **future.result()}) + "\n")
                out.flush()
            except Exception as exc:  # noqa: BLE001
                errors += 1
                print("error", m["key"], str(exc)[:120], file=sys.stderr)
            if i % 100 == 0:
                print(i)
    print(f"done, {errors} errors")


# ---- comparing Jev with the sender's headers ----------------------------------------


def declared(m: dict) -> dict:
    h = m.get("headers") or {}
    return {
        "act": h.get("act"),
        "needs_reply": (h.get("need") or "none") != "none" if h else None,
        "blocked": (h.get("act") == "block" or h.get("state") == "blocked") if h else None,
    }


def disagreements(m: dict, j: dict) -> list[str]:
    d = declared(m)
    out = []
    if d["act"] and j["act"] != d["act"]:
        out.append("act")
    if d["needs_reply"] is not None and (j["needs_reply_p"] >= 0.5) != d["needs_reply"]:
        out.append("needs_reply")
    if d["blocked"] is not None and (j["blocked_p"] >= 0.5) != d["blocked"]:
        out.append("blocked")
    if d["act"] == "done" and j["done_evidence_p"] < 0.5:
        out.append("done_without_evidence")
    if j["worth_sending_p"] < 0.5:
        out.append("not_worth_sending")
    return out


def cmd_sample(args):
    jev = load_jev()
    labelled = set()
    if (DATA / "labels.jsonl").exists():
        labelled = {json.loads(l)["key"] for l in (DATA / "labels.jsonl").read_text().splitlines() if l.strip()}
    rows = [m for m in load_messages() if m["key"] in jev and m["key"] not in labelled]
    rng = random.Random(args.seed)
    rng.shuffle(rows)
    flagged = [m for m in rows if disagreements(m, jev[m["key"]])]
    clean = [m for m in rows if not disagreements(m, jev[m["key"]])]
    # Half disagreements (who is right?), half agreements (are both wrong?).
    by_kind: dict[str, list] = {}
    for m in flagged:
        by_kind.setdefault(disagreements(m, jev[m["key"]])[0], []).append(m)
    picked, half = [], args.n // 2
    while len(picked) < half and any(by_kind.values()):
        for kind in list(by_kind):
            if by_kind[kind] and len(picked) < half:
                picked.append(by_kind[kind].pop())
    picked += clean[: args.n - len(picked)]
    rng.shuffle(picked)
    with (DATA / "to_label.jsonl").open("w", encoding="utf-8") as out:
        for m in picked:
            out.write(json.dumps({"key": m["key"], "body": m["body"]}, ensure_ascii=False) + "\n")
    print(f"{len(picked)} to label ({half} from {len(flagged)} flagged, rest from {len(clean)} clean)")


def pct(a, b):
    return f"{a}/{b} ({100 * a / b:.0f}%)" if b else "0/0 (n/a)"


def cmd_report(args):
    messages = {m["key"]: m for m in load_messages()}
    jev = {k: v for k, v in load_jev().items() if k in messages}
    labels = {}
    if (DATA / "labels.jsonl").exists():
        labels = {r["key"]: r for r in map(json.loads, (DATA / "labels.jsonl").read_text().splitlines()) if r}
    lines = ["# Jev as a send-time check on orchestra messages", "",
             "Counts and rates only; messages and labels stay in the gitignored data/. "
             "Jev saw each body with its header block removed.", ""]
    fresh = list(jev.values())
    tokens = sum(r["in_tokens"] for r in fresh)
    lat = sorted(r["latency"] for r in fresh)
    lines += ["## Corpus and cost", "",
              f"- {len(messages)} distinct real messages from local Claude and Codex transcripts "
              f"({Counter(m['source'] for m in messages.values())['sent']} sent here, "
              f"{Counter(m['source'] for m in messages.values())['received']} received here); "
              f"{sum(1 for m in messages.values() if m['parse_error'])} were rejected by the header parser.",
              f"- {len(jev)} Jev calls, {tokens} input tokens, about ${tokens * 0.042 / 1e6:.3f}; "
              + (f"latency p50 {lat[len(lat) // 2]:.2f} s, p95 {lat[int(len(lat) * .95)]:.2f} s." if lat else ""),
              ""]
    # Disagreement with headers, on the whole corpus.
    parsed = [(messages[k], j) for k, j in jev.items() if messages[k]["headers"]]
    lines += ["## Jev against the sender's own headers (all parsed messages)", "",
              "| check | disagree |", "|---|---|"]
    for kind in ("act", "needs_reply", "blocked"):
        n = sum(kind in disagreements(m, j) for m, j in parsed)
        lines.append(f"| {kind} | {pct(n, len(parsed))} |")
    done = [(m, j) for m, j in parsed if m["headers"]["act"] == "done"]
    lines.append(f"| done without evidence | {pct(sum(j['done_evidence_p'] < .5 for _, j in done), len(done))} |")
    lines.append(f"| not worth sending | {pct(sum(j['worth_sending_p'] < .5 for _, j in parsed), len(parsed))} |")
    conf = Counter((m["headers"]["act"], j["act"]) for m, j in parsed if m["headers"]["act"] != j["act"])
    lines += ["", "Most common act swaps (declared → Jev): "
              + ", ".join(f"{a}→{b} {c}" for (a, b), c in conf.most_common(8)), ""]
    # Labels.
    joined = [(messages[k], jev[k], labels[k]) for k in labels if k in jev and k in messages]
    lines += ["## Against labels", "", f"{len(joined)} labelled messages "
              "(sample: half where Jev and the headers disagree, half where they agree).", ""]
    if joined:
        lines += ["| question | Jev right | headers right | when they disagree: Jev right |",
                  "|---|---|---|---|"]
        for q in ("act", "needs_reply", "blocked"):
            items = [(m, j, l) for m, j, l in joined if q in l and declared(m)[q] is not None]
            jr = sum(_jev(j, q) == l[q] for m, j, l in items)
            hr = sum(declared(m)[q] == l[q] for m, j, l in items)
            dis = [(m, j, l) for m, j, l in items if _jev(j, q) != declared(m)[q]]
            lines.append(f"| {q} | {pct(jr, len(items))} | {pct(hr, len(items))} | "
                         f"{pct(sum(_jev(j, q) == l[q] for m, j, l in dis), len(dis))} |")
        for q in ("done_evidence", "worth_sending"):
            items = [(j, l) for m, j, l in joined if q in l]
            lines.append(f"| {q} | {pct(sum(_jev(j, q) == l[q] for j, l in items), len(items))} | – | – |")
        lines += ["", "Calibration of the yes/no questions (labelled): share of labels true, by Jev probability band.", ""]
        for q in ("needs_reply", "blocked", "done_evidence", "worth_sending"):
            bands = Counter()
            hits = Counter()
            for m, j, l in joined:
                if q not in l:
                    continue
                p = j[f"{q}_p"]
                band = "<0.2" if p < .2 else "0.2–0.8" if p < .8 else "≥0.8"
                bands[band] += 1
                hits[band] += bool(l[q])
            lines.append(f"- {q}: " + ", ".join(f"{b} {pct(hits[b], bands[b])}" for b in ("<0.2", "0.2–0.8", "≥0.8")))
        lines.append("")
    text = "\n".join(lines)
    print(text)
    if args.write:
        (HERE / "REPORT.md").write_text(text, encoding="utf-8")


def _jev(j: dict, q: str):
    return j["act"] if q == "act" else j[f"{q}_p"] >= 0.5


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("run")
    s = sub.add_parser("sample")
    s.add_argument("-n", type=int, default=160)
    s.add_argument("--seed", type=int, default=11)
    r = sub.add_parser("report")
    r.add_argument("--write", action="store_true")
    args = parser.parse_args()
    {"run": cmd_run, "sample": cmd_sample, "report": cmd_report}[args.cmd](args)


if __name__ == "__main__":
    main()
