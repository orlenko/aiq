# Jev as a send-time check on orchestra messages

Counts and rates only; messages and labels stay in the gitignored data/. Jev saw each body with its header block removed.

## Corpus and cost

- 1221 distinct real messages from local Claude and Codex transcripts (624 sent here, 597 received here); 130 were rejected by the header parser.
- 1221 Jev calls, 1647522 input tokens, about $0.069; latency p50 0.42 s, p95 0.57 s.

## Jev against the sender's own headers (all parsed messages)

| check | disagree |
|---|---|
| act | 657/1091 (60%) |
| needs_reply | 368/1091 (34%) |
| blocked | 240/1091 (22%) |
| done without evidence | 8/97 (8%) |
| not worth sending | 15/1091 (1%) |

Most common act swaps (declared → Jev): tell→status 229, tell→assign 135, done→status 69, ask→assign 49, ask→status 39, tell→dissent 31, tell→done 16, block→status 15

## Against labels

160 labelled messages (sample: half where Jev and the headers disagree, half where they agree).

| question | Jev right | headers right | when they disagree: Jev right |
|---|---|---|---|
| act | 88/131 (67%) | 104/131 (79%) | 2/23 (9%) |
| needs_reply | 114/131 (87%) | 107/131 (82%) | 20/33 (61%) |
| blocked | 104/131 (79%) | 119/131 (91%) | 11/37 (30%) |
| done_evidence | 104/160 (65%) | – | – |
| worth_sending | 149/160 (93%) | – | – |

Calibration of the yes/no questions (labelled): share of labels true, by Jev probability band.

- needs_reply: <0.2 0/38 (0%), 0.2–0.8 7/49 (14%), ≥0.8 64/73 (88%)
- blocked: <0.2 0/89 (0%), 0.2–0.8 7/59 (12%), ≥0.8 5/12 (42%)
- done_evidence: <0.2 0/32 (0%), 0.2–0.8 4/53 (8%), ≥0.8 42/75 (56%)
- worth_sending: <0.2 0/0 (n/a), 0.2–0.8 41/42 (98%), ≥0.8 118/118 (100%)
## The one rule that works

Warn at send time when the header says `NEED none` but Jev gives
`needs_reply` p ≥ 0.9:

- Labelled precision: 7/8 at p ≥ 0.9, 3/3 at p ≥ 0.95, and 12/18 at p ≥ 0.8.
- On the whole corpus it would fire on 131 of 1091 parsed messages (12%).
- In the other direction (NEED set, Jev p < 0.1), it never fired on a labelled message.

This one matters because `reply_required` depends only on `NEED`. It ranks the
inbox and the Stop-hook nudge, so a message that asks for something under
`NEED none` sorts below replies and nobody chases it.

## What does not work

- **act**: Jev scores 67% against the labels and the headers score 79%. Where the
  two disagree, Jev is right 2 times out of 23. The taxonomy is fuzzy even for the
  labeller, who marked act unsure on 49 of 160 messages. Leave `ACT` to the sender.
- **blocked**: Jev is below the headers (79% vs 91%). Only 42% of its p ≥ 0.8
  answers are true.
- **done_evidence**: only 56% of its p ≥ 0.8 answers are true. It counts any sha
  or path as evidence.
- **worth_sending**: 159 of 160 labelled messages were worth sending, so there is
  nothing here to measure.

## Caveats

- An Opus 5 labeller, blind to the headers and to Jev, produced the labels. A
  person did not. The sample is stratified: half are messages where Jev and
  the headers disagree. So the per-question accuracies are not population
  rates. The precision of a rule within its own cell carries over.
- The corpus is one machine's transcripts over about two weeks, from a few
  conductors and players.
