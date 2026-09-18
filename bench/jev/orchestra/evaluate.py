#!/usr/bin/env python3
"""Score agent-orchestra's shadow Jev judge against hand labels and the timers.

Input is what agent-orchestra 0.2.3+ writes with AGENT_ORCHESTRA_JEV_SHADOW=1:
`<runtime>/<member>.jev.jsonl` rows and `<runtime>/<member>.jev-tails/<sha>.txt`.
Tails and labels hold real session content. They stay in the runtime dir or in
the gitignored data/ here; REPORT.md gets counts and rates only.

  ./evaluate.py pick [-n 30]     # write data/to_label.jsonl: tails to label, no Jev answers
  ./evaluate.py label            # label them one by one into data/labels.jsonl
  ./evaluate.py report [--write] # print the report; --write saves REPORT.md

Labels: {"sha", "activity", "needs_human", "reported_recently"}, where
activity is working|waiting_for_input|blocked_on_prompt|ended and the other two are bools.

Runtime dir: $AGENT_ORCHESTRA_HOME/runtime, else $XDG_STATE_HOME/agent-orchestra/runtime,
else ~/.local/state/agent-orchestra/runtime. Override with --runtime.
"""

import argparse
import json
import os
import random
import statistics
import sys
from collections import Counter, defaultdict
from pathlib import Path

HERE = Path(__file__).resolve().parent
DATA = HERE / "data"
ACTIVITIES = ("working", "waiting_for_input", "blocked_on_prompt", "ended")
JEV_PRICE_PER_MTOK = 0.042
WORKING_P = 0.9
NEEDS_HUMAN_P = 0.8


def default_runtime() -> Path:
    if os.environ.get("AGENT_ORCHESTRA_HOME"):
        return Path(os.environ["AGENT_ORCHESTRA_HOME"]) / "runtime"
    base = Path(os.environ.get("XDG_STATE_HOME") or "~/.local/state").expanduser()
    return base / "agent-orchestra" / "runtime"


def load_rows(runtime: Path) -> list[dict]:
    rows = []
    for path in sorted(runtime.glob("*.jev.jsonl*")):
        for line in path.read_text(encoding="utf-8").splitlines():
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    rows.sort(key=lambda r: (r.get("member_id", ""), r.get("ts", 0)))
    return rows


def tail_path(runtime: Path, row: dict) -> Path:
    return runtime / f"{row['member_id']}.jev-tails" / f"{row['sha']}.txt"


def load_labels() -> dict[str, dict]:
    path = DATA / "labels.jsonl"
    if not path.exists():
        return {}
    labels = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            label = json.loads(line)
            labels[label["sha"]] = label
    return labels


def working_p(row: dict) -> float:
    probs = (row.get("jev") or {}).get("activity_probs") or {}
    return float(probs.get("working") or 0.0)


# ---- pick / label -------------------------------------------------------------


def cmd_pick(args, runtime: Path) -> None:
    """Distinct tails, stratified so rare cases are not drowned by idle ones."""
    rows = [r for r in load_rows(runtime) if r.get("sha") and r.get("jev") and tail_path(runtime, r).exists()]
    labelled = set(load_labels())
    by_sha = {}
    for row in rows:
        if row["sha"] not in labelled:
            by_sha.setdefault(row["sha"], row)
    strata = defaultdict(list)
    for row in by_sha.values():
        key = (row["jev"]["activity"], bool(row.get("open_tool_call")),
               row["jev"].get("needs_human_p", 0) > 0.5, (row.get("timer") or {}).get("on_me"))
        strata[key].append(row)
    rng = random.Random(args.seed)
    for bucket in strata.values():
        rng.shuffle(bucket)
    picked = []
    while len(picked) < args.n and any(strata.values()):
        for key in list(strata):
            if strata[key] and len(picked) < args.n:
                picked.append(strata[key].pop())
    DATA.mkdir(exist_ok=True)
    with (DATA / "to_label.jsonl").open("w", encoding="utf-8") as out:
        for row in picked:
            out.write(json.dumps({"sha": row["sha"], "tail": str(tail_path(runtime, row)),
                                  "age_seconds": row.get("age_seconds")}) + "\n")
    print(f"{len(picked)} tails from {len(by_sha)} unlabelled distinct -> data/to_label.jsonl")


