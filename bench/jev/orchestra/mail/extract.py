#!/usr/bin/env python3
"""Collect real orchestra messages from local Claude and Codex transcripts.

The hub drops a body once every recipient has it, and a member's done records
keep headers only. The full text survives in the session transcripts: inbox
and hook output a receiver saw, and the `agent-orchestra send` heredocs a
sender ran. This pulls both, parses the headers with agent-orchestra's own
parser, dedupes by body, and writes data/messages.jsonl (gitignored: real
content).

  ./extract.py [--skills ~/personal/skills]
"""

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOTS = [Path("~/.claude/projects").expanduser(), Path("~/.codex/sessions").expanduser()]
HEREDOC = re.compile(r"agent-orchestra\S*\s+send\b[^\n]*?<<-?\s*['\"]?(\w+)['\"]?[^\n]*\n(.*?)\n\s*\1\b", re.S)
MARK = '"text": "ACT '


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for item in value.values():
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)


def message_dicts(value):
    if isinstance(value, dict):
        if isinstance(value.get("text"), str) and value["text"].startswith("ACT "):
            yield value
        for item in value.values():
            yield from message_dicts(item)
    elif isinstance(value, list):
        for item in value:
            yield from message_dicts(item)


def received(text: str):
    """Message objects embedded as JSON in a tool result or hook context."""
    decoder = json.JSONDecoder()
    seen_end = -1
    start = 0
    while True:
        hit = text.find(MARK, start)
        if hit < 0:
            return
        start = hit + 1
        if hit < seen_end:
            continue
        # Walk back to each enclosing '{' until one decodes to something holding the hit.
        brace = text.rfind("{", 0, hit)
        while brace >= 0:
            try:
                value, end = decoder.raw_decode(text, brace)
            except json.JSONDecodeError:
                brace = text.rfind("{", 0, brace)
                continue
            if end > hit:
                seen_end = end
                yield from message_dicts(value)
                break
            brace = text.rfind("{", 0, brace)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--skills", type=Path, default=Path("~/personal/skills").expanduser())
    args = parser.parse_args()
    sys.path.insert(0, str(args.skills / "plugins" / "agent-orchestra"))
    from agent_orchestra.protocol import parse_message

    out: dict[str, dict] = {}
    for root in ROOTS:
        for path in root.rglob("*.jsonl"):
            try:
                raw = path.read_text(encoding="utf-8", errors="replace")
            except OSError:
                continue
            if "ACT " not in raw:
                continue
            for line in raw.splitlines():
                if "ACT " not in line:
                    continue
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                for s in strings(row):
                    if "ACT " not in s:
                        continue
                    found = [("received", m.get("text"), m) for m in received(s)]
                    found += [("sent", m.group(2), {}) for m in HEREDOC.finditer(s)]
                    for source, body, meta in found:
                        if not body or not body.lstrip().startswith("ACT "):
                            continue
                        body = body.strip()
                        key = hashlib.sha256(body.encode()).hexdigest()[:16]
                        if key in out:
                            continue
                        try:
                            env = parse_message(body, ["conductor"])
                            headers = {"act": env.act, "need": env.need, "task": env.task,
                                       "re": env.re, "state": env.lifecycle}
                            error = None
                        except Exception as exc:  # noqa: BLE001
                            headers, error = {}, str(exc)[:200]
                        out[key] = {"key": key, "source": source, "id": meta.get("id"),
                                    "provider": "codex" if ".codex" in str(path) else "claude",
                                    "headers": headers, "parse_error": error, "text": body}
    data = HERE / "data"
    data.mkdir(exist_ok=True)
    with (data / "messages.jsonl").open("w", encoding="utf-8") as handle:
        for row in out.values():
            handle.write(json.dumps(row, ensure_ascii=False) + "\n")
    from collections import Counter
    print(f"{len(out)} distinct messages")
    print(Counter(r["source"] for r in out.values()))
    print(Counter(r["headers"].get("act") for r in out.values()))
    print(Counter(r["headers"].get("need", "?") != "none" for r in out.values()), "need != none")
    print(sum(1 for r in out.values() if r["parse_error"]), "parse errors")


if __name__ == "__main__":
    main()