def cmd_label(args, runtime: Path) -> None:
    todo_path = DATA / "to_label.jsonl"
    if not todo_path.exists():
        sys.exit("run `pick` first")
    labels = load_labels()
    todo = [json.loads(l) for l in todo_path.read_text().splitlines() if l.strip()]
    todo = [t for t in todo if t["sha"] not in labels]
    keys = {a[0]: a for a in ACTIVITIES}
    with (DATA / "labels.jsonl").open("a", encoding="utf-8") as out:
        for i, item in enumerate(todo, 1):
            print("\n" + "=" * 78 + f"\n[{i}/{len(todo)}] {item['sha'][:12]}  "
                  f"(file age at sample: {item.get('age_seconds')} s)\n" + "=" * 78)
            print(Path(item["tail"]).read_text(encoding="utf-8"))
            answer = input("\nactivity [w]orking [a]waiting [b]locked [e]nded, [s]kip, [q]uit: ").strip().lower()
            if answer in {"q", "quit"}:
                break
            if answer not in keys or answer == "s":
                continue
            needs = input("needs_human? [y/N]: ").strip().lower() == "y"
            reported = input("reported_recently? [y/N]: ").strip().lower() == "y"
            out.write(json.dumps({"sha": item["sha"], "activity": keys[answer],
                                  "needs_human": needs, "reported_recently": reported}) + "\n")
            out.flush()


# ---- report -----------------------------------------------------------------


def pct(num: int, den: int) -> str:
    return f"{num}/{den} ({100 * num / den:.0f}%)" if den else "0/0 (n/a)"


def accuracy_section(rows: list[dict], labels: dict[str, dict]) -> list[str]:
    seen, joined = set(), []
    for row in rows:
        if row.get("jev") and row.get("sha") in labels and row["sha"] not in seen:
            seen.add(row["sha"])
            joined.append((row, labels[row["sha"]]))
    lines = ["## (a) Accuracy against hand labels", "",
             f"{len(joined)} labelled distinct tails.", ""]
    if not joined:
        return lines + ["No labels yet: run `pick`, then `label`.", ""]
    lines += ["| slice | n | activity | needs_human (p≥0.5) | reported_recently (p≥0.5) |",
              "|---|---|---|---|---|"]
    slices = {
        "all": joined,
        "newest event is an open tool call": [j for j in joined if j[0].get("open_tool_call")],
        "everything else": [j for j in joined if not j[0].get("open_tool_call")],
    }
    for name, items in slices.items():
        n = len(items)
        act = sum(r["jev"]["activity"] == l["activity"] for r, l in items)
        nh = sum((r["jev"]["needs_human_p"] >= 0.5) == l["needs_human"] for r, l in items)
        rr_items = [(r, l) for r, l in items if "reported_recently" in l]
        rr = sum((r["jev"]["reported_recently_p"] >= 0.5) == l["reported_recently"] for r, l in rr_items)
        lines.append(f"| {name} | {n} | {pct(act, n)} | {pct(nh, n)} | {pct(rr, len(rr_items))} |")
    confusion = Counter((l["activity"], r["jev"]["activity"]) for r, l in joined)
    lines += ["", "Activity confusion (label → Jev):", ""]
    lines += [f"- {a} → {b}: {c}" for (a, b), c in sorted(confusion.items()) if a != b] or ["- none"]
    sends = [(r, l) for r, l in joined if "sends_in_tail" in r]
    if sends:
        agree = sum(bool(r["sends_in_tail"]) == ((r["jev"]["reported_recently_p"] >= 0.5)) for r, _ in sends)
        lines += ["", "reported_recently against local sent records inside the tail window: "
                  + pct(agree, len(sends))]
    return lines + [""]


def skip_section(rows: list[dict], labels: dict[str, dict]) -> list[str]:
    stale = [r for r in rows if r.get("jev") and (r.get("timer") or {}).get("on_me") == "stale"]
    skip = [r for r in stale if working_p(r) > WORKING_P]
    lines = ["## (b) Stale nags Jev would skip", "",
             f"Samples where the timer calls this member's task stale: {len(stale)}. "
             f"Jev says working with p>{WORKING_P}: {pct(len(skip), len(stale))}.",
             f"Distinct members affected: {len({r['member_id'] for r in skip})}."]
    checked = [(r, labels[r["sha"]]) for r in skip if r.get("sha") in labels]
    if checked:
        right = sum(l["activity"] == "working" for _, l in checked)
        lines.append(f"Of those with a hand label, actually working: {pct(right, len(checked))}.")
    return lines + [""]


def early_section(rows: list[dict], labels: dict[str, dict]) -> list[str]:
    """Runs of needs_human>0.8 that start while the timer is quiet, and how long until it spoke."""
    by_member = defaultdict(list)
    for row in rows:
        if row.get("jev"):
            by_member[row["member_id"]].append(row)
    runs = []
    for samples in by_member.values():
        i = 0
        while i < len(samples):
            row = samples[i]
            if row["jev"]["needs_human_p"] > NEEDS_HUMAN_P and not (row.get("timer") or {}).get("on_me"):
                start = row
                j = i
                while j < len(samples) and samples[j]["jev"]["needs_human_p"] > NEEDS_HUMAN_P:
                    j += 1
                flagged = next((s for s in samples[i:] if (s.get("timer") or {}).get("on_me")), None)
                runs.append({"start": start, "lead": (flagged["ts"] - start["ts"]) if flagged else None,
                             "length": samples[j - 1]["ts"] - start["ts"]})
                i = j
            else:
                i += 1
    leads = [r["lead"] for r in runs if r["lead"] is not None]
    lines = ["## (c) needs_human alerts before the timer", "",
             f"Runs of needs_human p>{NEEDS_HUMAN_P} that began while the timer said nothing: {len(runs)}.",
             f"Timer later flagged the member: {len(leads)}; never flagged in the log: {len(runs) - len(leads)}."]
    if leads:
        lines.append(f"Lead time when it did: median {statistics.median(leads) / 60:.0f} min, "
                     f"min {min(leads) / 60:.0f}, max {max(leads) / 60:.0f}.")
    checked = [labels[r["start"]["sha"]] for r in runs if r["start"].get("sha") in labels]
    if checked:
        lines.append(f"Run starts with a hand label that truly needed a human: "
                     f"{pct(sum(l['needs_human'] for l in checked), len(checked))}.")
    return lines + [""]


def ops_section(rows: list[dict]) -> list[str]:
    answered = [r for r in rows if r.get("jev")]
    fresh = [r for r in answered if not r.get("reused")]
    errors = Counter((r.get("error") or "").split(":")[0] for r in rows if r.get("error"))
    latency = sorted(r["jev"]["latency_ms"] for r in fresh if r["jev"].get("latency_ms") is not None)
    tokens = sum(r["jev"].get("input_tokens") or 0 for r in fresh)
    span = (max(r["ts"] for r in rows) - min(r["ts"] for r in rows)) / 3600 if rows else 0
    lines = ["## Volume and cost", "",
             f"- {len(rows)} samples over {span:.1f} h from {len({r['member_id'] for r in rows})} members; "
             f"{len(fresh)} Jev calls, {len(answered) - len(fresh)} reused an unchanged tail.",
             f"- Errors: {sum(errors.values())}" + (f" ({', '.join(f'{k}: {v}' for k, v in errors.most_common())})"
                                                   if errors else "") + ".",
             f"- Input tokens {tokens}, about ${tokens * JEV_PRICE_PER_MTOK / 1e6:.4f}."]
    if latency:
        lines.append(f"- Latency p50 {latency[len(latency) // 2]} ms, "
                     f"p95 {latency[min(len(latency) - 1, int(len(latency) * 0.95))]} ms.")
    activity = Counter(r["jev"]["activity"] for r in answered)
    lines.append("- Jev activity mix: " + ", ".join(f"{a} {activity.get(a, 0)}" for a in ACTIVITIES) + ".")
    timer = Counter((r.get("timer") or {}).get("on_me") or "none" for r in answered)
    lines.append("- Timer view of the sampled member: " + ", ".join(f"{k} {v}" for k, v in timer.most_common()) + ".")
    return lines + [""]


def cmd_report(args, runtime: Path) -> None:
    rows = load_rows(runtime)
    labels = load_labels()
    body = ["# Jev shadow judge in agent-orchestra", "",
            "Counts and rates only. No transcript text appears here; tails and labels "
            "stay in the runtime dir and the gitignored data/.", ""]
    body += ops_section(rows) + accuracy_section(rows, labels) + skip_section(rows, labels) + early_section(rows, labels)
    text = "\n".join(body)
    print(text)
    if args.write:
        (HERE / "REPORT.md").write_text(text, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--runtime", type=Path, default=default_runtime())
    sub = parser.add_subparsers(dest="cmd", required=True)
    pick = sub.add_parser("pick")
    pick.add_argument("-n", type=int, default=30)
    pick.add_argument("--seed", type=int, default=7)
    sub.add_parser("label")
    report = sub.add_parser("report")
    report.add_argument("--write", action="store_true")
    args = parser.parse_args()
    {"pick": cmd_pick, "label": cmd_label, "report": cmd_report}[args.cmd](args, args.runtime)


if __name__ == "__main__":
    main()
